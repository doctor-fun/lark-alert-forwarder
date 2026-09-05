package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type capturingFeishu struct {
	mu      sync.Mutex
	replies []string // "messageID|text"
	err     error
}

func (c *capturingFeishu) replyFeishuMessage(_ context.Context, messageID, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.replies = append(c.replies, messageID+"|"+text)
	return nil
}

func TestEscalator_L1IncludesMentionsAndMarks(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL1: []escalationCandidate{
					{
						ID:             json.Number("42"),
						Alertname:      "matrix-api 5xx",
						Service:        "matrix-api",
						AssigneeOpenID: "ou_assignee",
						FeishuMsgID:    "om_xxx",
						MentionOpenIds: []string{"ou_a", "ou_b"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			atomic.AddInt32(&markCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	feishu := &capturingFeishu{}
	esc := &Escalator{
		backend:  &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:   feishu,
		interval: time.Hour, // 不让 loop 自己跑；我们手动调 tick
		stop:     make(chan struct{}),
	}
	esc.tick()

	if got := atomic.LoadInt32(&markCount); got != 1 {
		t.Fatalf("expect 1 mark_escalated, got %d", got)
	}
	feishu.mu.Lock()
	defer feishu.mu.Unlock()
	if len(feishu.replies) != 1 {
		t.Fatalf("expect 1 thread reply, got %d", len(feishu.replies))
	}
	if !strings.Contains(feishu.replies[0], "om_xxx|") {
		t.Fatalf("reply must target om_xxx: %s", feishu.replies[0])
	}
	if !strings.Contains(feishu.replies[0], `<at user_id="ou_a">`) ||
		!strings.Contains(feishu.replies[0], `<at user_id="ou_b">`) {
		t.Fatalf("reply must @ both ou_a and ou_b: %s", feishu.replies[0])
	}
	if !strings.Contains(feishu.replies[0], "L1 升级") {
		t.Fatalf("reply must mention L1: %s", feishu.replies[0])
	}
}

func TestEscalator_DataGroupUsesCaoOnlyAndLeavesL2Pending(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mark_escalated") {
			atomic.AddInt32(&markCount, 1)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	feishu := &capturingFeishu{}
	esc := &Escalator{
		backend:                     &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: time.Second},
		feishu:                      feishu,
		dataServiceMatcher:          func(service string) bool { return service == "attribution-service" },
		dataEscalationMentionOpenID: "ou_cao",
		stop:                        make(chan struct{}),
	}
	candidate := escalationCandidate{
		ID:             json.Number("42"),
		Service:        "attribution-service",
		Alertname:      "AttributionError",
		FeishuMsgID:    "om_data",
		MentionOpenIds: []string{"ou_other_a", "ou_other_b"},
		MentionPhones:  []string{"8613800138000"},
	}
	esc.escalate(context.Background(), candidate, 1)
	esc.escalate(context.Background(), candidate, 2)

	if got := atomic.LoadInt32(&markCount); got != 1 {
		t.Fatalf("mark count=%d, want only L1 marked", got)
	}
	feishu.mu.Lock()
	defer feishu.mu.Unlock()
	if len(feishu.replies) != 1 {
		t.Fatalf("replies=%v, want only L1", feishu.replies)
	}
	if !strings.Contains(feishu.replies[0], `<at user_id="ou_cao">`) ||
		strings.Contains(feishu.replies[0], "ou_other_") {
		t.Fatalf("L1 must only @ Cao: %s", feishu.replies[0])
	}
}

func TestEscalator_L2UsesLeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL2: []escalationCandidate{
					{ID: json.Number("7"), Alertname: "x", FeishuMsgID: "om_l2", MentionOpenIds: []string{"ou_lead"}},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	feishu := &capturingFeishu{}
	esc := &Escalator{
		backend:  &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:   feishu,
		interval: time.Hour,
		stop:     make(chan struct{}),
	}
	esc.tick()
	feishu.mu.Lock()
	defer feishu.mu.Unlock()
	if len(feishu.replies) != 1 {
		t.Fatalf("expect 1 reply, got %d", len(feishu.replies))
	}
	if !strings.Contains(feishu.replies[0], `<at user_id="ou_lead">`) {
		t.Fatalf("L2 must @ leader: %s", feishu.replies[0])
	}
	if !strings.Contains(feishu.replies[0], "L2 升级") {
		t.Fatalf("expect L2: %s", feishu.replies[0])
	}
}

// fakeVoice 单测用语音通知 mock；记录所有拨号调用，并能注入失败。
type fakeVoice struct {
	mu      sync.Mutex
	calls   []string // phone
	enabled bool
	err     error
}

func (f *fakeVoice) IsEnabled() bool { return f.enabled }
func (f *fakeVoice) SingleCallByTts(_ context.Context, phone string, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, phone)
	if f.err != nil {
		return "", f.err
	}
	return "call-" + phone, nil
}

// L2 升级时 mention_phones 不为空 + voice 启用 → 应该并发拨号一次每号；mark_escalated 仍执行。
func TestEscalator_L2DialsVoiceWhenEnabled(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL2: []escalationCandidate{
					{
						ID:             json.Number("9"),
						Alertname:      "MatrixAPIDown",
						Severity:       "P0",
						FeishuMsgID:    "om_l2",
						MentionOpenIds: []string{"ou_lead"},
						MentionPhones:  []string{"8613800138000"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			atomic.AddInt32(&markCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	feishu := &capturingFeishu{}
	voice := &fakeVoice{enabled: true}
	esc := &Escalator{
		backend:  &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:   feishu,
		voice:    voice,
		interval: time.Hour,
		stop:     make(chan struct{}),
	}
	esc.tick()
	voice.mu.Lock()
	calls := append([]string{}, voice.calls...)
	voice.mu.Unlock()
	if len(calls) != 1 || calls[0] != "8613800138000" {
		t.Fatalf("voice calls: %v", calls)
	}
	if atomic.LoadInt32(&markCount) != 1 {
		t.Fatalf("voice success must still mark_escalated; got %d", markCount)
	}
}

// severity 不在白名单时 L2 只 @ 不拨电话；mark_escalated 仍执行。
func TestEscalator_L2SkipsVoiceWhenSeverityNotAllowed(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL2: []escalationCandidate{
					{
						ID:             json.Number("21"),
						Alertname:      "MatrixAPIWarn",
						Severity:       "P1",
						FeishuMsgID:    "om_l2",
						MentionOpenIds: []string{"ou_lead"},
						MentionPhones:  []string{"8613800138000"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			atomic.AddInt32(&markCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	voice := &fakeVoice{enabled: true}
	esc := &Escalator{
		backend:         &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:          &capturingFeishu{},
		voice:           voice,
		interval:        time.Hour,
		voiceSeverities: parseVoiceSeverities("p0,critical"),
		stop:            make(chan struct{}),
	}
	esc.tick()
	voice.mu.Lock()
	calls := append([]string{}, voice.calls...)
	voice.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("P1 must NOT dial voice when allowlist=p0,critical; got %v", calls)
	}
	if atomic.LoadInt32(&markCount) != 1 {
		t.Fatalf("skip voice must still mark_escalated; got %d", markCount)
	}
}

// per-service 覆盖：某 service 显式配 none → 永不拨电话，即使 severity 是 P0。
func TestEscalator_PerServiceVoiceOverrideNone(t *testing.T) {
	esc := &Escalator{
		voiceSeverities:          parseVoiceSeverities("p0,critical"),
		voiceSeveritiesByService: parseServiceVoiceSeverities("data-platform=none"),
	}
	if esc.voiceSeverityAllowed("data-platform", "P0") {
		t.Fatal("data-platform=none should never dial, even for P0")
	}
	// 其他 service 仍走全局白名单
	if !esc.voiceSeverityAllowed("matrix-api", "P0") {
		t.Fatal("matrix-api P0 should dial via global allowlist")
	}
	if esc.voiceSeverityAllowed("matrix-api", "P1") {
		t.Fatal("matrix-api P1 should NOT dial")
	}
}

// per-service 覆盖：某 service 自定义只在 P1 拨（与全局 p0/critical 不同）。
func TestEscalator_PerServiceVoiceOverrideCustom(t *testing.T) {
	esc := &Escalator{
		voiceSeverities:          parseVoiceSeverities("p0,critical"),
		voiceSeveritiesByService: parseServiceVoiceSeverities("data-platform=p1"),
	}
	if !esc.voiceSeverityAllowed("data-platform", "P1") {
		t.Fatal("data-platform should dial on P1 per override")
	}
	if esc.voiceSeverityAllowed("data-platform", "P0") {
		t.Fatal("data-platform override=p1 should not dial on P0")
	}
}

// severity 在白名单时 L2 正常拨号（大小写不敏感）。
func TestEscalator_L2DialsWhenSeverityAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL2: []escalationCandidate{
					{
						ID:             json.Number("22"),
						Alertname:      "MatrixAPIDown",
						Severity:       "P0", // 大写也要命中小写白名单
						FeishuMsgID:    "om_l2",
						MentionOpenIds: []string{"ou_lead"},
						MentionPhones:  []string{"8613800138000"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	voice := &fakeVoice{enabled: true}
	esc := &Escalator{
		backend:         &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:          &capturingFeishu{},
		voice:           voice,
		interval:        time.Hour,
		voiceSeverities: parseVoiceSeverities("p0,critical"),
		stop:            make(chan struct{}),
	}
	esc.tick()
	voice.mu.Lock()
	calls := append([]string{}, voice.calls...)
	voice.mu.Unlock()
	if len(calls) != 1 || calls[0] != "8613800138000" {
		t.Fatalf("P0 should dial voice; got %v", calls)
	}
}

// L1(5 分钟)如果 backend 返回 assignee 手机号，且 severity 命中白名单，应立即拨负责人。
func TestEscalator_L1DialsAssigneeVoiceWhenSeverityAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL1: []escalationCandidate{
					{
						ID:             json.Number("3"),
						Alertname:      "MatrixAPIDown",
						Severity:       "P0",
						FeishuMsgID:    "om_x",
						MentionOpenIds: []string{"ou_a"},
						MentionPhones:  []string{"8613800138000"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	voice := &fakeVoice{enabled: true}
	esc := &Escalator{
		backend:         &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:          &capturingFeishu{},
		voice:           voice,
		interval:        time.Hour,
		voiceSeverities: parseVoiceSeverities("p0,critical"),
		stop:            make(chan struct{}),
	}
	esc.tick()
	voice.mu.Lock()
	defer voice.mu.Unlock()
	if len(voice.calls) != 1 || voice.calls[0] != "8613800138000" {
		t.Fatalf("L1 P0 should dial assignee voice; got calls=%v", voice.calls)
	}
}

func TestEscalator_L1SkipsVoiceWhenSeverityNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL1: []escalationCandidate{
					{
						ID:             json.Number("4"),
						Alertname:      "MatrixAPIWarn",
						Severity:       "P1",
						FeishuMsgID:    "om_x",
						MentionOpenIds: []string{"ou_a"},
						MentionPhones:  []string{"8613800138000"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	voice := &fakeVoice{enabled: true}
	esc := &Escalator{
		backend:         &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:          &capturingFeishu{},
		voice:           voice,
		interval:        time.Hour,
		voiceSeverities: parseVoiceSeverities("p0,critical"),
		stop:            make(chan struct{}),
	}
	esc.tick()
	voice.mu.Lock()
	defer voice.mu.Unlock()
	if len(voice.calls) != 0 {
		t.Fatalf("L1 P1 must NOT dial voice when allowlist=p0,critical; got calls=%v", voice.calls)
	}
}

// voice 禁用时不能阻塞 L2 主链路（@ + mark_escalated 必须正常完成）。
func TestEscalator_L2WorksWithoutVoice(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL2: []escalationCandidate{
					{ID: json.Number("11"), FeishuMsgID: "om_x", MentionOpenIds: []string{"ou_lead"}, MentionPhones: []string{"8613800138000"}},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			atomic.AddInt32(&markCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	esc := &Escalator{
		backend:  &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:   &capturingFeishu{},
		voice:    nil, // 显式 disable
		interval: time.Hour,
		stop:     make(chan struct{}),
	}
	esc.tick()
	if atomic.LoadInt32(&markCount) != 1 {
		t.Fatalf("voice nil must still mark_escalated; got %d", markCount)
	}
}

func TestEscalator_BackoffOnFailure(t *testing.T) {
	esc := &Escalator{interval: 30 * time.Second}
	if got := esc.nextInterval(); got != 30*time.Second {
		t.Fatalf("baseline want 30s, got %v", got)
	}
	esc.recordFailure()
	if got := esc.nextInterval(); got != 10*time.Second {
		t.Fatalf("after 1 failure want 10s, got %v", got)
	}
	esc.recordFailure()
	if got := esc.nextInterval(); got != 30*time.Second {
		t.Fatalf("after 2 failures want 30s, got %v", got)
	}
	esc.recordFailure()
	if got := esc.nextInterval(); got != 60*time.Second {
		t.Fatalf("after 3 failures want 60s, got %v", got)
	}
	esc.recordFailure()
	if got := esc.nextInterval(); got != 5*time.Minute {
		t.Fatalf("after 4 failures want 5m, got %v", got)
	}
	esc.recordSuccess()
	if got := esc.nextInterval(); got != 30*time.Second {
		t.Fatalf("recovered baseline want 30s, got %v", got)
	}
}

func TestEscalator_FeishuFailureKeepsCandidate(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL1: []escalationCandidate{
					{ID: json.Number("1"), FeishuMsgID: "om_x", MentionOpenIds: []string{"ou_a"}},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			atomic.AddInt32(&markCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	feishu := &capturingFeishu{err: http.ErrAbortHandler}
	esc := &Escalator{
		backend:  &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:   feishu,
		interval: time.Hour,
		stop:     make(chan struct{}),
	}
	esc.tick()
	if atomic.LoadInt32(&markCount) != 0 {
		t.Fatalf("feishu failure must NOT mark escalated, got %d", markCount)
	}
}

func TestEscalator_CandidateWithoutMsgIDStillMarks(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL1: []escalationCandidate{
					// msg_id 缺失：典型于 backend 接入前已有的旧 incident
					{ID: json.Number("99"), FeishuMsgID: "", MentionOpenIds: []string{"ou_a"}},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			atomic.AddInt32(&markCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	feishu := &capturingFeishu{}
	esc := &Escalator{
		backend:  &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:   feishu,
		interval: time.Hour,
		stop:     make(chan struct{}),
	}
	esc.tick()
	if atomic.LoadInt32(&markCount) != 1 {
		t.Fatalf("missing msg_id should still mark to avoid spam-loop, got %d", markCount)
	}
	if len(feishu.replies) != 0 {
		t.Fatalf("missing msg_id should not reply, got %d", len(feishu.replies))
	}
}

func TestEscalator_SkipServicesNoReplyStillMarks(t *testing.T) {
	var markCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/alert/v1/incidents/escalation_candidates"):
			_ = json.NewEncoder(w).Encode(listEscalationReply{
				NeedL1: []escalationCandidate{{
					ID:             json.Number("77"),
					Alertname:      "DirtyTikTokCampaign",
					Service:        "ad-delivery",
					FeishuMsgID:    "om_ad",
					MentionOpenIds: []string{"ou_a"},
				}},
				NeedL2: []escalationCandidate{{
					ID:             json.Number("78"),
					Alertname:      "DirtyTikTokAccountHour",
					Service:        "ad-delivery",
					FeishuMsgID:    "om_ad2",
					MentionOpenIds: []string{"ou_b"},
					MentionPhones:  []string{"8613800000000"},
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/mark_escalated"):
			atomic.AddInt32(&markCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	feishu := &capturingFeishu{}
	esc := &Escalator{
		backend:      &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second},
		feishu:       feishu,
		interval:     time.Hour,
		skipServices: parseEscalationSkipServices("ad-delivery"),
		stop:         make(chan struct{}),
	}
	esc.tick()
	if got := atomic.LoadInt32(&markCount); got != 2 {
		t.Fatalf("skip services should still mark L1+L2, got %d", got)
	}
	if len(feishu.replies) != 0 {
		t.Fatalf("skip services must not thread-reply, got %v", feishu.replies)
	}
}

func TestParseEscalationSkipServices(t *testing.T) {
	m := parseEscalationSkipServices("ad-delivery, Foo;BAR")
	if len(m) != 3 || m["ad-delivery"] == struct{}{} && m["foo"] == struct{}{} && m["bar"] == struct{}{} {
		// presence check
	}
	for _, k := range []string{"ad-delivery", "foo", "bar"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %s in %#v", k, m)
		}
	}
	if parseEscalationSkipServices("") != nil {
		t.Fatal("empty should be nil")
	}
}
