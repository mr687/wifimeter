package main

import (
	"strings"
	"testing"
	"time"
)

func TestDiscardRate(t *testing.T) {
	cases := []struct {
		total, discarded int
		want             float64
	}{
		{0, 0, 0},
		{100, 0, 0},
		{100, 1, 1},
		{8, 3, 37.5},
	}
	for _, tc := range cases {
		got := DBStats{Total: tc.total, Discarded: tc.discarded}.DiscardRate()
		if got != tc.want {
			t.Errorf("rate(%d, %d) = %v, want %v", tc.total, tc.discarded, got, tc.want)
		}
	}
}

func TestAgentPIDParses(t *testing.T) {
	if got := pidRe.FindStringSubmatch(`"PID" = 4242;`); len(got) != 2 || got[1] != "4242" {
		t.Errorf("pidRe = %v", got)
	}
	// A label launchd does not have prints no PID line at all.
	if got := pidRe.FindStringSubmatch(`could not find service`); got != nil {
		t.Errorf("expected no match, got %v", got)
	}
}

func TestWriteDBStatsEmpty(t *testing.T) {
	var sb strings.Builder
	if err := writeDBStats(&sb, DBStats{}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	for _, want := range []string{"samples", "none", "not loaded"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty stats missing %q:\n%s", want, out)
		}
	}
	// With no samples there is no rate to print.
	if strings.Contains(out, "discarded") {
		t.Errorf("empty stats should not report a rate:\n%s", out)
	}
}

func TestWriteDBStatsPopulated(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	s := DBStats{
		Total:     8,
		Discarded: 3,
		Networks:  2,
		Oldest:    now.AddDate(0, 0, -1),
		Newest:    now,
		SizeBytes: 4 << 20,
		AgentPID:  4242,
	}

	var sb strings.Builder
	if err := writeDBStats(&sb, s); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	for _, want := range []string{"8", "3 (37.5%)", "4242", "4.0 MB", "2026-09-09"} {
		if !strings.Contains(out, want) {
			t.Errorf("populated stats missing %q:\n%s", want, out)
		}
	}

	if !strings.Contains(out, "warning") {
		t.Errorf("expected a warning at 37.5%%:\n%s", out)
	}
}

func TestWriteDBStatsQuietWarning(t *testing.T) {
	s := DBStats{Total: 100, Discarded: 1, AgentPID: 7}
	var sb strings.Builder
	if err := writeDBStats(&sb, s); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "warning") {
		t.Errorf("unexpected warning at 1%%:\n%s", sb.String())
	}
}
