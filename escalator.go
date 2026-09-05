package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Escalator 周期性向 backend 询问需要升级的 incidents，并在 thread 里 @ 升级目标。
//
// 设计：
//   - 单 goroutine，30s 一拍；可由 ALERT_ESCALATOR_INTERVAL 调整。
//   - 每个 incident 的同一 level 升级是幂等的（backend mark_escalated 内部做了
//     "时间戳已存在则 no-op"），所以即便 forwarder 重启 / 多副本同时跑也不会
//     重复 @ 同一批人。
//   - backend 不可用时整段退避（10s → 30s → 60s → 5m），不阻塞主告警链路。
//   - 失败统计仅 log，不暴露 metrics（后续可加 prom counter）。
type Escalator struct {
	backend  *IncidentBackend
	feishu   feishuMessenger
	voice    voiceCaller // 可为 nil；nil 时只 @ 飞书不打电话
	interval time.Duration

	// voiceSeverities 限定哪些 severity 才会在 L2 升级时拨电话（已小写归一）。
	// nil/空 = 不限制（任何 severity 都打）；生产经 ALERT_VOICE_SEVERITIES 注入，
	// 默认只放行 p0/critical，避免低级别告警半夜打电话。
	voiceSeverities map[string]struct{}
	// voiceSeveritiesByService 按 service 覆盖电话 severity 策略（来自 SERVICE_VOICE_SEVERITIES）。
	// service 命中时用其专属 set；set 存在但为空 = 该 service 永不拨电话；未命中则回退 voiceSeverities。
	voiceSeveritiesByService map[string]map[string]struct{}
	// skipServices 命中则跳过 L1/L2 升级（不 thread @、不打电话），但仍 mark_escalated
	// 以免反复进入候选池。来源：ESCALATION_SKIP_SERVICES=ad-delivery,foo
	skipServices map[string]struct{}
	// dataServiceMatcher identifies alerts routed to the data group. Those
	// alerts have a dedicated L1 contact and keep L2 disabled.
	dataServiceMatcher          func(service string) bool
	dataEscalationMentionOpenID string

	stop     chan struct{}
	stopOnce sync.Once

	// 退避状态
	failureMu    sync.Mutex
	failureCount int
}

// feishuMessenger 抽象 forwarder 的飞书发送能力，便于单测 mock。
type feishuMessenger interface {
	replyFeishuMessage(ctx context.Context, messageID, text string) error
}

// voiceCaller 抽象阿里云语音通知能力，便于单测 mock。
// 真正实现是 *AliyunVoiceClient；nil 表示运维没配 ALIYUN_VOICE_* env，L2 只 @ 不打电话。
type voiceCaller interface {
	IsEnabled() bool
	SingleCallByTts(ctx context.Context, phone string, ttsParam map[string]string) (string, error)
}

func NewEscalator(backend *IncidentBackend, feishu feishuMessenger, voice voiceCaller, interval time.Duration, voiceSeverities map[string]struct{}, voiceSeveritiesByService map[string]map[string]struct{}, skipServices map[string]struct{}) *Escalator {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Escalator{
		backend:                  backend,
		feishu:                   feishu,
		voice:                    voice,
		interval:                 interval,
		voiceSeverities:          voiceSeverities,
		voiceSeveritiesByService: voiceSeveritiesByService,
		skipServices:             skipServices,
		stop:                     make(chan struct{}),
	}
}

