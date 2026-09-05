package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubLookup lets summary-card tests pretend the Feishu contact API
// already returned an email, without spinning up an HTTP server.
type stubLookup struct {
	email string
	err   error
}

func (s stubLookup) LookupUserEmail(ctx context.Context, openID string) (string, error) {
	return s.email, s.err
}

func TestBuildCopilotSummaryCardEmailedShape(t *testing.T) {
	req := AnalysisRequest{
		AlertName: "MatrixAPI5xxDetected",
		Service:   "matrix-api",
		Env:       "prod",
		Severity:  "P1",
		Operator:  "Alice",
		Link:      "https://grafana.example.com/d/matrix-api?viewPanel=2",
	}
	report := AnalysisReport{
		Title:       "matrix-api 5xx 由 order-srv 140003 引发",
		Judgement:   []string{"下游 order-srv 返回 order not exist (biz_code=140003)，被 matrix-api 直接抛 5xx"},
		Facts:       []string{"caller=callback.go:39"}, // must NOT appear in summary card
		GeneratedAt: time.Date(2026, 4, 29, 14, 30, 0, 0, time.UTC),
	}

	card := buildCopilotSummaryCard(req, report, "alice@example.com", true)

	// Header should still be the red "P1" template so visual continuity
	// with the original alert is preserved.
	if card.Header.Template != "red" {
		t.Errorf("expected red header for P1, got %q", card.Header.Template)
	}
	if !strings.Contains(card.Header.Title.Content, "matrix-api 5xx") {
		t.Errorf("header title missing report title: %q", card.Header.Title.Content)
	}

	rendered := mustMarshal(t, card.Elements)

	// Operator + meta line must be present.
	if !strings.Contains(rendered, "Alice") {
		t.Errorf("missing operator: %s", rendered)
	}
	if !strings.Contains(rendered, "matrix-api") {
		t.Errorf("missing service in meta: %s", rendered)
	}
	// Judgement preview must appear (TL;DR line).
	if !strings.Contains(rendered, "order not exist") {
		t.Errorf("expected judgement preview in card, got: %s", rendered)
	}
	// Recipient banner must include the resolved email so the operator
	// knows where to look.
	if !strings.Contains(rendered, "alice@example.com") {
		t.Errorf("expected email recipient in banner, got: %s", rendered)
	}
	// Facts/Next steps MUST NOT bleed into the summary card — the email
	// is the source of truth for those.
	if strings.Contains(rendered, "callback.go:39") {
		t.Errorf("summary card should not include facts; got: %s", rendered)
	}
	// Action buttons must include "我来处理" + 大盘 link.
	if !strings.Contains(rendered, "我来处理") {
		t.Errorf("missing claim button: %s", rendered)
	}
	if !strings.Contains(rendered, "https://grafana.example.com/d/matrix-api") {
		t.Errorf("missing dashboard link: %s", rendered)
	}
}

func TestBuildCopilotSummaryCardEmailFailedShowsWarning(t *testing.T) {
	req := AnalysisRequest{Service: "matrix-api", Severity: "warn"}
	report := AnalysisReport{Title: "X", Judgement: []string{"hi"}}
	card := buildCopilotSummaryCard(req, report, "", false)

	rendered := mustMarshal(t, card.Elements)
	if !strings.Contains(rendered, "邮件投递失败") {
		t.Errorf("expected failure banner when emailSent=false; got: %s", rendered)
	}
}

func TestBuildCopilotSummaryCardTruncatesLongJudgement(t *testing.T) {
	long := strings.Repeat("根因猜测内容a", 50) // ~150 chars
	report := AnalysisReport{Judgement: []string{long}}
	card := buildCopilotSummaryCard(AnalysisRequest{Severity: "P1"}, report, "x@y.com", true)
	rendered := mustMarshal(t, card.Elements)
	if !strings.Contains(rendered, "…") {
		t.Errorf("expected long judgement to be ellipsised, got:\n%s", rendered)
	}
}

func TestResolveRecipientsPrefersOperatorEmail(t *testing.T) {
	s := &server{
		emailLookup: stubLookup{email: "derek@example.com"},
		fallbackTo:  []string{"oncall@example.com"},
	}
	req := AnalysisRequest{OperatorOpenID: "ou_demo"}
	got := s.resolveRecipients(context.Background(), req)
	want := []string{"derek@example.com"}
	if !sliceEqual(got, want) {
		t.Errorf("expected operator email preferred, got %v", got)
	}
}

func TestResolveRecipientsFallsBackOnLookupError(t *testing.T) {
	s := &server{
		emailLookup: stubLookup{err: errFeishuScopeMissing},
		fallbackTo:  []string{"oncall@example.com"},
	}
	req := AnalysisRequest{OperatorOpenID: "ou_demo"}
	got := s.resolveRecipients(context.Background(), req)
	want := []string{"oncall@example.com"}
	if !sliceEqual(got, want) {
		t.Errorf("expected fallback when lookup errors, got %v", got)
	}
}

