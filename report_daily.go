package main

// Daily breakdown: one network's usage bucketed by local day.
//
// Pure functions over a slice of samples, so tests run on fixtures without
// opening the database.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// DayTotal is one local day's usage.
type DayTotal struct {
	Day time.Time
	RX  uint64
	TX  uint64
}

// Daily buckets valid samples by local day, oldest first. Days with no traffic
// stay as zeroes, so a quiet weekend reads as a gap instead of dropping out and
// making the remaining days look denser than they were.
func Daily(samples []Sample, days int, now time.Time) []DayTotal {
	if days < 1 {
		days = 1
	}
	loc := now.Location()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := midnight.AddDate(0, 0, -(days - 1))

	out := make([]DayTotal, days)
	index := make(map[string]int, days)
	for i := range out {
		out[i].Day = start.AddDate(0, 0, i)
		index[out[i].Day.Format(dayKey)] = i
	}

	for _, s := range samples {
		if !s.Valid {
			continue
		}
		local := s.At.In(loc)
		key := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).Format(dayKey)
		if i, ok := index[key]; ok {
			out[i].RX += s.RX
			out[i].TX += s.TX
		}
	}
	return out
}

const dayKey = "2006-01-02"

// DailySummary is peak day, median day, and busiest 7-day stretch. Peak and
// median are download bytes, matching what the report sorts on.
type DailySummary struct {
	Peak        DayTotal
	Median      uint64
	BusiestWeek uint64
}

// SummarizeDaily reports peak day, median day, and the busiest 7-day stretch.
// Median covers zero days too; dropping them would flatter the series.
func SummarizeDaily(dayTotals []DayTotal) DailySummary {
	var s DailySummary
	if len(dayTotals) == 0 {
		return s
	}

	values := make([]uint64, len(dayTotals))
	for i, d := range dayTotals {
		values[i] = d.RX
		if d.RX >= s.Peak.RX {
			s.Peak = d
		}
	}

	sorted := append([]uint64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if n := len(sorted); n%2 == 1 {
		s.Median = sorted[n/2]
	} else {
		s.Median = (sorted[n/2-1] + sorted[n/2]) / 2
	}

	// Slide rather than chopping into calendar weeks, so a stretch spanning
	// two of them still counts.
	for i := 0; i+7 <= len(dayTotals); i++ {
		var sum uint64
		for _, d := range dayTotals[i : i+7] {
			sum += d.RX
		}
		if sum > s.BusiestWeek {
			s.BusiestWeek = sum
		}
	}
	if s.BusiestWeek == 0 {
		for _, d := range dayTotals {
			s.BusiestWeek += d.RX
		}
	}
	return s
}

const barWidth = 10

// WriteDaily renders the breakdown. Bars scale to the peak day rather than the
// total, so an ordinary day still shows something instead of collapsing to one
// block.
func WriteDaily(w io.Writer, name string, dayTotals []DayTotal, sum DailySummary) error {
	var total uint64
	for _, d := range dayTotals {
		total += d.RX
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s — last %d days (total ↓ %s)\n\n", name, len(dayTotals), humanBytes(total))
	for _, d := range dayTotals {
		fmt.Fprintf(&b, "%s  %s  %s\n",
			d.Day.Format("Mon 02"), bar(d.RX, sum.Peak.RX, barWidth), humanBytes(d.RX))
	}

	fmt.Fprintf(&b, "\npeak day %s on %s   median %s   busiest week %s\n",
		humanBytes(sum.Peak.RX), sum.Peak.Day.Format("Mon 02"),
		humanBytes(sum.Median), humanBytes(sum.BusiestWeek))

	_, err := io.WriteString(w, b.String())
	return err
}

func bar(value, max uint64, width int) string {
	if max == 0 {
		return strings.Repeat("░", width)
	}
	filled := int(float64(value) / float64(max) * float64(width))
	if filled < 0 {
		filled = 0
	} else if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}