// parseEscalationSkipServices 解析 "ad-delivery,foo;bar" 成小写 set。空串返回 nil。
func parseEscalationSkipServices(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == ' '
	})
	for _, f := range fields {
		svc := strings.ToLower(strings.TrimSpace(f))
		if svc != "" {
			out[svc] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (e *Escalator) shouldSkipService(service string) bool {
	if len(e.skipServices) == 0 {
		return false
	}
	_, ok := e.skipServices[strings.ToLower(strings.TrimSpace(service))]
	return ok
}

// parseServiceVoiceSeverities 解析 "svcA=p0,critical;svcB=none" 成 per-service set。
//   - 值为 none/off/-/空 → 该 service 的 set 为空 map（= 永不拨电话，但 key 存在以示"有覆盖"）
//   - 其余按逗号拆 severity，小写归一
//
// service 间用 ; 或换行分隔。整体空串返回 nil。
func parseServiceVoiceSeverities(raw string) map[string]map[string]struct{} {
	out := map[string]map[string]struct{}{}
	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' })
	for _, e := range entries {
		kv := strings.SplitN(e, "=", 2)
		if len(kv) != 2 {
			continue
		}
		svc := strings.ToLower(strings.TrimSpace(kv[0]))
		if svc == "" {
			continue
		}
		val := strings.ToLower(strings.TrimSpace(kv[1]))
		set := map[string]struct{}{}
		if val != "" && val != "none" && val != "off" && val != "-" {
			for _, sev := range strings.Split(val, ",") {
				if s := strings.TrimSpace(sev); s != "" {
					set[s] = struct{}{}
				}
			}
		}
		out[svc] = set // 即使空也保留 key：表示该 service 显式覆盖为"永不拨"
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseVoiceSeverities 把 "p0,critical" 这样的 CSV 解析成小写 set。
// 空串返回 nil —— 调用方按 nil=不限制处理。
func parseVoiceSeverities(csv string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(csv, ",") {
		s := strings.ToLower(strings.TrimSpace(part))
		if s != "" {
			out[s] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sortedSeverityKeys 把 severity set 排序成稳定切片，仅用于启动日志可读。
func sortedSeverityKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// voiceSeverityAllowed 判断该 (service, severity) 是否允许拨电话。
//   - service 在 voiceSeveritiesByService 里有覆盖 → 用其专属 set（空 set = 永不拨）
//   - 否则回退全局 voiceSeverities；全局 nil/空 = 不限制（返回 true）
func (e *Escalator) voiceSeverityAllowed(service, severity string) bool {
	set := e.voiceSeverities
	if e.voiceSeveritiesByService != nil {
		if ov, ok := e.voiceSeveritiesByService[strings.ToLower(strings.TrimSpace(service))]; ok {
			if len(ov) == 0 {
				return false // 显式覆盖为"永不拨"
			}
			set = ov
		}
	}
	if len(set) == 0 {
		return true
	}
	_, ok := set[strings.ToLower(strings.TrimSpace(severity))]
	return ok
}

// Start 启动循环；调用方负责 defer Close()。
//
// 不在 init 时立刻跑一次，避免和 main 启动期的其他初始化竞争。
func (e *Escalator) Start() {
	if e.backend == nil {
		log.Printf("alert escalator: disabled (no backend)")
		return
	}
	go e.loop()
	log.Printf("alert escalator: started interval=%s", e.interval)
}

func (e *Escalator) Close() {
	e.stopOnce.Do(func() { close(e.stop) })
}

func (e *Escalator) loop() {
	t := time.NewTimer(e.interval)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.tick()
			t.Reset(e.nextInterval())
		}
	}
}

// nextInterval 失败时退避；成功时回到基线。
func (e *Escalator) nextInterval() time.Duration {
	e.failureMu.Lock()
	defer e.failureMu.Unlock()
	switch {
	case e.failureCount == 0:
		return e.interval
	case e.failureCount == 1:
		return 10 * time.Second
	case e.failureCount == 2:
		return 30 * time.Second
	case e.failureCount == 3:
		return 60 * time.Second
	default:
		return 5 * time.Minute
	}
}

func (e *Escalator) recordFailure() {
	e.failureMu.Lock()
	defer e.failureMu.Unlock()
	e.failureCount++
}

func (e *Escalator) recordSuccess() {
	e.failureMu.Lock()
	defer e.failureMu.Unlock()
	e.failureCount = 0
}

func (e *Escalator) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reply, err := e.backend.ListEscalationCandidates(ctx)
	if err != nil {
		log.Printf("alert escalator: list candidates failed: %v", err)
		e.recordFailure()
		return
	}
	e.recordSuccess()
	for _, c := range reply.NeedL1 {
		e.escalate(ctx, c, 1)
	}
	for _, c := range reply.NeedL2 {
		e.escalate(ctx, c, 2)
	}
}

// escalate 在 incident 的 thread 里 @ 升级目标，然后告诉 backend "已升过"。
//
// 顺序很重要：先 @，再标记。如果 @ 失败下一轮还会重试；如果先标记后 @
// 失败，下一轮就不会再升级，等于真的丢了。
func (e *Escalator) escalate(ctx context.Context, c escalationCandidate, level int) {
	id := c.idAsInt64()
	if e.shouldSkipService(c.Service) {
		log.Printf("alert escalator: skip L%d incident=%d service=%q (ESCALATION_SKIP_SERVICES)",
			level, id, c.Service)
		_ = e.backend.MarkEscalated(ctx, id, int32(level))
		return
	}
	if e.dataServiceMatcher != nil && e.dataServiceMatcher(c.Service) {
		if level == 2 {
			log.Printf("alert escalator: data-group L2 disabled incident=%d service=%q", id, c.Service)
			return
		}
		if level == 1 && e.dataEscalationMentionOpenID != "" {
			c.MentionOpenIds = []string{e.dataEscalationMentionOpenID}
			c.MentionPhones = nil
		}
	}
	if c.FeishuMsgID == "" {
		log.Printf("alert escalator: skip incident=%d level=%d (no feishu_msg_id; was the incident posted before backend integration?)",
			id, level)
		// 仍然 mark_escalated，否则会反复进入候选列表打日志。
		_ = e.backend.MarkEscalated(ctx, id, int32(level))
		return
	}
	mentions := strings.Builder{}
	for _, openID := range c.MentionOpenIds {
		fmt.Fprintf(&mentions, "<at user_id=\"%s\"></at> ", openID)
	}
	if mentions.Len() == 0 {
		// fallback：服务池/leader 没配，只能群里喊话不带 @。
		mentions.WriteString("（升级目标未配置）")
	}
	var msg string
	switch level {
	case 1:
		msg = fmt.Sprintf("⚠️ L1 升级：%s 已超 5 分钟没人认领\n%s 请尽快接手 → %s",
			c.Alertname, mentions.String(), c.Service)
	case 2:
		msg = fmt.Sprintf("🚨 L2 升级：%s 已超 15 分钟没人接\n%s 请介入兜底",
			c.Alertname, mentions.String())
	}
	if err := e.feishu.replyFeishuMessage(ctx, c.FeishuMsgID, msg); err != nil {
		log.Printf("alert escalator: reply L%d failed incident=%d: %v", level, id, err)
		// 不 mark：下一轮重试。
		return
	}

	// 语音升级：backend 决定每个 level 的拨号目标，forwarder 只负责按白名单拨号。
	// 当前策略是 L1(5 分钟)拨当前告警负责人，L2(15 分钟)拨 leader 兜底。
	//
	// 不阻塞 mark_escalated：拨号失败只 log，下一轮 backend 已经标记对应 level 后就不会
	// 再进入候选池，避免一直循环打。
	if (level == 1 || level == 2) && e.voice != nil && e.voice.IsEnabled() && len(c.MentionPhones) > 0 {
		if e.voiceSeverityAllowed(c.Service, c.Severity) {
			e.dialVoice(ctx, c, level)
		} else {
			log.Printf("alert escalator: L%d skip voice call incident=%d service=%q severity=%q (not in voice allowlist)",
				level, id, c.Service, c.Severity)
		}
	}

	if err := e.backend.MarkEscalated(ctx, id, int32(level)); err != nil {
		// 升级消息已经发出，但标记失败 → 下一轮可能再 @ 一次。
		// 这是"宁可重复也不漏"的取舍；mark_escalated 自身是幂等的，重发的影响只是
		// 多一条 thread 消息而已。
		log.Printf("alert escalator: mark L%d failed incident=%d (will likely re-notify): %v", level, id, err)
	}
}

// dialVoice 给 candidate.MentionPhones 里所有号码拨打 TTS 语音通知。
//
// 设计：
//   - 每通拨号独立 ctx + 15s 超时，互不影响；
//   - 拨号失败只记 log（包含 RequestId/CallId），不重试；下一轮 escalator
//     如果 backend 还没 mark_escalated 自然会再拨一次。
//   - TtsParam 用 alertname + severity 两个占位，对应运维在阿里云控制台审核的
//     模板（如 "告警 ${alertname}，严重级别 ${severity}，请尽快处理"）。
func (e *Escalator) dialVoice(parentCtx context.Context, c escalationCandidate, level int) {
	id := c.idAsInt64()
	param := map[string]string{
		"alertname": truncateTTSParam(c.Alertname, 32),
		"severity":  truncateTTSParam(c.Severity, 8),
	}
	for _, phone := range c.MentionPhones {
		phone := phone
		ctx, cancel := context.WithTimeout(parentCtx, 15*time.Second)
		callID, err := e.voice.SingleCallByTts(ctx, phone, param)
		cancel()
		if err != nil {
			log.Printf("alert escalator: L%d voice call failed incident=%d phone=%s: %v",
				level, id, maskPhone(phone), err)
			continue
		}
		log.Printf("alert escalator: L%d voice call ok incident=%d phone=%s call_id=%s",
			level, id, maskPhone(phone), callID)
	}
}

// truncateTTSParam 把 TTS 模板入参截到 n 字符 —— 阿里云 TTS 模板对参数长度有上限，
// 超长会触发 isv.PARAMS_TOO_LONG 错。32 字符对 alertname 来说足够区分，超长部分
// 也不影响接电话人辨认告警；保险起见兜底。
func truncateTTSParam(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// maskPhone 在日志里把手机号掩码成 "861****8000" 形式，避免明文打日志。
// SLS 日志会被全员检索，号码不应该裸奔。
func maskPhone(s string) string {
	if len(s) <= 7 {
		return "***"
	}
	head := s[:3]
	tail := s[len(s)-4:]
	return head + strings.Repeat("*", len(s)-7) + tail
}
