package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IncidentBackend 与 matrix-backend 的 alert_incident gRPC-HTTP 网关交互。
//
// 设计：
//   - 全部接口都是同步的，但调用方必须给短超时；backend 故障绝不能阻塞告警链路。
//   - 出错时返回带类型的 error；上层（main.go handleGrafanaWebhook）应当 log 并降级，
//     用旧路径直接发卡（无 incident_id），保证告警永远能抵达飞书。
//   - 所有方法都接受 context；超时控制由 caller 注入。
type IncidentBackend struct {
	BaseURL    string        // 例如 http://matrix-backend.prod.svc:8000
	HTTPClient *http.Client  // 默认 http.DefaultClient + 3s 超时
	Timeout    time.Duration // 单次请求超时；<=0 时为 3s
}

// upsertIncidentRequest 与 backend api/matrix/v1/alert_incident.proto 对齐。
// 请求侧 protojson 既能识别 snake_case 也能识别 camelCase，但与响应侧保持一致用
// camelCase，避免再踩"protojson 默认输出 camelCase"那个坑。
type upsertIncidentRequest struct {
	Fingerprint  string `json:"fingerprint"`
	AlertName    string `json:"alertname"`
	Service      string `json:"service"`
	Env          string `json:"env"`
	Severity     string `json:"severity"`
	Summary      string `json:"summary"`
	Description  string `json:"description"`
	DashboardURL string `json:"dashboardUrl"`
	FeishuChatID string `json:"feishuChatId"`
	FiredAt      string `json:"firedAt"`
}

// incidentInfoDTO 与 backend 的 IncidentInfo proto 对齐。
//
// 注意 1：backend 使用 Kratos protojson，proto3 int64 默认序列化为 JSON string
// （避免 JS Number 精度丢失），所以 ID 字段用 json.Number 接收，能同时吃
// "id":"1" 和 "id":1 两种格式；取值用 idAsInt64() 帮助方法。
//
// 注意 2：protojson 默认输出 **lowerCamelCase**（如 assigneeOpenId），即便 proto
// 字段名是 snake_case (`assignee_open_id`)。所以多词字段这里必须写 camelCase，
// 否则收到的全是空字符串 —— 卡片不渲染"责任人"、escalator 找不到 feishuMsgId
// 就 skip 升级。一开始用 snake_case 翻车过，跟 backend 联调时务必看清这点。
type incidentInfoDTO struct {
	ID               json.Number `json:"id"`
	Fingerprint      string      `json:"fingerprint"`
	AlertName        string      `json:"alertname"`
	Service          string      `json:"service"`
	Env              string      `json:"env"`
	Severity         string      `json:"severity"`
	Status           int32       `json:"status"`
	Summary          string      `json:"summary"`
	Description      string      `json:"description"`
	DashboardURL     string      `json:"dashboardUrl"`
	FeishuMsgID      string      `json:"feishuMsgId"`
	FeishuChatID     string      `json:"feishuChatId"`
	AssigneeOpenID   string      `json:"assigneeOpenId"`
	AssignedAt       string      `json:"assignedAt"`
	AckedAt          string      `json:"ackedAt"`
	ResolvedAt       string      `json:"resolvedAt"`
	ResolverOpenID   string      `json:"resolverOpenId"`
	ResolutionNote   string      `json:"resolutionNote"`
	ReassignCount    int32       `json:"reassignCount"`
	FireCount        int32       `json:"fireCount"`
	FirstFiredAt     string      `json:"firstFiredAt"`
	LastFiredAt      string      `json:"lastFiredAt"`
	EscalatedL1At    string      `json:"escalatedL1At"`
	EscalatedL2At    string      `json:"escalatedL2At"`
	CopilotRootCause string      `json:"copilotRootCause"`
}

type upsertIncidentReply struct {
	Incident *incidentInfoDTO `json:"incident"`
	Dedup    bool             `json:"dedup"`
	// FireMentionOpenIds：admin 配了 fire-time 全员 @ 时由 backend 决定的列表（已去 OOO/去 assignee/去停用）。
	// forwarder 在卡片底部追加一行 <at user_id="ou_xx"></at>，给整队人一次过的可见性。
	// 字段名按 protojson 风格输出；与 *_open_ids 后缀对齐 backend 的 alert_incident.proto。
	FireMentionOpenIds []string `json:"fireMentionOpenIds"`
}

