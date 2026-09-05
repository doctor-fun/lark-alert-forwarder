package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeVoiceProbeDialer struct {
	enabled bool
	mu      sync.Mutex
	calls   []string
}

func (f *fakeVoiceProbeDialer) IsEnabled() bool { return f.enabled }

func (f *fakeVoiceProbeDialer) SingleCallByTts(_ context.Context, phone string, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, normalizePhone(phone))
	return "call-" + normalizePhone(phone), nil
}

func TestParseVoiceProbeTargetsCSVWithHeader(t *testing.T) {
	raw := "open_id,name,phone,service\nou_a,张三,+8613800138000,matrix-api\n"
	got, err := parseVoiceProbeTargets(strings.NewReader(raw), "csv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].OpenID != "ou_a" || got[0].Name != "张三" || got[0].Phone != "+8613800138000" || got[0].Service != "matrix-api" {
		t.Fatalf("unexpected target: %+v", got[0])
	}
}

func TestParseVoiceProbeTargetsJSONL(t *testing.T) {
	raw := `{"open_id":"ou_a","name":"张三","phone":"+8613800138000"}` + "\n" +
		`{"open_id":"ou_b","name":"李四","phone":"+8613900139000","service":"user-srv"}`
	got, err := parseVoiceProbeTargets(strings.NewReader(raw), "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Service != "user-srv" {
		t.Fatalf("unexpected targets: %+v", got)
	}
}

func TestRunVoiceProbeDryRunDedupesAndMasksOutput(t *testing.T) {
	targets := []voiceProbeTarget{
		{OpenID: "ou_a", Name: "张三", Phone: "+8613800138000"},
		{OpenID: "ou_a", Name: "张三重复", Phone: "+8613800138000"},
		{OpenID: "ou_b", Name: "李四", Phone: "bad"},
	}
	var out bytes.Buffer
	dialer := &fakeVoiceProbeDialer{enabled: true}
	err := runVoiceProbe(context.Background(), voiceProbeConfig{
		DryRun:   true,
		Interval: time.Millisecond,
	}, targets, dialer, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(dialer.calls) != 0 {
		t.Fatalf("dry-run must not dial, got calls=%v", dialer.calls)
	}
	text := out.String()
	if strings.Contains(text, "13800138000") || strings.Contains(text, "+8613800138000") {
		t.Fatalf("output leaked phone: %s", text)
	}
	if !strings.Contains(text, `"masked_phone":"861******8000"`) {
		t.Fatalf("masked phone missing: %s", text)
	}
	if !strings.Contains(text, `"duplicates":1`) {
		t.Fatalf("summary should include duplicate count: %s", text)
	}
	if !strings.Contains(text, `"invalid_phones":1`) {
		t.Fatalf("summary should include invalid phone count: %s", text)
	}
}

func TestRunVoiceProbeDialsAndEmitsCallID(t *testing.T) {
	targets := []voiceProbeTarget{{OpenID: "ou_a", Name: "张三", Phone: "+8613800138000"}}
	var out bytes.Buffer
	dialer := &fakeVoiceProbeDialer{enabled: true}
	err := runVoiceProbe(context.Background(), voiceProbeConfig{
		DryRun:   false,
		Interval: 0,
		Timeout:  time.Second,
	}, targets, dialer, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(dialer.calls) != 1 || dialer.calls[0] != "8613800138000" {
		t.Fatalf("calls=%v", dialer.calls)
	}
	dec := json.NewDecoder(strings.NewReader(out.String()))
	var result voiceProbeResult
	if err := dec.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "called" || result.CallID != "call-8613800138000" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunVoiceProbeCLIRefusesDialWithoutConfirm(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runVoiceProbeCLI([]string{"--dry-run=false"}, strings.NewReader("open_id,name,phone\nou_a,a,+8613800138000\n"), &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(stderr.String(), "refusing to dial") {
		t.Fatalf("stderr=%s", stderr.String())
	}
}