func TestResolveRecipientsFallsBackWhenOpenIDMissing(t *testing.T) {
	s := &server{
		emailLookup: stubLookup{email: "should-not-be-used@example.com"},
		fallbackTo:  []string{"oncall@example.com"},
	}
	got := s.resolveRecipients(context.Background(), AnalysisRequest{})
	want := []string{"oncall@example.com"}
	if !sliceEqual(got, want) {
		t.Errorf("expected fallback when open_id missing, got %v", got)
	}
}

func TestResolveRecipientsEmptyWhenNoFallback(t *testing.T) {
	s := &server{
		emailLookup: stubLookup{err: errFeishuScopeMissing},
	}
	got := s.resolveRecipients(context.Background(), AnalysisRequest{OperatorOpenID: "ou_demo"})
	if got != nil {
		t.Errorf("expected nil recipients when no fallback, got %v", got)
	}
}

// TestReplyCopilotAnalysisEmailsAndPostsSummary ties the whole new path
// together: analyzer runs, mailer captures the HTML report, Feishu API
// captures the slim summary card, no full report leaks back into chat.
func TestReplyCopilotAnalysisEmailsAndPostsSummary(t *testing.T) {
	mailer := &fakeMailer{}
	var capturedCardBody []byte

	// Stand up a tiny Feishu API stub: tenant token + reply endpoint.
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t","expire":7200}`))
	})
	mux.HandleFunc("/open-apis/im/v1/messages/", func(w http.ResponseWriter, r *http.Request) {
		// expecting POST .../reply
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		capturedCardBody = body
		_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"om_reply"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &server{
		feishuAppID:     "cli_test",
		feishuAppSecret: "sec_test",
		feishuChatID:    "oc_test",
		feishuAPIBase:   srv.URL,
		client:          &http.Client{Timeout: 5 * time.Second},
		analyzer:        RuleBasedAnalyzer{},
		analysisTimeout: 5 * time.Second,
		mailer:          mailer,
		emailLookup:     stubLookup{email: "alice@example.com"},
	}

	req := AnalysisRequest{
		AlertName:      "MatrixAPI5xxDetected",
		Service:        "matrix-api",
		Env:            "prod",
		Severity:       "P1",
		Status:         "firing",
		Description:    "5xx burn rate",
		Operator:       "Alice",
		OperatorOpenID: "ou_demo",
		Link:           "https://grafana.example.com",
	}
	s.replyCopilotAnalysis("om_origin", req)

	msg, ok := mailer.lastMessage()
	if !ok {
		t.Fatal("expected an email to be sent, got none")
	}
	if !sliceEqual(msg.To, []string{"alice@example.com"}) {
		t.Errorf("unexpected To: %v", msg.To)
	}
	if !strings.Contains(msg.Subject, "MatrixAPI5xxDetected") {
		t.Errorf("subject missing alert name: %q", msg.Subject)
	}
	if !strings.Contains(msg.BodyHTML, "matrix-api") {
		t.Errorf("HTML body missing service ref: %q", msg.BodyHTML)
	}

	// The reply card must mention the recipient and MUST NOT carry the
	// full facts/judgement/next_steps payload (those live in the email).
	body := string(capturedCardBody)
	if !strings.Contains(body, "alice@example.com") {
		t.Errorf("summary card missing email banner; body=%s", body)
	}
	if strings.Contains(body, "查询") && strings.Contains(body, "ack-pod-connect") {
		t.Errorf("summary card leaked next_steps; body=%s", body)
	}
}

func TestReplyCopilotAnalysisFallsBackToFullCardOnEmailFailure(t *testing.T) {
	mailer := &fakeMailer{sendErr: errors.New("smtp dial: connection refused")}
	var capturedCard []byte

	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t","expire":7200}`))
	})
	mux.HandleFunc("/open-apis/im/v1/messages/", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		capturedCard = body
		_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"om_x"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &server{
		feishuAppID:     "cli_test",
		feishuAppSecret: "sec_test",
		feishuChatID:    "oc_test",
		feishuAPIBase:   srv.URL,
		client:          &http.Client{Timeout: 5 * time.Second},
		analyzer:        RuleBasedAnalyzer{},
		analysisTimeout: 5 * time.Second,
		mailer:          mailer,
		emailLookup:     stubLookup{email: "derek@example.com"},
	}
	req := AnalysisRequest{
		AlertName: "MatrixAPI5xxDetected", Service: "matrix-api",
		Env: "prod", Severity: "P1", OperatorOpenID: "ou_demo",
	}
	s.replyCopilotAnalysis("om_origin", req)

	// On email failure we MUST still post something useful to the
	// thread; expect the legacy full card (which contains facts list).
	body := string(capturedCard)
	if !strings.Contains(body, "事实") {
		t.Errorf("expected fallback to full card with 事实 section, got: %s", body)
	}
}

