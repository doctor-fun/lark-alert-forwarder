package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	defaultAlertSilenceDuration = 30 * time.Minute
	maxP0AlertSilenceDuration   = 24 * time.Hour
	maxAlertSilenceDuration     = 7 * 24 * time.Hour
)

type alertSilenceDurationOption struct {
	Label    string
	Duration string
}

type alertSilence struct {
	Fingerprint    string
	AlertName      string
	Service        string
	Env            string
	Severity       string
	OperatorOpenID string
	Reason         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type alertSilenceMatch struct {
	Fingerprint string
	AlertName   string
	Service     string
	Env         string
	Severity    string
}

type alertSilenceStore struct {
	mu    sync.Mutex
	now   func() time.Time
	items map[string]alertSilence
}

func newAlertSilenceStore() *alertSilenceStore {
	return &alertSilenceStore{
		now:   time.Now,
		items: make(map[string]alertSilence),
	}
}

func (s *alertSilenceStore) Put(item alertSilence) (alertSilence, error) {
	if s == nil {
		return alertSilence{}, errors.New("alert silence store is nil")
	}
	item.Fingerprint = strings.TrimSpace(item.Fingerprint)
	if item.Fingerprint == "" {
		return alertSilence{}, errors.New("missing fingerprint")
	}
	now := s.currentTime()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	if !item.ExpiresAt.After(now) {
		return alertSilence{}, errors.New("silence expiry must be in the future")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]alertSilence)
	}
	s.items[item.Fingerprint] = item
	return item, nil
}

func (s *alertSilenceStore) Match(fingerprint string) (alertSilence, bool) {
	if s == nil {
		return alertSilence{}, false
	}
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return alertSilence{}, false
	}
	now := s.currentTime()
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[fp]
	if !ok {
		return alertSilence{}, false
	}
	if !item.ExpiresAt.After(now) {
		delete(s.items, fp)
		return alertSilence{}, false
	}
	return item, true
}

func (s *alertSilenceStore) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func alertSilenceMatchFromPayload(payload grafanaWebhook) alertSilenceMatch {
	first := grafanaAlert{}
	if len(payload.Alerts) > 0 {
		first = payload.Alerts[0]
	}
	alertName := firstNonEmpty(payload.CommonLabels["alertname"], first.Labels["alertname"], payload.Title, "Grafana Alert")
	service := firstNonEmpty(payload.CommonLabels["service"], first.Labels["service"], "-")
	env := firstNonEmpty(payload.CommonLabels["env"], payload.CommonLabels["namespace"], first.Labels["env"], first.Labels["namespace"], "-")
	severity := firstNonEmpty(payload.CommonLabels["severity"], first.Labels["severity"], "-")
	return alertSilenceMatch{
		Fingerprint: computeFingerprint(alertName, service, env, mergedAlertLabels(payload)),
		AlertName:   alertName,
		Service:     service,
		Env:         env,
		Severity:    severity,
	}
}

func normalizeAlertSilenceDuration(severity, raw string) (time.Duration, bool) {
	duration := defaultAlertSilenceDuration
	if parsed, err := time.ParseDuration(strings.TrimSpace(raw)); err == nil && parsed > 0 {
		duration = parsed
	}
	maxDuration := maxAlertSilenceDuration
	if strings.EqualFold(strings.TrimSpace(severity), "P0") {
		maxDuration = maxP0AlertSilenceDuration
	}
	if duration > maxDuration {
		return maxDuration, true
	}
	return duration, false
}

func alertSilenceDurationOptions(severity string) []alertSilenceDurationOption {
	if strings.EqualFold(strings.TrimSpace(severity), "P0") {
		return []alertSilenceDurationOption{
			{Label: "屏蔽10分钟", Duration: "10m"},
			{Label: "屏蔽30分钟", Duration: "30m"},
			{Label: "屏蔽4小时", Duration: "4h"},
			{Label: "屏蔽6小时", Duration: "6h"},
			{Label: "屏蔽12小时", Duration: "12h"},
			{Label: "屏蔽24小时", Duration: "24h"},
		}
	}
	return []alertSilenceDurationOption{
		{Label: "屏蔽30分钟", Duration: "30m"},
		{Label: "屏蔽2小时", Duration: "2h"},
		{Label: "屏蔽4小时", Duration: "4h"},
		{Label: "屏蔽12小时", Duration: "12h"},
		{Label: "屏蔽24小时", Duration: "24h"},
		{Label: "屏蔽2天", Duration: "48h"},
		{Label: "屏蔽7天", Duration: "168h"},
	}
}

func formatAlertSilenceDuration(d time.Duration) string {
	if d <= 0 {
		return "0分钟"
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d天", int(d/(24*time.Hour)))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d小时", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d分钟", int(d/time.Minute))
	}
	return d.String()
}
