package main

import (
	"strings"
	"testing"
)

func TestSplitOpenIDList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,,", nil},
		{"ou_a", []string{"ou_a"}},
		{"ou_a, ou_b , ou_c", []string{"ou_a", "ou_b", "ou_c"}},
		{" ,ou_x, , ou_y ", []string{"ou_x", "ou_y"}},
	}
	for _, c := range cases {
		got := splitOpenIDList(c.in)
		if !equalStrings(got, c.want) {
			t.Errorf("splitOpenIDList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPickEmergencyAssignee_Deterministic(t *testing.T) {
	list := []string{"ou_a", "ou_b", "ou_c"}
	// 同 fingerprint 反复调用必须命中同一个人——这是兜底设计的核心契约。
	first := pickEmergencyAssignee(list, "alertname=p0|service=matrix-api")
	for i := 0; i < 100; i++ {
		got := pickEmergencyAssignee(list, "alertname=p0|service=matrix-api")
		if got != first {
			t.Fatalf("non-deterministic pick: first=%s, got %s at iter %d", first, got, i)
		}
	}
}

func TestPickEmergencyAssignee_DistributesAcrossFingerprints(t *testing.T) {
	list := []string{"ou_a", "ou_b", "ou_c"}
	hit := map[string]int{}
	// 至少 3 个不同 fingerprint 应该命中 2 个以上不同人，验证不是退化成"永远第一个"。
	for i := 0; i < 30; i++ {
		fp := "alertname=test_" + string(rune('A'+i))
		hit[pickEmergencyAssignee(list, fp)]++
	}
	if len(hit) < 2 {
		t.Fatalf("pick is degenerate: only hit %d distinct assignees: %v", len(hit), hit)
	}
}

func TestPickEmergencyAssignee_EmptyAndSingle(t *testing.T) {
	if got := pickEmergencyAssignee(nil, "x"); got != "" {
		t.Errorf("empty list should return empty, got %s", got)
	}
	if got := pickEmergencyAssignee([]string{"ou_only"}, "x"); got != "ou_only" {
		t.Errorf("single-item list should always return that item, got %s", got)
	}
}

func TestGrafanaWebhook_FingerprintForEmergency_PrefersCommonLabels(t *testing.T) {
	g := grafanaWebhook{
		CommonLabels: map[string]string{
			"alertname": "HighErrorRate",
			"service":   "matrix-api",
			"env":       "prod",
		},
		Alerts: []grafanaAlert{
			{Labels: map[string]string{"alertname": "ShouldBeIgnored"}},
		},
		Title: "fallback title",
	}
	got := g.fingerprintForEmergency()
	if !strings.Contains(got, "alertname=HighErrorRate") || !strings.Contains(got, "service=matrix-api") {
		t.Fatalf("expected commonLabels-based fp, got %q", got)
	}
}

func TestGrafanaWebhook_FingerprintForEmergency_FallsBackToAlertLabels(t *testing.T) {
	g := grafanaWebhook{
		CommonLabels: nil,
		Alerts: []grafanaAlert{
			{Labels: map[string]string{
				"alertname": "Pod CrashLoop",
				"namespace": "matrix",
			}},
		},
		Title: "ignored",
	}
	got := g.fingerprintForEmergency()
	if !strings.Contains(got, "alertname=Pod CrashLoop") || !strings.Contains(got, "namespace=matrix") {
		t.Fatalf("expected alert[0].Labels-based fp, got %q", got)
	}
}

func TestGrafanaWebhook_FingerprintForEmergency_FallsBackToTitle(t *testing.T) {
	g := grafanaWebhook{Title: " Last-Resort Title "}
	got := g.fingerprintForEmergency()
	if got != "Last-Resort Title" {
		t.Fatalf("expected trimmed title, got %q", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
