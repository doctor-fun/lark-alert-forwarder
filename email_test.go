package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMailer captures every Send call so end-to-end tests can assert on
// what would have been delivered, without standing up a real SMTP server.
// Safe for concurrent use because replyCopilotAnalysis runs in a
// goroutine and tests sometimes await delivery from a different one.
type fakeMailer struct {
	mu       sync.Mutex
	messages []EmailMessage
	sendErr  error
}

func (f *fakeMailer) Send(ctx context.Context, msg EmailMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.messages = append(f.messages, msg)
	return nil
}

func (f *fakeMailer) lastMessage() (EmailMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return EmailMessage{}, false
	}
	return f.messages[len(f.messages)-1], true
}

func TestNewSMTPMailerValidatesRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		cfg     SMTPConfig
		wantErr string
	}{
		{
			name:    "missing host",
			cfg:     SMTPConfig{Port: 465, Username: "u", Password: "p"},
			wantErr: "host is required",
		},
		{
			name:    "missing port",
			cfg:     SMTPConfig{Host: "smtp.example.com", Username: "u", Password: "p"},
			wantErr: "port is required",
		},
		{
			name:    "missing creds",
			cfg:     SMTPConfig{Host: "smtp.example.com", Port: 465},
			wantErr: "username and password are required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewSMTPMailer(tc.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got mailer=%v", tc.wantErr, m)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestNewSMTPMailerDefaultsTimeoutAndFrom(t *testing.T) {
	m, err := NewSMTPMailer(SMTPConfig{
		Host: "smtp.exmail.qq.com", Port: 465,
		Username: "alerts@example.com", Password: "secret",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if m.cfg.Timeout != 15*time.Second {
		t.Errorf("expected default timeout 15s, got %v", m.cfg.Timeout)
	}
	// FromAddress should default to Username so a partially-configured
	// deployment doesn't end up with a blank From header.
	if m.cfg.FromAddress != "alerts@example.com" {
		t.Errorf("expected FromAddress to default to Username, got %q", m.cfg.FromAddress)
	}
}

func TestRenderRFC5322MultipartIncludesBothBodies(t *testing.T) {
	msg := EmailMessage{
		To:       []string{"ops@example.com"},
		Cc:       []string{"oncall@example.com"},
		Subject:  "[Copilot][prod/P1] matrix-api 5xx",
		BodyText: "plain text body",
		BodyHTML: "<h1>html body</h1>",
	}
	wire, err := renderRFC5322(msg, "alerts@example.com", "Alert Copilot")
	if err != nil {
		t.Fatalf("renderRFC5322: %v", err)
	}
	w := string(wire)
	for _, want := range []string{
		"From: =?utf-8?B?", // Alert Copilot is ASCII, but encoder still wraps non-ASCII; ensure header rendered
		"To: ops@example.com",
		"Cc: oncall@example.com",
		"multipart/alternative",
		"text/plain",
		"text/html",
		"Content-Transfer-Encoding: base64",
	} {
		if want == "From: =?utf-8?B?" {
			// Alert Copilot is pure ASCII; expect the simpler form.
			if !strings.Contains(w, "From: Alert Copilot <alerts@example.com>") {
				t.Errorf("expected From header to include display name, got:\n%s", w)
			}
			continue
		}
		if !strings.Contains(w, want) {
			t.Errorf("expected wire to contain %q\nwire:\n%s", want, w)
		}
	}
	// Bodies are base64-encoded to keep SMTP lines under RFC 5321 998 octets.
	if !strings.Contains(w, base64Encode([]byte("plain text body"))) {
		t.Errorf("expected base64 plain body in wire:\n%s", w)
	}
	if !strings.Contains(w, base64Encode([]byte("<h1>html body</h1>"))) {
		t.Errorf("expected base64 html body in wire:\n%s", w)
	}
}

func TestRenderRFC5322FoldsLongHTMLLines(t *testing.T) {
	long := "<div style=\"" + strings.Repeat("x", 1200) + "\">ok</div>"
	msg := EmailMessage{
		To:       []string{"ops@example.com"},
		Subject:  "long body",
		BodyHTML: long,
	}
	wire, err := renderRFC5322(msg, "alerts@example.com", "")
	if err != nil {
		t.Fatalf("renderRFC5322: %v", err)
	}
	for i, line := range strings.Split(string(wire), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line %d length %d exceeds RFC 5321 limit: %q...", i, len(line), line[:80])
		}
	}
}

func TestRenderRFC5322EncodesCJKSubject(t *testing.T) {
	msg := EmailMessage{
		To:       []string{"ops@example.com"},
		Subject:  "告警 Copilot 归因",
		BodyText: "x",
	}
	wire, err := renderRFC5322(msg, "alerts@example.com", "")
	if err != nil {
		t.Fatalf("renderRFC5322: %v", err)
	}
	w := string(wire)
	if !strings.Contains(w, "Subject: =?utf-8?B?") {
		t.Errorf("CJK subject must be MIME encoded, got:\n%s", w)
	}
	// Raw UTF-8 must NOT appear in headers — would break Outlook.
	if strings.Contains(w[:strings.Index(w, "\r\n\r\n")], "告警 Copilot") {
		t.Errorf("raw CJK leaked into headers")
	}
}

func TestRenderReportEmailIncludesAllSections(t *testing.T) {
	req := AnalysisRequest{
		AlertName: "MatrixAPI5xxDetected",
		Service:   "matrix-api",
		Env:       "prod",
		Severity:  "P1",
		Operator:  "Derek",
		Link:      "https://grafana.example.com/d/matrix-api",
	}
	report := AnalysisReport{
		Title:           "matrix-api 5xx 由 order-srv 140003 引发",
		Facts:           []string{"caller=callback.go:39", "biz_code=140003"},
		Judgement:       []string{"下游 order-srv 返回 order not exist"},
		NextSteps:       []string{"在 order-srv 反查 transactionId"},
		References:      []string{"sls-log-query"},
		DiagramPlantUML: "@startuml\nAlert --> Evidence\nEvidence --> Judgement\n@enduml",
		GeneratedAt:     time.Date(2026, 4, 29, 6, 0, 0, 0, time.UTC),
	}
	msg, err := renderReportEmail(req, report, []string{"derek@example.com"})
	if err != nil {
		t.Fatalf("renderReportEmail: %v", err)
	}
	if want := "[Copilot][prod/P1] MatrixAPI5xxDetected"; !strings.Contains(msg.Subject, want) {
		t.Errorf("subject missing %q: %q", want, msg.Subject)
	}
	body := msg.BodyHTML
	for _, want := range []string{
		"matrix-api 5xx 由 order-srv 140003 引发",
		"caller=callback.go:39",
		"biz_code=140003",
		"下游 order-srv 返回 order not exist",
		"在 order-srv 反查 transactionId",
		"RCA 证据链（展开版）",
		"1. 告警现象",
		"2. 已确认事实",
		"3. 待补证 / 排除项",
		"4. 当前判断",
		"5. 建议动作",
		"归因结论与置信度",
		"sls-log-query",
		"https://grafana.example.com/d/matrix-api",
		// header colour: P1 = critical = red
		"#d83931",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Contains(body, "@startuml") || strings.Contains(body, "Alert --&gt; Evidence") {
		t.Errorf("HTML body should render a readable RCA flow instead of raw PlantUML:\n%s", body)
	}
	// Plain-text fallback must also have the report so screen readers /
	// non-HTML clients see something.
	if !strings.Contains(msg.BodyText, "caller=callback.go:39") {
		t.Errorf("plain text body missing facts: %q", msg.BodyText)
	}
	if !strings.Contains(msg.BodyText, "@startuml") {
		t.Errorf("plain text body missing diagram: %q", msg.BodyText)
	}
}

func TestRenderReportEmailRefusesEmptyTo(t *testing.T) {
	_, err := renderReportEmail(AnalysisRequest{}, AnalysisReport{}, nil)
	if err == nil {
		t.Fatal("expected error for empty To list")
	}
	if !strings.Contains(err.Error(), "at least one recipient") {
		t.Errorf("expected recipient-required error, got: %v", err)
	}
}

func TestSMTPMailerFromEnvDisabledWhenHostMissing(t *testing.T) {
	t.Setenv("SMTP_HOST", "")
	m, err := smtpMailerFromEnv()
	if err != nil || m != nil {
		t.Fatalf("expected (nil, nil) when SMTP_HOST is empty, got mailer=%v err=%v", m, err)
	}
}

func TestSMTPMailerFromEnvRequiresValidPort(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_PORT", "")
	if _, err := smtpMailerFromEnv(); err == nil {
		t.Fatal("expected error for missing SMTP_PORT")
	}
	t.Setenv("SMTP_PORT", "abc")
	if _, err := smtpMailerFromEnv(); err == nil {
		t.Fatal("expected error for non-numeric SMTP_PORT")
	}
}

func TestSMTPMailerFromEnvHappyPath(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.exmail.qq.com")
	t.Setenv("SMTP_PORT", "465")
	t.Setenv("SMTP_USERNAME", "alerts@example.com")
	t.Setenv("SMTP_PASSWORD", "secret")
	t.Setenv("SMTP_FROM_NAME", "Alert Copilot")
	t.Setenv("SMTP_USE_TLS", "true")
	m, err := smtpMailerFromEnv()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if m == nil {
		t.Fatal("expected mailer, got nil")
	}
	if m.cfg.Host != "smtp.exmail.qq.com" || m.cfg.Port != 465 {
		t.Errorf("config mismatch: %+v", m.cfg)
	}
	if !m.cfg.UseTLS {
		t.Errorf("expected UseTLS=true")
	}
	if m.cfg.FromName != "Alert Copilot" {
		t.Errorf("expected FromName=Alert Copilot, got %q", m.cfg.FromName)
	}
}

func TestSplitEmailListHandlesWhitespaceAndEmpty(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"a@x.com", []string{"a@x.com"}},
		{"a@x.com,b@x.com", []string{"a@x.com", "b@x.com"}},
		{" a@x.com , , b@x.com ", []string{"a@x.com", "b@x.com"}},
	}
	for _, tc := range cases {
		got := splitEmailList(tc.in)
		if !sliceEqual(got, tc.want) {
			t.Errorf("splitEmailList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func sliceEqual(a, b []string) bool {
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