// mustMarshal renders any value to JSON for substring assertions.
// Centralised so test failure output is consistent across files.
func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// --- Chat broadcast tests ---

type stubChatMembers struct {
	members []ChatMember
	err     error
}

func (s stubChatMembers) ListChatMembers(ctx context.Context, chatID string) ([]ChatMember, error) {
	return s.members, s.err
}

func TestResolveRecipientsBroadcastsToAllMappedMembers(t *testing.T) {
	s := &server{
		chatMembers: stubChatMembers{members: []ChatMember{
			{OpenID: "ou_aaa", Name: "Alice"},
			{OpenID: "ou_bbb", Name: "Bob"},
			{OpenID: "ou_ccc", Name: "Carol"},
		}},
		emailMap: staticEmailMap{
			"ou_aaa": "alice@example.com",
			"ou_bbb": "bob@example.com",
			"ou_ccc": "carol@example.com",
		},
		emailLookup: stubLookup{email: "should-not-be-used@example.com"},
		fallbackTo:  []string{"fallback@example.com"},
	}
	req := AnalysisRequest{
		OperatorOpenID: "ou_aaa",
		OpenChatID:     "oc_test",
	}
	got := s.resolveRecipients(context.Background(), req)
	want := []string{"alice@example.com", "bob@example.com", "carol@example.com"}
	if !sliceEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestResolveRecipientsBroadcastDeduplicatesOperator(t *testing.T) {
	s := &server{
		chatMembers: stubChatMembers{members: []ChatMember{
			{OpenID: "ou_aaa", Name: "Alice"},
			{OpenID: "ou_bbb", Name: "Bob"},
		}},
		emailMap: staticEmailMap{
			"ou_aaa": "alice@example.com",
			"ou_bbb": "bob@example.com",
		},
	}
	req := AnalysisRequest{
		OperatorOpenID: "ou_aaa",
		OpenChatID:     "oc_test",
	}
	got := s.resolveRecipients(context.Background(), req)
	// operator (ou_aaa) already in broadcast list; must not duplicate
	if len(got) != 2 {
		t.Errorf("expected 2 recipients (no dup), got %v", got)
	}
}

func TestResolveRecipientsBroadcastSkipsUnmappedGracefully(t *testing.T) {
	s := &server{
		chatMembers: stubChatMembers{members: []ChatMember{
			{OpenID: "ou_aaa", Name: "Alice"},
			{OpenID: "ou_bbb", Name: "Bob"},
			{OpenID: "ou_ccc", Name: "新人"},
		}},
		emailMap: staticEmailMap{
			"ou_aaa": "alice@example.com",
			"ou_bbb": "bob@example.com",
			// ou_ccc not mapped
		},
		fallbackTo: []string{"fallback@example.com"},
	}
	req := AnalysisRequest{
		OperatorOpenID: "ou_aaa",
		OpenChatID:     "oc_test",
	}
	got := s.resolveRecipients(context.Background(), req)
	// should still include the 2 mapped members; NOT fall back
	want := []string{"alice@example.com", "bob@example.com"}
	if !sliceEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestResolveRecipientsFallsBackWhenNoMapConfigured(t *testing.T) {
	s := &server{
		chatMembers: stubChatMembers{members: []ChatMember{
			{OpenID: "ou_aaa", Name: "Alice"},
		}},
		// emailMap is nil → broadcast yields nothing → falls through
		emailLookup: stubLookup{err: errors.New("no email")},
		fallbackTo:  []string{"fallback@example.com"},
	}
	req := AnalysisRequest{
		OperatorOpenID: "ou_aaa",
		OpenChatID:     "oc_test",
	}
	got := s.resolveRecipients(context.Background(), req)
	want := []string{"fallback@example.com"}
	if !sliceEqual(got, want) {
		t.Errorf("expected fallback %v, got %v", want, got)
	}
}

func TestResolveRecipientsChatMemberErrorFallsThrough(t *testing.T) {
	s := &server{
		chatMembers: stubChatMembers{err: errors.New("feishu 500")},
		emailMap:    staticEmailMap{"ou_aaa": "a@x.com"},
		emailLookup: stubLookup{email: "operator@x.com"},
		fallbackTo:  []string{"fallback@x.com"},
	}
	req := AnalysisRequest{
		OperatorOpenID: "ou_aaa",
		OpenChatID:     "oc_test",
	}
	got := s.resolveRecipients(context.Background(), req)
	// Chat member list failed → operator looked up via map → still works
	want := []string{"a@x.com"}
	if !sliceEqual(got, want) {
		t.Errorf("expected operator from map %v, got %v", want, got)
	}
}
