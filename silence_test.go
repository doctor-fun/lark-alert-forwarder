package main

import (
	"testing"
	"time"
)

func TestNormalizeAlertSilenceDurationCapsP0(t *testing.T) {
	d, capped := normalizeAlertSilenceDuration("P0", "48h")
	if d != 24*time.Hour || !capped {
		t.Fatalf("P0 duration=%s capped=%v, want 24h/true", d, capped)
	}
}

func TestNormalizeAlertSilenceDurationAllowsP0LongWindow(t *testing.T) {
	d, capped := normalizeAlertSilenceDuration("P0", "12h")
	if d != 12*time.Hour || capped {
		t.Fatalf("P0 duration=%s capped=%v, want 12h/false", d, capped)
	}
}

func TestNormalizeAlertSilenceDurationCapsNonP0(t *testing.T) {
	d, capped := normalizeAlertSilenceDuration("P2", "240h")
	if d != 7*24*time.Hour || !capped {
		t.Fatalf("P2 duration=%s capped=%v, want 168h/true", d, capped)
	}
}

func TestNormalizeAlertSilenceDurationAllowsNonP0DayWindow(t *testing.T) {
	d, capped := normalizeAlertSilenceDuration("P2", "168h")
	if d != 7*24*time.Hour || capped {
		t.Fatalf("P2 duration=%s capped=%v, want 168h/false", d, capped)
	}
}

func TestAlertSilenceDurationOptions(t *testing.T) {
	p0 := alertSilenceDurationOptions("P0")
	if len(p0) != 6 ||
		p0[0].Duration != "10m" ||
		p0[1].Duration != "30m" ||
		p0[2].Duration != "4h" ||
		p0[3].Duration != "6h" ||
		p0[4].Duration != "12h" ||
		p0[5].Duration != "24h" {
		t.Fatalf("P0 options = %#v", p0)
	}
	p1 := alertSilenceDurationOptions("P1")
	if len(p1) != 7 ||
		p1[0].Duration != "30m" ||
		p1[1].Duration != "2h" ||
		p1[2].Duration != "4h" ||
		p1[3].Duration != "12h" ||
		p1[4].Duration != "24h" ||
		p1[5].Duration != "48h" ||
		p1[6].Duration != "168h" {
		t.Fatalf("P1 options = %#v", p1)
	}
}

func TestFormatAlertSilenceDurationUsesDays(t *testing.T) {
	if got := formatAlertSilenceDuration(48 * time.Hour); got != "2天" {
		t.Fatalf("format 48h = %q, want 2天", got)
	}
	if got := formatAlertSilenceDuration(7 * 24 * time.Hour); got != "7天" {
		t.Fatalf("format 168h = %q, want 7天", got)
	}
}
