package main

import (
	"testing"
	"time"
)

// Argument parsing only: no database, no filesystem, no HOME.

func TestParseMergeArgsAcceptsEitherOrder(t *testing.T) {
	for _, args := range [][]string{
		{"Motoloro2", "--into", "Motoloro"},
		{"--into", "Motoloro", "Motoloro2"},
		{"Motoloro2", "--into=Motoloro"},
	} {
		src, into, err := parseMergeArgs(args)
		if err != nil {
			t.Errorf("parseMergeArgs(%v) = error %v, want success", args, err)
			continue
		}
		if src != "Motoloro2" || into != "Motoloro" {
			t.Errorf("parseMergeArgs(%v) = %q, %q; want Motoloro2, Motoloro", args, src, into)
		}
	}
}

func TestParseMergeArgsRejectsIncomplete(t *testing.T) {
	for _, args := range [][]string{
		{"Motoloro2"},
		{"--into", "Motoloro"},
		{},
		{""},
	} {
		src, into, err := parseMergeArgs(args)
		if err == nil {
			t.Errorf("parseMergeArgs(%v) = %q, %q; want an error", args, src, into)
		}
	}
}

func TestParseMergeArgsRejectsSelfMerge(t *testing.T) {
	if _, _, err := parseMergeArgs([]string{"Same", "--into", "Same"}); err == nil {
		t.Error("merging a network into itself should be rejected")
	}
}

// os.Exit here would kill the test binary instead of failing one test.
func TestParseMergeArgsRejectsUnknownFlag(t *testing.T) {
	if _, _, err := parseMergeArgs([]string{"A", "--nope", "B"}); err == nil {
		t.Error("unknown flag should be rejected")
	}
}

func TestPluralDays(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{1, "1 day"},
		{2, "2 days"},
		{7, "7 days"},
		{30, "30 days"},
	} {
		if got := pluralDays(tc.n); got != tc.want {
			t.Errorf("pluralDays(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestWindowsFor(t *testing.T) {
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.Local)
	got := windowsFor(now, 1)
	if len(got) != 3 {
		t.Fatalf("got %d windows, want 3", len(got))
	}
	if got[1].Name != "1 day" {
		t.Errorf("window 1 = %q, want %q", got[1].Name, "1 day")
	}
	if !got[2].From.IsZero() {
		t.Error("all time window should be unbounded")
	}
}
