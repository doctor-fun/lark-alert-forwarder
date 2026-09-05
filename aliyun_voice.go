package main

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// AliyunVoiceClient 调阿里云"语音服务（dyvmsapi）SingleCallByTts" 接口拨打告警电话。
//
// 设计：
//   - 完全用 net/http 自实现 POP 签名（HMAC-SHA1），不引阿里云 SDK；保持 forwarder
//     单二进制小、依赖少。
//   - 配置全部走 env：ALIYUN_VOICE_ACCESS_KEY_ID / _SECRET / _TTS_CODE / _REGION /
//     _CALLED_SHOW_NUMBER。缺任一关键字段时 IsEnabled() 返回 false，escalator
//     不会触发拨号 —— 告警链路其他部分照常工作。
//   - SingleCallByTts 返回 RequestId / CallId 用于后续排查。
//
// 模板规范：用户在阿里云控制台申请 TtsCode（如 TTS_xxxxxxx），并定义一段含 ${alertname}
// / ${severity} 占位的语音播报文本，例如：
//
//	"matrix 告警通知，告警名称 ${alertname}，严重级别 ${severity}，请尽快处理"
//
// forwarder 在拨号时把告警里的对应字段填到 TtsParam 里。
type AliyunVoiceClient struct {
	AccessKeyID     string
	AccessKeySecret string
	// Region 阿里云语音服务的地域；默认 cn-hangzhou。海外地域目前不支持 dyvmsapi。
	Region string
	// TtsCode 模板 ID，必须先在阿里云控制台审核通过。空 → IsEnabled=false。
	TtsCode string
	// CalledShowNumber 主叫显示号；可选，未配则用阿里云默认主显号。
	CalledShowNumber string
	// Endpoint 自定义 endpoint；默认 dyvmsapi.aliyuncs.com。
	Endpoint string

	HTTPClient *http.Client
	// nowFn / nonceFn 注入到签名构造里，方便单测固定 Timestamp + SignatureNonce
	// 后跟阿里云官方文档示例的签名对比。生产路径用零值，回退到 time.Now / uuid。
	nowFn   func() time.Time
	nonceFn func() string
}

// NewAliyunVoiceClientFromEnv 从环境变量构造客户端。缺关键 env 时返回 nil。
//
// 这里**不返回 error**：缺配置 = 功能未开启 = 告警链路正常但 L2 不拨号。
// 让运维通过日志看到 "voice disabled" 即可，不要让 forwarder 启动失败。
func NewAliyunVoiceClientFromEnv() *AliyunVoiceClient {
	ak := strings.TrimSpace(envFirst("ALIYUN_VOICE_ACCESS_KEY_ID", "ALIBABA_CLOUD_ACCESS_KEY_ID"))
	sk := strings.TrimSpace(envFirst("ALIYUN_VOICE_ACCESS_KEY_SECRET", "ALIBABA_CLOUD_ACCESS_KEY_SECRET"))
	tts := strings.TrimSpace(envFirst("ALIYUN_VOICE_TTS_CODE"))
	if ak == "" || sk == "" || tts == "" {
		return nil
	}
	region := strings.TrimSpace(envFirst("ALIYUN_VOICE_REGION"))
	if region == "" {
		region = "cn-hangzhou"
	}
	endpoint := strings.TrimSpace(envFirst("ALIYUN_VOICE_ENDPOINT"))
	if endpoint == "" {
		endpoint = "https://dyvmsapi.aliyuncs.com"
	}
	return &AliyunVoiceClient{
		AccessKeyID:      ak,
		AccessKeySecret:  sk,
		Region:           region,
		TtsCode:          tts,
		CalledShowNumber: strings.TrimSpace(envFirst("ALIYUN_VOICE_CALLED_SHOW_NUMBER")),
		Endpoint:         endpoint,
		HTTPClient:       &http.Client{Timeout: 15 * time.Second},
	}
}

// IsEnabled 返回 client 是否具备拨号能力。nil 或缺关键字段时为 false。
func (c *AliyunVoiceClient) IsEnabled() bool {
	if c == nil {
		return false
	}
	return c.AccessKeyID != "" && c.AccessKeySecret != "" && c.TtsCode != ""
}

