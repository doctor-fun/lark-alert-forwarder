package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

const userFeedbackOncallSnapshotFreshness = 5 * time.Second

type userFeedbackOncallAssignee struct {
	Role   string
	OpenID string
	Name   string
}

type userFeedbackOncallRuntimeConfig struct {
	Enabled     bool
	ChatID      string
	ReplyPrefix string
	MentionTTL  time.Duration
	Assignees   []userFeedbackOncallAssignee
	Source      string
}

type userFeedbackOncallRefreshState struct {
	mu       sync.Mutex
	inFlight bool
	done     chan struct{}
	result   bool
}

var userFeedbackOncallRefreshStates sync.Map

func userFeedbackOncallRefreshStateFor(s *server) *userFeedbackOncallRefreshState {
	state, _ := userFeedbackOncallRefreshStates.LoadOrStore(s, &userFeedbackOncallRefreshState{})
	return state.(*userFeedbackOncallRefreshState)
}

func (s *server) reloadUserFeedbackOncallFromBackend(ctx context.Context) bool {
	return s.refreshUserFeedbackOncallFromBackend(ctx, time.Time{}, 0, true)
}

func (s *server) refreshUserFeedbackOncallFromBackendIfStale(ctx context.Context, now time.Time) bool {
	return s.refreshUserFeedbackOncallFromBackend(ctx, now, userFeedbackOncallSnapshotFreshness, false)
}

func (s *server) refreshUserFeedbackOncallFromBackend(
	ctx context.Context,
	now time.Time,
	freshness time.Duration,
	force bool,
) bool {
	if s == nil || s.backend == nil {
		return false
	}
	state := userFeedbackOncallRefreshStateFor(s)
	state.mu.Lock()
	if !force && s.userFeedbackOncallSnapshotIsFresh(now, freshness) {
		state.mu.Unlock()
		return true
	}
	if state.inFlight {
		done := state.done
		state.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-done:
			state.mu.Lock()
			result := state.result
			state.mu.Unlock()
			return result
		}
	}
	state.inFlight = true
	state.done = make(chan struct{})
	state.mu.Unlock()

	success := s.loadUserFeedbackOncallFromBackend(ctx)

	state.mu.Lock()
	state.result = success
	state.inFlight = false
	close(state.done)
	state.mu.Unlock()
	return success
}

func (s *server) userFeedbackOncallSnapshotIsFresh(now time.Time, freshness time.Duration) bool {
	if freshness <= 0 {
		return false
	}
	s.userFeedbackOncallSnapshotMu.RLock()
	snapshotAt := s.userFeedbackOncallSnapshotAt
	s.userFeedbackOncallSnapshotMu.RUnlock()
	return !snapshotAt.IsZero() && now.Sub(snapshotAt) <= freshness
}

func (s *server) loadUserFeedbackOncallFromBackend(ctx context.Context) bool {
	reply, err := s.backend.GetFeishuFeedbackOncallSnapshot(ctx)
	if err != nil {
		log.Printf("user-feedback oncall snapshot reload failed: %v", err)
		return false
	}
	cfg := userFeedbackOncallRuntimeConfig{
		Enabled:     reply.Enabled,
		ChatID:      strings.TrimSpace(reply.ChatID),
		ReplyPrefix: strings.TrimSpace(reply.ReplyPrefix),
		Source:      "backend",
	}
	if ttl, err := reply.MentionTTLSeconds.Int64(); err == nil && ttl > 0 {
		cfg.MentionTTL = time.Duration(ttl) * time.Second
	}
	for _, a := range reply.Assignees {
		openID := strings.TrimSpace(a.OpenID)
		if openID == "" {
			continue
		}
		cfg.Assignees = append(cfg.Assignees, userFeedbackOncallAssignee{
			Role:   strings.TrimSpace(a.Role),
			OpenID: openID,
			Name:   strings.TrimSpace(a.Name),
		})
	}
	if cfg.ReplyPrefix == "" {
		cfg.ReplyPrefix = defaultUserFeedbackOncallReplyPrefix
	}
	if cfg.MentionTTL <= 0 {
		cfg.MentionTTL = defaultUserFeedbackOncallMentionTTL
	}
	if cfg.ChatID == "" {
		cfg.ChatID = defaultUserFeedbackOncallChatID
	}
	s.userFeedbackOncallSnapshotMu.Lock()
	s.userFeedbackOncallSnapshot = &cfg
	s.userFeedbackOncallSnapshotAt = time.Now()
	s.userFeedbackOncallSnapshotMu.Unlock()
	log.Printf("user-feedback oncall snapshot loaded chat=%s enabled=%v assignees=%d",
		cfg.ChatID, cfg.Enabled, len(cfg.Assignees))
	return true
}