// idAsInt64 把 backend 返回的 incident.id 解成 int64；空字符串/"0"/失败都返回 0，
// 让 caller 用 `> 0` 来判断是否真的拿到 id。
func (d *incidentInfoDTO) idAsInt64() int64 {
	if d == nil {
		return 0
	}
	s := strings.TrimSpace(d.ID.String())
	if s == "" {
		return 0
	}
	if v, err := d.ID.Int64(); err == nil {
		return v
	}
	return 0
}

func (d *alertSilenceDTO) toAlertSilence() (alertSilence, error) {
	if d == nil {
		return alertSilence{}, errors.New("nil silence")
	}
	var createdAt time.Time
	if strings.TrimSpace(d.CreatedAt) != "" {
		if t, err := time.Parse(time.RFC3339, d.CreatedAt); err == nil {
			createdAt = t
		}
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(d.ExpiresAt))
	if err != nil {
		return alertSilence{}, err
	}
	return alertSilence{
		Fingerprint:    d.Fingerprint,
		AlertName:      d.AlertName,
		Service:        d.Service,
		Env:            d.Env,
		Severity:       d.Severity,
		OperatorOpenID: d.OperatorOpenID,
		Reason:         d.Reason,
		CreatedAt:      createdAt,
		ExpiresAt:      expiresAt,
	}, nil
}

type ackIncidentReply struct {
	Incident   *incidentInfoDTO `json:"incident"`
	Reassigned bool             `json:"reassigned"`
}

type resolveIncidentReply struct {
	Incident       *incidentInfoDTO `json:"incident"`
	ResolveMinutes json.Number      `json:"resolveMinutes"`
}

type resolveIncidentByFingerprintReply struct {
	Incident *incidentInfoDTO `json:"incident"`
	Resolved bool             `json:"resolved"`
}

type alertSilenceDTO struct {
	ID             json.Number `json:"id"`
	Fingerprint    string      `json:"fingerprint"`
	AlertName      string      `json:"alertname"`
	Service        string      `json:"service"`
	Env            string      `json:"env"`
	Severity       string      `json:"severity"`
	OperatorOpenID string      `json:"operatorOpenId"`
	Reason         string      `json:"reason"`
	ExpiresAt      string      `json:"expiresAt"`
	CreatedAt      string      `json:"createdAt"`
	UpdatedAt      string      `json:"updatedAt"`
}

type createAlertSilenceReply struct {
	Silence *alertSilenceDTO `json:"silence"`
}

type matchAlertSilenceReply struct {
	Active  bool             `json:"active"`
	Silence *alertSilenceDTO `json:"silence"`
}

// idAsInt64 与 incidentInfoDTO.idAsInt64 同义，但作用于升级候选。
func (c *escalationCandidate) idAsInt64() int64 {
	if c == nil {
		return 0
	}
	if v, err := c.ID.Int64(); err == nil {
		return v
	}
	return 0
}

type escalationCandidate struct {
	ID             json.Number `json:"id"`
	Alertname      string      `json:"alertname"`
	Service        string      `json:"service"`
	Severity       string      `json:"severity"`
	AssigneeOpenID string      `json:"assigneeOpenId"`
	FeishuMsgID    string      `json:"feishuMsgId"`
	FeishuChatID   string      `json:"feishuChatId"`
	AssignedAt     string      `json:"assignedAt"`
	MentionOpenIds []string    `json:"mentionOpenIds"`
	// MentionPhones：L2 升级时 backend 会从 leader 的 OncallMember.phone 字段填进来，
	// forwarder 收到后调阿里云语音通知拨号。空表示没人配电话，仅走飞书 @。
	MentionPhones []string `json:"mentionPhones"`
}

type listEscalationReply struct {
	NeedL1 []escalationCandidate `json:"needL1"`
	NeedL2 []escalationCandidate `json:"needL2"`
}

