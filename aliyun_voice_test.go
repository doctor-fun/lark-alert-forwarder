package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 阿里云官方文档 POP 签名示例（https://help.aliyun.com/zh/vms/the-http-protocol-and-signature）：
// 给定一组固定参数 + Timestamp + Nonce，文档给出了 sortedQueryString。
// 我们用同样的入参跑 popSign，比对中间 sortedQueryString 必须完全一致，
// 这样后续算签名就只是 HMAC-SHA1 的标准库行为，可以放心。
func TestPOPSign_OfficialFixtureSortedQuery(t *testing.T) {
	params := map[string]string{
		"AccessKeyId":      "testId",
		"Action":           "SingleCallByTts",
		"CalledNumber":     "130********",
		"CalledShowNumber": "057112345678",
		"Format":           "XML",
		"OutId":            "123",
		"RegionId":         "cn-hangzhou",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   "f7d2d4ef-6d5f-4da4-86ed-88e001a66abb",
		"SignatureVersion": "1.0",
		"Timestamp":        "2017-09-28T14:31:56Z",
		"TtsCode":          "TTS_0000000",
		"TtsParam":         `{"code":"1234","product":"test"}`,
		"Version":          "2017-05-25",
	}
	// 反向构造 popSign 内部的 sortedQueryString（即不带 stringToSign 那层包装）。
	// 期望值来自阿里云官方文档 "未URL编码的值（方便用户对比）" 那段。
	wantUnencoded := "AccessKeyId=testId&Action=SingleCallByTts&CalledNumber=130%2A%2A%2A%2A%2A%2A%2A%2A&CalledShowNumber=057112345678&Format=XML&OutId=123&RegionId=cn-hangzhou&SignatureMethod=HMAC-SHA1&SignatureNonce=f7d2d4ef-6d5f-4da4-86ed-88e001a66abb&SignatureVersion=1.0&Timestamp=2017-09-28T14%3A31%3A56Z&TtsCode=TTS_0000000&TtsParam=%7B%22code%22%3A%221234%22%2C%22product%22%3A%22test%22%7D&Version=2017-05-25"

	got := buildSortedQueryStringForTest(params)
	if got != wantUnencoded {
		t.Fatalf("sortedQueryString mismatch:\n got=%s\nwant=%s", got, wantUnencoded)
	}

	// 再验签：用假 secret 跑一遍 popSign，跟手写 HMAC-SHA1 比对。
	const secret = "fakeSecret"
	gotSig := popSign(http.MethodGet, params, secret)
	stringToSign := "GET&" + popPercentEncode("/") + "&" + popPercentEncode(wantUnencoded)
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	_, _ = mac.Write([]byte(stringToSign))
	wantSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if gotSig != wantSig {
		t.Fatalf("signature mismatch:\n got=%s\nwant=%s\nstringToSign=%s", gotSig, wantSig, stringToSign)
	}
}

// buildSortedQueryStringForTest 复用 popSign 中间逻辑，返回 sortedQueryString。
// 单独抽出来只在测试里用，避免污染 main 包 API。
func buildSortedQueryStringForTest(params map[string]string) string {
	var sb strings.Builder
	keys := sortedKeys(params)
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(popPercentEncode(k))
		sb.WriteByte('=')
		sb.WriteString(popPercentEncode(params[k]))
	}
	return sb.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 用标准库 sort.Strings 也行；这里复刻 popSign 的排序方式
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestPopPercentEncode_SpecialCases(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello world", "hello%20world"}, // 空格 → %20（不是 +）
		{"a*b", "a%2Ab"},                  // * → %2A
		{"~tilde", "~tilde"},              // ~ 不编码
		{"/", "%2F"},                      // 标准编码
		{`{"k":"v"}`, "%7B%22k%22%3A%22v%22%7D"},
	}
	for _, c := range cases {
		if got := popPercentEncode(c.in); got != c.want {
			t.Errorf("popPercentEncode(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"+8613800138000", "8613800138000"},
		{"86 138 0013 8000", "8613800138000"},
		{"138-0013-8000", "13800138000"},
		{" ", ""},
		{"+86138 *# 0000000", "861380000000"}, // 把非数字字符全去掉，避免奇怪输入
	}
	for _, c := range cases {
		if got := normalizePhone(c.in); got != c.want {
			t.Errorf("normalizePhone(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSingleCallByTts_NotConfigured(t *testing.T) {
	var c *AliyunVoiceClient
	if c.IsEnabled() {
		t.Fatal("nil client should not be enabled")
	}
	empty := &AliyunVoiceClient{} // 全空
	if empty.IsEnabled() {
		t.Fatal("empty client should not be enabled")
	}
	_, err := empty.SingleCallByTts(context.Background(), "+8613800138000", nil)
	if err == nil {
		t.Fatal("expected error when not configured")
	}
}

// 用 httptest.Server 模拟阿里云 API：收到请求后校验 query string + 返回 fake JSON。
// 验证 SingleCallByTts 整链路（参数装配 + 签名挂在 query + body 解析）。
func TestSingleCallByTts_EndToEndAgainstFake(t *testing.T) {
	var seenURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"OK","Message":"OK","RequestId":"req-1","CallId":"call-2"}`))
	}))
	defer srv.Close()

	c := &AliyunVoiceClient{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		TtsCode:         "TTS_TEST",
		Region:          "cn-hangzhou",
		Endpoint:        srv.URL,
		HTTPClient:      srv.Client(),
		nowFn:           func() time.Time { return time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC) },
		nonceFn:         func() string { return "deadbeef" },
	}
	callID, err := c.SingleCallByTts(context.Background(), "+8613800138000",
		map[string]string{"alertname": "demo", "severity": "P0"})
	if err != nil {
		t.Fatalf("SingleCallByTts: %v", err)
	}
	if callID != "call-2" {
		t.Fatalf("CallId=%q want call-2", callID)
	}
	for _, must := range []string{
		"Action=SingleCallByTts",
		"CalledNumber=8613800138000",
		"TtsCode=TTS_TEST",
		"SignatureNonce=deadbeef",
		"Signature=",
	} {
		if !strings.Contains(seenURL, must) {
			t.Errorf("seen URL missing %q in %s", must, seenURL)
		}
	}
}

func TestSingleCallByTts_AliyunErrorReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Code":"isv.INVALID_PARAMETERS","Message":"bad phone","RequestId":"r-x"}`))
	}))
	defer srv.Close()

	c := &AliyunVoiceClient{
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		TtsCode:         "TTS_TEST",
		Region:          "cn-hangzhou",
		Endpoint:        srv.URL,
		HTTPClient:      srv.Client(),
		nowFn:           func() time.Time { return time.Now() },
		nonceFn:         func() string { return "n1" },
	}
	_, err := c.SingleCallByTts(context.Background(), "+8613800138000", nil)
	if err == nil {
		t.Fatal("expected aliyun error to surface as Go error")
	}
	if !strings.Contains(err.Error(), "isv.INVALID_PARAMETERS") {
		t.Fatalf("err missing aliyun code: %v", err)
	}
}
