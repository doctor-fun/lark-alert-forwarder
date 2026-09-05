package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshUserFeedbackOncallFromBackendIfStaleUsesNewAssignee(t *testing.T) {
	now := time.Now()
	var calls atomic.Int32
	backend := newUserFeedbackOncallSnapshotServer(t, &calls, http.StatusOK, `{
		"enabled":true,
		"chatId":"oc_feedback",
		"replyPrefix":"收到新的用户反馈，请跟进",
		"mentionTtlSeconds":"3600",
		"assignees":[{"role":"backend","openId":"ou_new","name":"新值班"}]
	}`)
	s := newUserFeedbackOncallRefreshTestServer(t, backend, now.Add(-userFeedbackOncallSnapshotFreshness-time.Second), "旧值班")

	if ok := s.refreshUserFeedbackOncallFromBackendIfStale(context.Background(), now); !ok {
		t.Fatal("stale snapshot refresh should succeed")
	}
	cfg, ok := s.effectiveUserFeedbackOncallConfig(now)
	if !ok {
		t.Fatal("refreshed config should be effective")
	}
	reply := buildUserFeedbackOncallReply(cfg.ReplyPrefix, cfg.Assignees)
	if !strings.Contains(reply, "新值班") || strings.Contains(reply, "旧值班") {
		t.Fatalf("reply should use refreshed assignee, got %q", reply)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls = %d, want 1", got)
	}
}

func TestRefreshUserFeedbackOncallFromBackendIfStaleKeepsOldSnapshotOnFailure(t *testing.T) {
	now := time.Now()
	var calls atomic.Int32
	backend := newUserFeedbackOncallSnapshotServer(t, &calls, http.StatusServiceUnavailable, `backend unavailable`)
	s := newUserFeedbackOncallRefreshTestServer(t, backend, now.Add(-userFeedbackOncallSnapshotFreshness-time.Second), "旧值班")

	if ok := s.refreshUserFeedbackOncallFromBackendIfStale(context.Background(), now); ok {
		t.Fatal("failed backend refresh should report failure")
	}
	cfg, ok := s.effectiveUserFeedbackOncallConfig(now)
	if !ok {
		t.Fatal("last valid snapshot should remain effective")
	}
	reply := buildUserFeedbackOncallReply(cfg.ReplyPrefix, cfg.Assignees)
	if !strings.Contains(reply, "旧值班") {
		t.Fatalf("reply should keep old assignee after refresh failure, got %q", reply)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls = %d, want 1", got)
	}
}

func TestRefreshUserFeedbackOncallFromBackendIfStaleSkipsFreshSnapshot(t *testing.T) {
	now := time.Now()
	var calls atomic.Int32
	backend := newUserFeedbackOncallSnapshotServer(t, &calls, http.StatusOK, `{
		"enabled":true,
		"chatId":"oc_feedback",
		"assignees":[{"role":"backend","openId":"ou_new","name":"新值班"}]
	}`)
	s := newUserFeedbackOncallRefreshTestServer(t, backend, now.Add(-time.Second), "当前值班")

	if ok := s.refreshUserFeedbackOncallFromBackendIfStale(context.Background(), now); !ok {
		t.Fatal("fresh snapshot should be considered available")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fresh snapshot should not request backend, calls = %d", got)
	}
}

func newUserFeedbackOncallSnapshotServer(
	t *testing.T,
	calls *atomic.Int32,
	status int,
	body string,
) *httptest.Server {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/alert/v1/oncall/feishu_feedback_oncall:snapshot" {
			t.Errorf("unexpected backend request method=%s path=%s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(backend.Close)
	return backend
}

func newUserFeedbackOncallRefreshTestServer(
	t *testing.T,
	backend *httptest.Server,
	snapshotAt time.Time,
	assigneeName string,
) *server {
	t.Helper()
	s := &server{
		backend: &IncidentBackend{
			BaseURL:    backend.URL,
			HTTPClient: backend.Client(),
			Timeout:    time.Second,
		},
		userFeedbackOncallSnapshot: &userFeedbackOncallRuntimeConfig{
			Enabled:     true,
			ChatID:      "oc_feedback",
			ReplyPrefix: "收到新的用户反馈，请跟进",
			MentionTTL:  time.Hour,
			Assignees: []userFeedbackOncallAssignee{{
				Role:   "backend",
				OpenID: "ou_old",
				Name:   assigneeName,
			}},
			Source: "backend",
		},
		userFeedbackOncallSnapshotAt: snapshotAt,
	}
	t.Cleanup(func() { userFeedbackOncallRefreshStates.Delete(s) })
	return s
}
