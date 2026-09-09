package main

import (
	"strings"
	"testing"
	"time"
)

// Every fixture here is built in memory. Nothing opens a database, so these
// cannot touch real usage data.

func dailyFixtures(now time.Time) []Sample {
	hot := "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"
	quiet := "198.51.100.1|255.255.255.0|00:00:5e:00:53:02"

	day := func(offset int) time.Time {
		return now.AddDate(0, 0, -offset).Add(3 * time.Hour)
	}

	return []Sample{
		// heaviest network, spread over the last four days
		{At: day(0), FPKey: hot, RX: 700 << 20, TX: 10 << 20, Valid: true},
		{At: day(1), FPKey: hot, RX: 3 << 30, TX: 40 << 20, Valid: true},
		{At: day(2), FPKey: hot, RX: 900 << 20, TX: 20 << 20, Valid: true},
		{At: day(3), FPKey: hot, RX: 500 << 20, TX: 5 << 20, Valid: true},

		// a lighter network, to prove HeaviestKey picks the other one
		{At: day(0), FPKey: quiet, RX: 10 << 20, TX: 1 << 20, Valid: true},

		// discarded, and must not appear in any daily figure
		{At: day(1), FPKey: hot, RX: 99 << 30, TX: 99 << 30, Valid: false, Reason: ReasonSleep},
	}
}

func TestDaily(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	hot := "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"
	got := Daily(filterKey(dailyFixtures(now), hot), 7, now)

	if len(got) != 7 {
		t.Fatalf("got %d days, want 7", len(got))
	}

	// Oldest first, so the series reads left to right like a calendar.
	if !got[0].Day.Before(got[len(got)-1].Day) {
		t.Fatalf("days not ascending: %v then %v", got[0].Day, got[len(got)-1].Day)
	}
	if got[6].Day.Day() != 9 {
		t.Errorf("last day = %v, want the 9th", got[6].Day)
	}

	// Days with no traffic survive as zeroes rather than being dropped.
	for _, i := range []int{0, 1, 2} {
		if got[i].RX != 0 {
			t.Errorf("day %v rx = %d, want 0", got[i].Day, got[i].RX)
		}
	}

	// Newest four days, oldest first: 500 MiB, 900 MiB, 3 GiB, 700 MiB.
	want := []uint64{500 << 20, 900 << 20, 3 << 30, 700 << 20}
	for i, w := range want {
		if got[3+i].RX != w {
			t.Errorf("day %v rx = %d, want %d", got[3+i].Day, got[3+i].RX, w)
		}
	}

	// The 99 GiB discarded sample must not leak into any day.
	for _, d := range got {
		if d.RX > 4<<30 {
			t.Errorf("discarded sample leaked into %v: %d bytes", d.Day, d.RX)
		}
	}
}

// filterKey narrows samples to one network, the way the CLI will before
// breaking a single network down by day.
func filterKey(samples []Sample, key string) []Sample {
	var out []Sample
	for _, s := range samples {
		if s.FPKey == key {
			out = append(out, s)
		}
	}
	return out
}

func TestSummarizeDaily(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	hot := "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"
	got := SummarizeDaily(Daily(filterKey(dailyFixtures(now), hot), 7, now))

	if got.Peak.RX != 3<<30 {
		t.Errorf("peak = %d, want %d", got.Peak.RX, 3<<30)
	}
	if got.BusiestWeek < 3<<30 {
		t.Errorf("busiest week = %d, should include the peak day", got.BusiestWeek)
	}
	// Seven days: three quiet, then 500 MiB, 900 MiB, 3 GiB, 700 MiB.
	if got.Median != 500<<20 {
		t.Errorf("median = %d, want %d", got.Median, 500<<20)
	}
}

func TestSummarizeDailyShortRange(t *testing.T) {
	// Fewer than seven days: the fallback sums what there is instead of
	// reporting a zero week.
	totals := []DayTotal{
		{Day: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC), RX: 5 << 30},
		{Day: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), RX: 2 << 30},
	}
	got := SummarizeDaily(totals)
	if got.BusiestWeek != 7<<30 {
		t.Errorf("busiest week = %d, want %d", got.BusiestWeek, 7<<30)
	}
	if got.Peak.RX != 5<<30 {
		t.Errorf("peak = %d, want %d", got.Peak.RX, 5<<30)
	}
}

func TestWriteDaily(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	dayTotals := Daily(dailyFixtures(now), 7, now)

	var sb strings.Builder
	if err := WriteDaily(&sb, "Phone hotspot", dayTotals, SummarizeDaily(dayTotals)); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	for _, want := range []string{"Phone hotspot", "peak day", "median", "busiest week", "█"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "\n") < 10 {
		t.Errorf("output too short:\n%s", out)
	}
	// One line per day, plus the header, blank line, and summary.
	if lines := strings.Count(out, "\n"); lines != 7+4 {
		t.Errorf("got %d lines, want %d:\n%s", lines, 7+4, out)
	}
}

func TestHeaviestKey(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	got := HeaviestKey(dailyFixtures(now))
	want := "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"
	if got != want {
		t.Errorf("HeaviestKey = %q, want %q", got, want)
	}
	if HeaviestKey(nil) != "" {
		t.Error("no samples should give an empty key")
	}
}

func TestBar(t *testing.T) {
	if got := bar(0, 100, 10); got != "░░░░░░░░░░" {
		t.Errorf("zero = %q", got)
	}
	if got := bar(100, 100, 10); got != "██████████" {
		t.Errorf("full = %q", got)
	}
	// No maximum means nothing to scale against.
	if got := bar(5, 0, 4); got != "░░░░" {
		t.Errorf("no max = %q", got)
	}
	// A value above the maximum must not overflow the bar.
	if got := bar(200, 100, 4); got != "████" {
		t.Errorf("overflow = %q", got)
	}
}
