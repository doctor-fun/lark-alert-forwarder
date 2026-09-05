package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDataWorksToGrafana_QueryParamsWin(t *testing.T) {
	s := &server{}
	body := dataWorksPayload{
		Content:  "任务 sync_user 同步失败，已重试 3 次",
		Severity: "P3",
		Service:  "from-body",
	}
	q := url.Values{}
	q.Set("service", "data-platform")
	q.Set("severity", "P0")
	q.Set("alertname", "DataSyncFailed")
	q.Set("env", "prod")

	payload := s.dataWorksToGrafana(body, q, "")

	if payload.CommonLabels["service"] != "data-platform" {
		t.Fatalf("service should come from query, got %s", payload.CommonLabels["service"])
	}
	if payload.CommonLabels["severity"] != "P0" {
		t.Fatalf("severity should come from query, got %s", payload.CommonLabels["severity"])
	}
	if payload.CommonLabels["alertname"] != "DataSyncFailed" {
		t.Fatalf("alertname should come from query, got %s", payload.CommonLabels["alertname"])
	}
	if payload.CommonLabels["source"] != "dataworks" {
		t.Fatalf("source label missing, got %#v", payload.CommonLabels)
	}
	if payload.Status != "firing" {
		t.Fatalf("status should default firing, got %s", payload.Status)
	}
	if payload.CommonAnnotations["description"] != body.Content {
		t.Fatalf("description should carry body content, got %s", payload.CommonAnnotations["description"])
	}
	if len(payload.Alerts) != 1 || payload.Alerts[0].Status != "firing" {
		t.Fatalf("expected one firing alert, got %#v", payload.Alerts)
	}
}

func TestDataWorksToGrafana_Defaults(t *testing.T) {
	s := &server{}
	payload := s.dataWorksToGrafana(dataWorksPayload{}, url.Values{}, "raw text fallback")

	if payload.CommonLabels["service"] != "dataworks" {
		t.Fatalf("service default should be dataworks, got %s", payload.CommonLabels["service"])
	}
	if payload.CommonLabels["severity"] != "P1" {
		t.Fatalf("severity default should be P1, got %s", payload.CommonLabels["severity"])
	}
	if payload.CommonLabels["env"] != "prod" {
		t.Fatalf("env default should be prod, got %s", payload.CommonLabels["env"])
	}
	if payload.CommonAnnotations["description"] != "raw text fallback" {
		t.Fatalf("description should fall back to raw body, got %s", payload.CommonAnnotations["description"])
	}
}

func TestDataWorksToGrafana_ResolvedStatus(t *testing.T) {
	s := &server{}
	q := url.Values{}
	q.Set("status", "resolved")
	payload := s.dataWorksToGrafana(dataWorksPayload{Content: "恢复"}, q, "")
	if payload.Status != "resolved" {
		t.Fatalf("status should be resolved, got %s", payload.Status)
	}
}

func TestHandleDataWorksAlert_Unauthorized(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/dataworks/alert", strings.NewReader(`{"content":"x"}`))
	rec := httptest.NewRecorder()
	s.handleDataWorksAlert(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token should be 401, got %d", rec.Code)
	}
}

func TestEnqueueRefactorMetric_SkipsDataWorksSource(t *testing.T) {
	called := make(chan struct{}, 1)
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		_, _ = w.Write([]byte(`{"item":{"id":"wi_x"},"duplicate":false}`))
	}))
	defer orchestrator.Close()

	s := &server{
		refactorAutoMetric: true,
		refactorOrchestrator: &RefactorOrchestratorClient{
			BaseURL:    orchestrator.URL,
			HTTPClient: orchestrator.Client(),
		},
	}
	payload := grafanaWebhook{
		Status:       "firing",
		CommonLabels: map[string]string{"service": "data-platform", "source": "dataworks"},
	}
	s.enqueueRefactorMetricFromGrafana(payload, cardContext{})

	select {
	case <-called:
		t.Fatal("dataworks-sourced alert must NOT enqueue a refactor task")
	case <-time.After(300 * time.Millisecond):
		// 期望：没有任何入队请求打到 orchestrator
	}
}

func TestValidForwarderToken_QueryParam(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/dataworks/alert?token=secret", nil)
	if !s.validForwarderToken(req) {
		t.Fatal("query token=secret should be valid")
	}
	bad := httptest.NewRequest(http.MethodPost, "/dataworks/alert?token=wrong", nil)
	if s.validForwarderToken(bad) {
		t.Fatal("wrong query token should be invalid")
	}
}
