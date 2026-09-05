package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRunnerAnalyzerHappyPath(t *testing.T) {
	var gotPath string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent":"attribution-agent","status":"success","title":"matrix-api 5xx 上涨","facts":["来自 runner"],"judgement":["疑似下游"],"next_steps":["看 SLS"],"references":["sls-log-query"],"diagram_plantuml":"@startuml\nA --> B\n@enduml","generated_at":"2026-04-29T05:00:00Z"}`))
	}))
	defer srv.Close()

	an := HTTPRunnerAnalyzer{
		BaseURL:   srv.URL,
		AgentName: "attribution-agent",
		Timeout:   2 * time.Second,
	}
	report, err := an.Analyze(context.Background(), AnalysisRequest{
		AlertName: "MatrixAPI5xxDetected",
		Service:   "matrix-api",
		Env:       "prod",
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if gotPath != "/agents/attribution-agent/invoke" {
		t.Fatalf("path=%s", gotPath)
	}
	var payload runnerInvokeRequest
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Context["alertname"] != "MatrixAPI5xxDetected" {
		t.Fatalf("context lost: %#v", payload.Context)
	}
	if report.Title != "matrix-api 5xx 上涨" || len(report.Facts) != 1 {
		t.Fatalf("report mismatch: %#v", report)
	}
	if !strings.Contains(report.DiagramPlantUML, "@startuml") {
		t.Fatalf("diagram lost: %#v", report)
	}
}

func TestHTTPRunnerAnalyzerStatus5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := HTTPRunnerAnalyzer{BaseURL: srv.URL, Timeout: time.Second}.Analyze(context.Background(), AnalysisRequest{})
	if err == nil || !strings.Contains(err.Error(), "runner status 500") {
		t.Fatalf("expected runner status error, got %v", err)
	}
}

func TestHTTPRunnerAnalyzerStatusFailedReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"failed","error":"driver oops"}`))
	}))
	defer srv.Close()
	_, err := HTTPRunnerAnalyzer{BaseURL: srv.URL, Timeout: time.Second}.Analyze(context.Background(), AnalysisRequest{})
	if err == nil || !strings.Contains(err.Error(), "driver oops") {
		t.Fatalf("expected driver error, got %v", err)
	}
}

func TestHTTPRunnerAnalyzerFlattensObjectItems(t *testing.T) {
	body := `{
        "agent": "attribution-agent",
        "status": "success",
        "title": "matrix-api 5xx 上涨",
        "facts": [
            {"fact":"5xx burst","source":"context.alertname"},
            "env=prod"
        ],
        "judgement": "未知，需要进一步排查",
        "next_steps": [
            {"action":"check sls","purpose":"find error","keywords":["matrix-api"]},
            "open dashboard"
        ],
        "references": ["sls-log-query"],
        "diagram_plantuml": "@startuml\nAlert --> Evidence\nEvidence --> Judgement\n@enduml",
        "generated_at": "2026-04-29T05:00:00Z"
    }`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	report, err := HTTPRunnerAnalyzer{BaseURL: srv.URL, Timeout: time.Second}.
		Analyze(context.Background(), AnalysisRequest{AlertName: "MatrixAPI5xxDetected"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(report.Facts) != 2 {
		t.Fatalf("facts len = %d", len(report.Facts))
	}
	if !strings.Contains(report.Facts[0], "5xx burst") || !strings.Contains(report.Facts[0], "source=context.alertname") {
		t.Fatalf("facts[0] = %q", report.Facts[0])
	}
	if report.Facts[1] != "env=prod" {
		t.Fatalf("facts[1] = %q", report.Facts[1])
	}
	if len(report.Judgement) != 1 || !strings.Contains(report.Judgement[0], "需要进一步排查") {
		t.Fatalf("judgement = %#v", report.Judgement)
	}
	if len(report.NextSteps) != 2 ||
		!strings.Contains(report.NextSteps[0], "check sls") ||
		!strings.Contains(report.NextSteps[0], "purpose=find error") ||
		report.NextSteps[1] != "open dashboard" {
		t.Fatalf("next_steps = %#v", report.NextSteps)
	}
	if report.RawText != "" {
		t.Fatalf("raw_text should be empty when structured: %q", report.RawText)
	}
	if !strings.Contains(report.DiagramPlantUML, "Evidence --> Judgement") {
		t.Fatalf("diagram = %q", report.DiagramPlantUML)
	}
}

func TestFallbackAnalyzerUsesRuleBasedOnHTTPFailure(t *testing.T) {
	fallback := FallbackAnalyzer{
		Primary: HTTPRunnerAnalyzer{
			BaseURL: "http://127.0.0.1:1", // unreachable
			Timeout: 200 * time.Millisecond,
		},
		Fallback: RuleBasedAnalyzer{},
	}
	report, err := fallback.Analyze(context.Background(), AnalysisRequest{AlertName: "MatrixAPI5xxDetected", Service: "matrix-api", Env: "prod", Status: "firing"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if report.Title == "" || len(report.Judgement) == 0 {
		t.Fatalf("rule-based report invalid: %#v", report)
	}
	text := report.FormatText()
	if !strings.Contains(text, "规则兜底") || !strings.Contains(text, "AI runner 未产出有效归因") {
		t.Fatalf("fallback marker missing:\n%s", text)
	}
}
