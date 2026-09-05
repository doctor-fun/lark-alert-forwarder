package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// feishuMock spins up a minimal HTTPS-less Feishu API stub. We map a
// path to a static response body; tenant-token and contact endpoints
// share the same dispatcher so a single test can exercise both legs of
// the LookupUserEmail call chain.
type feishuMock struct {
	*httptest.Server
	calls []string
}

func newFeishuMock(t *testing.T, handlers map[string]func(w http.ResponseWriter, r *http.Request)) *feishuMock {
	t.Helper()
	m := &feishuMock{}
	mux := http.NewServeMux()
	for path, h := range handlers {
		path := path
		h := h
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			m.calls = append(m.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
			h(w, r)
		})
	}
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

func newServerWithFeishu(t *testing.T, baseURL string) *server {
	t.Helper()
	return &server{
		feishuAppID:     "cli_test",
		feishuAppSecret: "secret_test",
		feishuChatID:    "oc_test",
		feishuAPIBase:   baseURL,
		client:          &http.Client{Timeout: 5 * time.Second},
	}
}

func TestLookupUserEmailReturnsEnterpriseEmail(t *testing.T) {
	mock := newFeishuMock(t, map[string]func(http.ResponseWriter, *http.Request){
		"/open-apis/auth/v3/tenant_access_token/internal": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"t-test","expire":7200}`))
		},
		"/open-apis/contact/v3/users/ou_demo": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer t-test" {
				t.Errorf("expected Bearer t-test, got %q", got)
			}
			_, _ = w.Write([]byte(`{
				"code": 0, "msg": "ok",
				"data": {"user": {
					"open_id": "ou_demo",
					"name": "Derek",
					"email": "derek.personal@example.com",
					"enterprise_email": "alice@example.com"
				}}
			}`))
		},
	})
	s := newServerWithFeishu(t, mock.URL)

	email, err := s.LookupUserEmail(context.Background(), "ou_demo")
	if err != nil {
		t.Fatalf("LookupUserEmail: %v", err)
	}
	if email != "alice@example.com" {
		t.Errorf("expected enterprise_email preferred, got %q", email)
	}
}

func TestLookupUserEmailFallsBackToPersonalEmail(t *testing.T) {
	mock := newFeishuMock(t, map[string]func(http.ResponseWriter, *http.Request){
		"/open-apis/auth/v3/tenant_access_token/internal": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t","expire":7200}`))
		},
		"/open-apis/contact/v3/users/ou_demo": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"code": 0,
				"data": {"user": {"open_id":"ou_demo","name":"Derek","email":"derek@gmail.com","enterprise_email":""}}
			}`))
		},
	})
	s := newServerWithFeishu(t, mock.URL)

	email, err := s.LookupUserEmail(context.Background(), "ou_demo")
	if err != nil {
		t.Fatalf("LookupUserEmail: %v", err)
	}
	if email != "derek@gmail.com" {
		t.Errorf("expected fallback to email, got %q", email)
	}
}

func TestLookupUserEmailSurfacesScopeError(t *testing.T) {
	mock := newFeishuMock(t, map[string]func(http.ResponseWriter, *http.Request){
		"/open-apis/auth/v3/tenant_access_token/internal": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t","expire":7200}`))
		},
		"/open-apis/contact/v3/users/ou_demo": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":99991661,"msg":"missing scopes"}`))
		},
	})
	s := newServerWithFeishu(t, mock.URL)

	_, err := s.LookupUserEmail(context.Background(), "ou_demo")
	if err == nil {
		t.Fatal("expected scope-missing error")
	}
	if !errors.Is(err, errFeishuScopeMissing) {
		t.Errorf("expected errFeishuScopeMissing sentinel, got %v", err)
	}
	// Friendly message must mention the developer console so on-call
	// engineers don't need to grep our codebase.
	if !strings.Contains(err.Error(), "open.feishu.cn") {
		t.Errorf("expected actionable error mentioning open.feishu.cn, got %q", err.Error())
	}
}

func TestLookupUserEmailRejectsBlankOpenID(t *testing.T) {
	s := newServerWithFeishu(t, "http://unused")
	if _, err := s.LookupUserEmail(context.Background(), "  "); err == nil {
		t.Fatal("expected error for blank open_id")
	}
}

func TestLookupUserEmailRequiresAppCreds(t *testing.T) {
	s := &server{client: &http.Client{}, feishuAPIBase: "http://unused"}
	if _, err := s.LookupUserEmail(context.Background(), "ou_demo"); err == nil {
		t.Fatal("expected error when feishu app not configured")
	}
}