// ====== 计算 fingerprint，与 backend biz.ComputeFingerprint 保持一致 ======
//
// 必须和 matrix-backend/internal/biz/alert_incident.go 的算法一致，否则去重就废了。
// 故意不引入 backend 包做依赖，复制一遍稳得住。
var fingerprintLabelKeys = []string{
	"namespace",
	"instance",
	"severity",
	"route",
	"path",
	"uri",
	"handler",
	"operation",
	"status",
	"status_code",
	"http_status",
	"code",
}

func computeFingerprint(alertname, service, env string, labels map[string]string) string {
	h := sha1.New()
	parts := []string{
		"alertname=" + strings.TrimSpace(alertname),
		"service=" + strings.TrimSpace(service),
		"env=" + strings.TrimSpace(env),
	}
	for _, k := range fingerprintLabelKeys {
		if v := strings.TrimSpace(labels[k]); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	sort.Strings(parts)
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ====== 调用方法 ======

// UpsertOnFire 把 Grafana 告警上报给 backend。
//
// 返回：
//   - incident：当前 incident DTO（包含 assignee_open_id 等）；ID 必须埋到飞书卡片
//     的 button.value.incident_id，让后续 ACK/RESOLVE 都能反查回来。
//   - dedup：true 表示命中已有 fingerprint，调用方应 patch 卡片而非发新卡。
//   - fireMentions：admin 配了 fire-time 全员 @ 时由 backend 决定的列表（已去 OOO/
//     去 assignee/去停用）；空表示按原行为只 @ assignee。dedup 时也照样返回，
//     便于上层在 patch 卡片时保持一致。
func (b *IncidentBackend) UpsertOnFire(ctx context.Context, payload grafanaWebhook) (*incidentInfoDTO, bool, []string, error) {
	if b == nil || strings.TrimSpace(b.BaseURL) == "" {
		return nil, false, nil, errors.New("backend not configured")
	}
	first := grafanaAlert{}
	if len(payload.Alerts) > 0 {
		first = payload.Alerts[0]
	}
	alertname := firstNonEmpty(payload.CommonLabels["alertname"], first.Labels["alertname"], payload.Title, "Grafana Alert")
	service := firstNonEmpty(payload.CommonLabels["service"], "-")
	env := firstNonEmpty(payload.CommonLabels["env"], payload.CommonLabels["namespace"], "-")
	severity := firstNonEmpty(payload.CommonLabels["severity"], "P2")
	summary := firstNonEmpty(payload.CommonAnnotations["summary"], payload.Title, "")
	description := firstNonEmpty(payload.CommonAnnotations["description"], payload.Message, "")

	// 把 first alert + commonLabels 合并算 fingerprint，让"同一告警来自不同实例"
	// 共用一条 incident（commonLabels 已经是 grafana 给的"共有 label 集"）。
	mergedLabels := map[string]string{}
	for k, v := range payload.CommonLabels {
		mergedLabels[k] = v
	}
	for k, v := range first.Labels {
		if _, ok := mergedLabels[k]; !ok {
			mergedLabels[k] = v
		}
	}
	fp := computeFingerprint(alertname, service, env, mergedLabels)

	firedAt := ""
	if !first.StartsAt.IsZero() {
		firedAt = first.StartsAt.UTC().Format(time.RFC3339)
	}

	req := upsertIncidentRequest{
		Fingerprint:  fp,
		AlertName:    alertname,
		Service:      service,
		Env:          env,
		Severity:     severity,
		Summary:      summary,
		Description:  description,
		DashboardURL: grafanaLink(payload),
		FiredAt:      firedAt,
	}

	var reply upsertIncidentReply
	if err := b.callJSON(ctx, http.MethodPost, "/alert/v1/incidents:upsert", nil, req, &reply); err != nil {
		return nil, false, nil, err
	}
	if reply.Incident == nil {
		return nil, false, nil, errors.New("backend returned empty incident")
	}
	return reply.Incident, reply.Dedup, reply.FireMentionOpenIds, nil
}

func (b *IncidentBackend) ResolveByFingerprint(ctx context.Context, payload grafanaWebhook) (*incidentInfoDTO, bool, error) {
	if b == nil || strings.TrimSpace(b.BaseURL) == "" {
		return nil, false, errors.New("backend not configured")
	}
	first := grafanaAlert{}
	if len(payload.Alerts) > 0 {
		first = payload.Alerts[0]
	}
	alertname := firstNonEmpty(payload.CommonLabels["alertname"], first.Labels["alertname"], payload.Title, "Grafana Alert")
	service := firstNonEmpty(payload.CommonLabels["service"], "-")
	env := firstNonEmpty(payload.CommonLabels["env"], payload.CommonLabels["namespace"], "-")
	mergedLabels := map[string]string{}
	for k, v := range payload.CommonLabels {
		mergedLabels[k] = v
	}
	for k, v := range first.Labels {
		if _, ok := mergedLabels[k]; !ok {
			mergedLabels[k] = v
		}
	}
	fp := computeFingerprint(alertname, service, env, mergedLabels)
	if strings.TrimSpace(fp) == "" {
		return nil, false, errors.New("computed empty fingerprint")
	}

	body := map[string]any{
		"fingerprint":      fp,
		"resolver_open_id": "grafana",
		"note":             "Grafana RESOLVED webhook",
	}
	var reply resolveIncidentByFingerprintReply
	if err := b.callJSON(ctx, http.MethodPost, "/alert/v1/incidents:resolve_by_fingerprint", nil, body, &reply); err != nil {
		return nil, false, err
	}
	return reply.Incident, reply.Resolved, nil
}

func (b *IncidentBackend) CreateSilence(ctx context.Context, item alertSilence) (alertSilence, error) {
	if b == nil || strings.TrimSpace(b.BaseURL) == "" {
		return alertSilence{}, errors.New("backend not configured")
	}
	body := map[string]any{
		"fingerprint":    item.Fingerprint,
		"alertname":      item.AlertName,
		"service":        item.Service,
		"env":            item.Env,
		"severity":       item.Severity,
		"operatorOpenId": item.OperatorOpenID,
		"reason":         item.Reason,
		"expiresAt":      item.ExpiresAt.UTC().Format(time.RFC3339),
	}
	var reply createAlertSilenceReply
	if err := b.callJSON(ctx, http.MethodPost, "/alert/v1/silences", nil, body, &reply); err != nil {
		return alertSilence{}, err
	}
	if reply.Silence == nil {
		return alertSilence{}, errors.New("backend returned empty silence")
	}
	return reply.Silence.toAlertSilence()
}

func (b *IncidentBackend) MatchSilence(ctx context.Context, fingerprint string) (alertSilence, bool, error) {
	if b == nil || strings.TrimSpace(b.BaseURL) == "" {
		return alertSilence{}, false, errors.New("backend not configured")
	}
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return alertSilence{}, false, errors.New("fingerprint is empty")
	}
	var reply matchAlertSilenceReply
	if err := b.callJSON(ctx, http.MethodGet, "/alert/v1/silences/"+url.PathEscape(fp)+":match", nil, nil, &reply); err != nil {
		return alertSilence{}, false, err
	}
	if !reply.Active || reply.Silence == nil {
		return alertSilence{}, false, nil
	}
	item, err := reply.Silence.toAlertSilence()
	if err != nil {
		return alertSilence{}, false, err
	}
	return item, true, nil
}

func (b *IncidentBackend) Ack(ctx context.Context, id int64, operatorOpenID string) (*incidentInfoDTO, bool, error) {
	if b == nil || b.BaseURL == "" {
		return nil, false, errors.New("backend not configured")
	}
	body := map[string]any{"id": id, "operator_open_id": operatorOpenID}
	var reply ackIncidentReply
	if err := b.callJSON(ctx, http.MethodPost, fmt.Sprintf("/alert/v1/incidents/%d/ack", id), nil, body, &reply); err != nil {
		return nil, false, err
	}
	return reply.Incident, reply.Reassigned, nil
}

func (b *IncidentBackend) Reassign(ctx context.Context, id int64, fromOperator, toOpenID string) (*incidentInfoDTO, error) {
	if b == nil || b.BaseURL == "" {
		return nil, errors.New("backend not configured")
	}
	body := map[string]any{"id": id, "operator_open_id": fromOperator, "to_open_id": toOpenID}
	var reply struct {
		Incident *incidentInfoDTO `json:"incident"`
	}
	if err := b.callJSON(ctx, http.MethodPost, fmt.Sprintf("/alert/v1/incidents/%d/reassign", id), nil, body, &reply); err != nil {
		return nil, err
	}
	return reply.Incident, nil
}

func (b *IncidentBackend) Resolve(ctx context.Context, id int64, operatorOpenID, note string) (*incidentInfoDTO, int64, error) {
	if b == nil || b.BaseURL == "" {
		return nil, 0, errors.New("backend not configured")
	}
	body := map[string]any{"id": id, "operator_open_id": operatorOpenID, "note": note}
	var reply resolveIncidentReply
	if err := b.callJSON(ctx, http.MethodPost, fmt.Sprintf("/alert/v1/incidents/%d/resolve", id), nil, body, &reply); err != nil {
		return nil, 0, err
	}
	mins, _ := reply.ResolveMinutes.Int64()
	return reply.Incident, mins, nil
}

func (b *IncidentBackend) Discard(ctx context.Context, id int64, operatorOpenID, reason string) (*incidentInfoDTO, error) {
	if b == nil || b.BaseURL == "" {
		return nil, errors.New("backend not configured")
	}
	body := map[string]any{"id": id, "operator_open_id": operatorOpenID, "reason": reason}
	var reply struct {
		Incident *incidentInfoDTO `json:"incident"`
	}
	if err := b.callJSON(ctx, http.MethodPost, fmt.Sprintf("/alert/v1/incidents/%d/discard", id), nil, body, &reply); err != nil {
		return nil, err
	}
	return reply.Incident, nil
}

func (b *IncidentBackend) BindFeishuMessage(ctx context.Context, id int64, feishuMsgID string) error {
	if b == nil || b.BaseURL == "" {
		return errors.New("backend not configured")
	}
	body := map[string]any{"id": id, "feishuMsgId": feishuMsgID}
	return b.callJSON(ctx, http.MethodPost, fmt.Sprintf("/alert/v1/incidents/%d/bind_feishu_message", id), nil, body, nil)
}

// ListEscalationCandidates 升级调度器轮询 backend；返回需要 L1/L2 的列表。
func (b *IncidentBackend) ListEscalationCandidates(ctx context.Context) (*listEscalationReply, error) {
	if b == nil || b.BaseURL == "" {
		return nil, errors.New("backend not configured")
	}
	q := url.Values{}
	q.Set("now", time.Now().UTC().Format(time.RFC3339))
	var reply listEscalationReply
	if err := b.callJSON(ctx, http.MethodGet, "/alert/v1/incidents/escalation_candidates", q, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

func (b *IncidentBackend) MarkEscalated(ctx context.Context, id int64, level int32) error {
	if b == nil || b.BaseURL == "" {
		return errors.New("backend not configured")
	}
	body := map[string]any{"id": id, "level": level}
	return b.callJSON(ctx, http.MethodPost, fmt.Sprintf("/alert/v1/incidents/%d/mark_escalated", id), nil, body, nil)
}

type feishuFeedbackOncallAssigneeDTO struct {
	Role   string `json:"role"`
	OpenID string `json:"openId"`
	Name   string `json:"name"`
}

type feishuFeedbackOncallSnapshotReply struct {
	Enabled           bool                              `json:"enabled"`
	ChatID            string                            `json:"chatId"`
	ReplyPrefix       string                            `json:"replyPrefix"`
	MentionTTLSeconds json.Number                       `json:"mentionTtlSeconds"`
	Assignees         []feishuFeedbackOncallAssigneeDTO `json:"assignees"`
}

func (b *IncidentBackend) GetFeishuFeedbackOncallSnapshot(ctx context.Context) (*feishuFeedbackOncallSnapshotReply, error) {
	if b == nil || strings.TrimSpace(b.BaseURL) == "" {
		return nil, errors.New("backend not configured")
	}
	var reply feishuFeedbackOncallSnapshotReply
	if err := b.callJSON(ctx, http.MethodGet, "/alert/v1/oncall/feishu_feedback_oncall:snapshot", nil, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// protoJSONInt64 accepts both JSON numbers and protojson's default int64
// representation (quoted decimal strings).
type protoJSONInt64 int64

func (n *protoJSONInt64) UnmarshalJSON(data []byte) error {
	var text string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	} else {
		text = strings.TrimSpace(string(data))
	}
	if text == "" || text == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return err
	}
	*n = protoJSONInt64(v)
	return nil
}

func (n protoJSONInt64) Int() int {
	return int(int64(n))
}

// workOrderWeeklyStatsDTO 对应 matrix-backend GetWorkOrderWeeklyStatsReply 的
// protojson 输出（字段名为 lowerCamelCase）。
type workOrderWeeklyStatsDTO struct {
	Total                  protoJSONInt64 `json:"total"`
	PrevTotal              protoJSONInt64 `json:"prevTotal"`
	Untouched              protoJSONInt64 `json:"untouched"`
	Processing             protoJSONInt64 `json:"processing"`
	Closed                 protoJSONInt64 `json:"closed"`
	ClosedByUser           protoJSONInt64 `json:"closedByUser"`
	ClosedByAuto           protoJSONInt64 `json:"closedByAuto"`
	ClosedByAdmin          protoJSONInt64 `json:"closedByAdmin"`
	Replied                protoJSONInt64 `json:"replied"`
	RepliedWithin24h       protoJSONInt64 `json:"repliedWithin24h"`
	CurrentUnreplied       protoJSONInt64 `json:"currentUnreplied"`
	AvgFirstReplyMinutes   float64        `json:"avgFirstReplyMinutes"`
	MaxFirstReplyMinutes   float64        `json:"maxFirstReplyMinutes"`
	MaxUnrepliedAgeMinutes float64        `json:"maxUnrepliedAgeMinutes"`
	TopVersions            []struct {
		Label string         `json:"label"`
		Count protoJSONInt64 `json:"count"`
	} `json:"topVersions"`
	TopLanguages []struct {
		Label string         `json:"label"`
		Count protoJSONInt64 `json:"count"`
	} `json:"topLanguages"`
}

// GetWorkOrderWeeklyStats 拉取 [start, end) 窗口的 App 内工单聚合（只读）。
func (b *IncidentBackend) GetWorkOrderWeeklyStats(ctx context.Context, start, end time.Time) (*workOrderWeeklyStats, error) {
	if b == nil || strings.TrimSpace(b.BaseURL) == "" {
		return nil, errors.New("backend not configured")
	}
	q := url.Values{}
	q.Set("startTime", strconv.FormatInt(start.Unix(), 10))
	q.Set("endTime", strconv.FormatInt(end.Unix(), 10))
	var dto workOrderWeeklyStatsDTO
	if err := b.callJSON(ctx, http.MethodGet, "/api/work-orders/weekly-stats", q, nil, &dto); err != nil {
		return nil, err
	}
	out := &workOrderWeeklyStats{
		Total:            dto.Total.Int(),
		PrevTotal:        dto.PrevTotal.Int(),
		Untouched:        dto.Untouched.Int(),
		Processing:       dto.Processing.Int(),
		Closed:           dto.Closed.Int(),
		ClosedByUser:     dto.ClosedByUser.Int(),
		ClosedByAuto:     dto.ClosedByAuto.Int(),
		ClosedByAdmin:    dto.ClosedByAdmin.Int(),
		Replied:          dto.Replied.Int(),
		RepliedWithin24h: dto.RepliedWithin24h.Int(),
		CurrentUnreplied: dto.CurrentUnreplied.Int(),
		AvgFirstReplyM:   dto.AvgFirstReplyMinutes,
		MaxFirstReplyM:   dto.MaxFirstReplyMinutes,
		MaxUnrepliedAgeM: dto.MaxUnrepliedAgeMinutes,
	}
	for _, v := range dto.TopVersions {
		out.TopVersions = append(out.TopVersions, feedbackLabelCount{Label: v.Label, Count: v.Count.Int()})
	}
	for _, l := range dto.TopLanguages {
		out.TopLanguages = append(out.TopLanguages, feedbackLabelCount{Label: l.Label, Count: l.Count.Int()})
	}
	return out, nil
}

// ====== HTTP 调用辅助 ======

func (b *IncidentBackend) callJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	if b.HTTPClient == nil {
		b.HTTPClient = &http.Client{}
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := strings.TrimRight(b.BaseURL, "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(rctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(respBody))
		if len(detail) > 256 {
			detail = detail[:256]
		}
		return fmt.Errorf("backend %s %s -> %d: %s", method, path, resp.StatusCode, detail)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}