// SingleCallByTts 给单个号码发起一通 TTS 语音通知。
//
//   - calledNumber：国际格式手机号（含国家区号，如 +8613800138000），客户端会
//     自动剥掉 "+" 前缀（阿里云只接 86138 这种形式；不要带 + 号或空格）。
//   - ttsParam：填到模板里的占位符，例如 {"alertname": "...", "severity": "P0"}；
//     最终序列化成 JSON 作为 TtsParam 系统参数传递。
//
// 返回 CallId（阿里云端唯一标识）用于排查。出错时不区分网络错和业务错，
// 让 caller 直接 log 整条 err；escalator 会基于这个错 mark 失败但不阻塞下一轮。
func (c *AliyunVoiceClient) SingleCallByTts(ctx context.Context, calledNumber string, ttsParam map[string]string) (string, error) {
	if !c.IsEnabled() {
		return "", fmt.Errorf("aliyun voice not configured")
	}
	called := normalizePhone(calledNumber)
	if called == "" {
		return "", fmt.Errorf("invalid called number %q", calledNumber)
	}

	now := c.now()
	nonce := c.nonce()

	// 业务参数 + 系统参数全部进 map，统一排序后参与签名。
	params := map[string]string{
		"Format":           "JSON",
		"Version":          "2017-05-25",
		"AccessKeyId":      c.AccessKeyID,
		"SignatureMethod":  "HMAC-SHA1",
		"Timestamp":        now.UTC().Format("2006-01-02T15:04:05Z"),
		"SignatureVersion": "1.0",
		"SignatureNonce":   nonce,
		"Action":           "SingleCallByTts",
		"RegionId":         c.Region,
		"CalledNumber":     called,
		"TtsCode":          c.TtsCode,
	}
	if c.CalledShowNumber != "" {
		params["CalledShowNumber"] = c.CalledShowNumber
	}
	if len(ttsParam) > 0 {
		raw, err := json.Marshal(ttsParam)
		if err != nil {
			return "", fmt.Errorf("marshal tts_param: %w", err)
		}
		params["TtsParam"] = string(raw)
	}

	signature := popSign(http.MethodGet, params, c.AccessKeySecret)
	params["Signature"] = signature

	// 构造请求 URL；POP 协议要求所有参数都在 query string 里，包括 Signature。
	q := make(url.Values, len(params))
	for k, v := range params {
		q.Set(k, v)
	}
	endpoint := c.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}
	full := endpoint + "/?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	// 阿里云不论成功失败都返回 200 + JSON body；Code="OK" 才算成功。
	var out struct {
		Code      string `json:"Code"`
		Message   string `json:"Message"`
		RequestId string `json:"RequestId"`
		CallId    string `json:"CallId"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode aliyun response http=%d: %w; body=%s",
			resp.StatusCode, err, truncate(string(body), 256))
	}
	if out.Code != "OK" {
		return "", fmt.Errorf("aliyun voice call failed http=%d code=%s message=%s requestId=%s",
			resp.StatusCode, out.Code, out.Message, out.RequestId)
	}
	return out.CallId, nil
}

func (c *AliyunVoiceClient) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

func (c *AliyunVoiceClient) nonce() string {
	if c.nonceFn != nil {
		return c.nonceFn()
	}
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// rand 失败是极小概率事件；用 timestamp+pid 兜底，签名仍然能通过
		// （nonce 只要"在 15 分钟窗口内不重复"即可）。
		return fmt.Sprintf("nonce-%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// popSign 实现阿里云 POP 签名算法（HMAC-SHA1）。
//
// 步骤（按官方文档）：
//  1. 按 key 字典序对 params 排序；
//  2. 对每个 k=v 做 popPercentEncode 再 join 成 a=1&b=2 形式；
//  3. stringToSign = METHOD + "&" + popEncode("/") + "&" + popEncode(canonicalized)
//  4. HMAC-SHA1(stringToSign, secret+"&") → base64
//
// 单测里用阿里云官方 fixture 校验，所以这个函数改动一定要先跑 TestPOPSign_OfficialFixture。
func popSign(method string, params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(popPercentEncode(k))
		sb.WriteByte('=')
		sb.WriteString(popPercentEncode(params[k]))
	}
	stringToSign := strings.ToUpper(method) + "&" + popPercentEncode("/") + "&" + popPercentEncode(sb.String())

	mac := hmac.New(sha1.New, []byte(secret+"&"))
	_, _ = mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// popPercentEncode 在标准 RFC 3986 基础上做 3 个替换：
//
//	+    → %20
//	*    → %2A
//	%7E  → ~     （取消 ~ 的转义）
//
// 直接用 url.QueryEscape 会把空格变成 +，必须二次替换。
func popPercentEncode(s string) string {
	enc := url.QueryEscape(s)
	enc = strings.ReplaceAll(enc, "+", "%20")
	enc = strings.ReplaceAll(enc, "*", "%2A")
	enc = strings.ReplaceAll(enc, "%7E", "~")
	return enc
}

// normalizePhone 把 OncallMember.phone 里可能的 "+86 138-0000-0000" 类格式归一成 "8613800000000"。
// 阿里云 CalledNumber 要求纯数字，可以带国家码，但不允许 + / 空格 / 横杠。
func normalizePhone(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// envFirst 取第一个非空的环境变量；用来兼容多个常见 env 名（如 ALIYUN_VOICE_*
// 和 ALIBABA_CLOUD_*）。如果某一天我们统一改名也只要再加一个备选。
func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

