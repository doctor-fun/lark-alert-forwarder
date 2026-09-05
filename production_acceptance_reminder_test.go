package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseProductionAcceptanceOwners(t *testing.T) {
	owners := parseProductionAcceptanceOwners("Dave|ou_he, Eve|ou_fu, skip, |missing")
	if len(owners) != 2 || owners[0].Name != "Dave" || owners[1].OpenID != "ou_fu" {
		t.Fatalf("owners=%+v", owners)
	}
	if parseProductionAcceptanceOwners("") != nil {
		t.Fatal("expected empty env to yield no owners")
	}
}

func newProductionAcceptanceReminderForTest() *productionAcceptanceReminder {
	return &productionAcceptanceReminder{
		cfg: productionAcceptanceReminderConfig{
			ChatID: "oc_acceptance",
			Owners: []productionAcceptanceReminderOwner{
				{Name: "Dave", OpenID: "ou_he"},
				{Name: "Eve", OpenID: "ou_fu"},
			},
		},
	}
}

func TestProductionAcceptanceWindowThursdayBoundary(t *testing.T) {
	loc := chinaLocation()
	tests := []struct {
		now       time.Time
		wantStart string
		wantEnd   string
	}{
		{
			now:       time.Date(2026, 8, 13, 8, 59, 0, 0, loc),
			wantStart: "2026-08-06 09:00",
			wantEnd:   "2026-08-13 09:00",
		},
		{
			now:       time.Date(2026, 8, 13, 9, 0, 0, 0, loc),
			wantStart: "2026-08-13 09:00",
			wantEnd:   "2026-08-20 09:00",
		},
		{
			now:       time.Date(2026, 8, 20, 9, 0, 0, 0, loc),
			wantStart: "2026-08-20 09:00",
			wantEnd:   "2026-08-27 09:00",
		},
	}
	for _, tt := range tests {
		start, end, active := productionAcceptanceWindow(tt.now)
		if !active {
			t.Fatalf("window should be active at %s", tt.now)
		}
		if got := start.Format("2006-01-02 15:04"); got != tt.wantStart {
			t.Fatalf("start=%s, want %s", got, tt.wantStart)
		}
		if got := end.Format("2006-01-02 15:04"); got != tt.wantEnd {
			t.Fatalf("end=%s, want %s", got, tt.wantEnd)
		}
	}
}

func TestProductionAcceptanceReminderSendsOncePerDayAndResetsWindow(t *testing.T) {
	r := newProductionAcceptanceReminderForTest()
	loc := chinaLocation()
	thursday := time.Date(2026, 8, 13, 9, 0, 0, 0, loc)

	if window, send := r.prepareSend(thursday); !send || window != "2026-08-13" {
		t.Fatalf("first send: window=%q send=%v", window, send)
	}
	if _, send := r.prepareSend(thursday.Add(8 * time.Hour)); send {
		t.Fatal("same day must not send twice")
	}
	if _, send := r.prepareSend(thursday.AddDate(0, 0, 1)); !send {
		t.Fatal("next day should send once")
	}
	if _, send := r.prepareSend(thursday.AddDate(0, 0, 7)); !send {
		t.Fatal("new Thursday window should reset daily send state")
	}
}

func TestProductionAcceptanceReminderOnlyOwnerStopsGlobally(t *testing.T) {
	r := newProductionAcceptanceReminderForTest()
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, chinaLocation())
	window, send := r.prepareSend(now)
	if !send {
		t.Fatal("reminder should send in active window")
	}
	if ok, _ := r.stop(now, window, "ou_other", true); ok {
		t.Fatal("non-owner must not stop global reminder")
	}
	if _, send := r.prepareSend(now.AddDate(0, 0, 1)); !send {
		t.Fatal("non-owner action must not stop following daily reminder")
	}
	if ok, reason := r.stop(now, window, "ou_he", false); !ok {
		t.Fatalf("owner should stop global reminder: %s", reason)
	}
	_, stopped, completed, operator := r.snapshot(now)
	if !stopped || completed || operator != "ou_he" {
		t.Fatalf("unexpected stop state: stopped=%v completed=%v operator=%q", stopped, completed, operator)
	}
	if _, send := r.prepareSend(now.AddDate(0, 0, 2)); send {
		t.Fatal("owner global suppression must stop remaining window reminders")
	}
}

func TestProductionAcceptanceReminderCardAndActionUpdate(t *testing.T) {
	r := newProductionAcceptanceReminderForTest()
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, chinaLocation())
	window, send := r.prepareSend(now)
	if !send {
		t.Fatal("reminder should be active")
	}
	card := buildProductionAcceptanceReminderCard(r.cfg, window, false, false, "")
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	content := string(raw)
	for _, expected := range []string{
		"生产验收已完成",
		"本轮无需生产验收提醒",
		productionAcceptanceCompleteAction,
		productionAcceptanceSuppressAction,
		"ou_he",
		"ou_fu",
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("card missing %q: %s", expected, content)
		}
	}

	s := &server{productionAcceptanceReminder: r}
	event := feishuCardActionEvent{}
	event.Operator.OpenID = "ou_fu"
	event.Action.Value = map[string]string{
		"action": productionAcceptanceCompleteAction,
		"window": window,
	}
	reply, err := s.handleProductionAcceptanceReminderActionAt(now, event)
	if err != nil {
		t.Fatalf("handle action: %v", err)
	}
	if reply.Toast == nil || reply.Toast.Type != "success" || reply.Card == nil || reply.Card.Data.Header.Title.Content != "生产验收提醒" {
		t.Fatalf("unexpected callback response: %+v", reply)
	}
	if reply.Card.Data.Config["wide_screen_mode"] != true {
		t.Fatalf("callback card must retain legacy compatible format: %+v", reply.Card.Data.Config)
	}
	updated, err := json.Marshal(reply.Card.Data)
	if err != nil {
		t.Fatalf("marshal updated card: %v", err)
	}
	if !strings.Contains(string(updated), "已完成") || strings.Contains(string(updated), productionAcceptanceCompleteAction) {
		t.Fatalf("completed card must show status and remove callbacks: %s", updated)
	}
}

func TestProductionAcceptanceReminderActionRejectsNonOwner(t *testing.T) {
	r := newProductionAcceptanceReminderForTest()
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, chinaLocation())
	window, _ := r.prepareSend(now)
	s := &server{productionAcceptanceReminder: r}
	event := feishuCardActionEvent{}
	event.Operator.OpenID = "ou_other"
	event.Action.Value = map[string]string{
		"action": productionAcceptanceSuppressAction,
		"window": window,
	}
	reply, err := s.handleProductionAcceptanceReminderActionAt(now, event)
	if err != nil {
		t.Fatalf("handle action: %v", err)
	}
	if reply.Toast == nil || reply.Toast.Type != "warning" || !strings.Contains(reply.Toast.Content, "仅Dave或Eve") {
		t.Fatalf("non-owner response=%+v", reply)
	}
}

func TestProductionAcceptanceReminderActionsDispatch(t *testing.T) {
	r := newProductionAcceptanceReminderForTest()
	now := time.Now()
	window, _ := r.prepareSend(now)
	s := &server{productionAcceptanceReminder: r}
	event := feishuCardActionEvent{}
	event.Operator.OpenID = "ou_he"
	event.Action.Value = map[string]string{
		"action": productionAcceptanceSuppressAction,
		"window": window,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	reply, err := s.handleCardAction(context.Background(), raw)
	if err != nil {
		t.Fatalf("dispatch action: %v", err)
	}
	if reply.Toast == nil {
		t.Fatal("production acceptance action was not dispatched")
	}
}