func (s *server) startUserFeedbackOncallReloader() func() {
	if s == nil || s.backend == nil {
		return func() {}
	}
	interval := durationFromEnv("USER_FEEDBACK_ONCALL_RELOAD_INTERVAL", 5*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reloadUserFeedbackOncallFromBackend(context.Background())
			}
		}
	}()
	return cancel
}

func (s *server) effectiveUserFeedbackOncallConfig(now time.Time) (userFeedbackOncallRuntimeConfig, bool) {
	if s == nil {
		return userFeedbackOncallRuntimeConfig{}, false
	}
	s.userFeedbackOncallSnapshotMu.RLock()
	snap := s.userFeedbackOncallSnapshot
	s.userFeedbackOncallSnapshotMu.RUnlock()
	if snap != nil {
		if !snap.Enabled || len(snap.Assignees) == 0 {
			return userFeedbackOncallRuntimeConfig{}, false
		}
		return *snap, true
	}
	candidates := splitOpenIDList(strings.Join(s.userFeedbackOncallCandidates, ","))
	if len(candidates) == 0 {
		return userFeedbackOncallRuntimeConfig{}, false
	}
	assignee := pickUserFeedbackOncallAssignee(candidates, now)
	if assignee == "" {
		return userFeedbackOncallRuntimeConfig{}, false
	}
	ttl := s.userFeedbackOncallMentionTTL
	if ttl <= 0 {
		ttl = defaultUserFeedbackOncallMentionTTL
	}
	chatID := strings.TrimSpace(s.userFeedbackOncallChatID)
	if chatID == "" {
		chatID = defaultUserFeedbackOncallChatID
	}
	replyPrefix := strings.TrimSpace(s.userFeedbackOncallReplyPrefix)
	if replyPrefix == "" {
		replyPrefix = defaultUserFeedbackOncallReplyPrefix
	}
	return userFeedbackOncallRuntimeConfig{
		Enabled:     true,
		ChatID:      chatID,
		ReplyPrefix: replyPrefix,
		MentionTTL:  ttl,
		Assignees: []userFeedbackOncallAssignee{{
			Role:   "backend",
			OpenID: assignee,
		}},
		Source: "env",
	}, true
}

func formatUserFeedbackOncallAssignees(assignees []userFeedbackOncallAssignee) string {
	parts := make([]string, 0, len(assignees))
	for _, a := range assignees {
		label := userFeedbackOncallAssigneeLabel(a)
		if label != "" {
			parts = append(parts, label)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "值班同学：" + strings.Join(parts, "、")
}

func userFeedbackOncallAssigneeLabel(a userFeedbackOncallAssignee) string {
	if label := strings.TrimSpace(a.Name); label != "" {
		return label
	}
	switch strings.TrimSpace(a.Role) {
	case "qa":
		return "测试值班"
	case "frontend":
		return "大前端值班"
	case "backend":
		return "后端值班"
	default:
		return "值班人"
	}
}

func buildUserFeedbackOncallReply(prefix string, assignees []userFeedbackOncallAssignee) string {
	prefix = strings.TrimSpace(prefix)
	mentions := formatUserFeedbackOncallAssignees(assignees)
	if prefix == "" {
		return mentions
	}
	if mentions == "" {
		return prefix
	}
	return prefix + " " + mentions
}
