package main

// Report rendering. Rollups are computed at query time: at ten second sampling
// that is around 8.6k rows a day, which SQLite walks comfortably, so there is
// no aggregation table to keep in sync.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Window is a named time range to total up. A zero From means unbounded.
type Window struct {
	Name string
	From time.Time
}

// Row is one line of the report: one network, one totals pair per window.
type Row struct {
	Key     string
	Label   string
	Windows []uint64 // alternating rx, tx
}

type Report struct {
	Windows        []Window
	Rows           []Row
	Discarded      int
	Total          int
	DiscardReasons map[Reason]int
}

// Aggregate folds samples into per-network totals. Invalid samples are counted
// for the footer rather than summed.
func Aggregate(samples []Sample, windows []Window, labels map[string]string) Report {
	rep := Report{
		Windows:        windows,
		DiscardReasons: make(map[Reason]int),
	}
	byKey := make(map[string]*Row)

	for _, s := range samples {
		rep.Total++
		if !s.Valid {
			rep.Discarded++
			rep.DiscardReasons[s.Reason]++
			continue
		}
		row, ok := byKey[s.FPKey]
		if !ok {
			row = &Row{Key: s.FPKey, Label: labels[s.FPKey], Windows: make([]uint64, 2*len(windows))}
			byKey[s.FPKey] = row
		}
		for i, w := range windows {
			if w.From.IsZero() || !s.At.Before(w.From) {
				row.Windows[2*i] += s.RX
				row.Windows[2*i+1] += s.TX
			}
		}
	}

	for _, row := range byKey {
		rep.Rows = append(rep.Rows, *row)
	}
	// Heaviest download first, ties broken by key so the output is stable.
	sort.Slice(rep.Rows, func(i, j int) bool {
		if rep.Rows[i].Windows[0] != rep.Rows[j].Windows[0] {
			return rep.Rows[i].Windows[0] > rep.Rows[j].Windows[0]
		}
		return rep.Rows[i].Key < rep.Rows[j].Key
	})
	return rep
}

// colWidth fits "↓ 1.8 GB ↑ 210 MB", the widest cell expected in practice.
const colWidth = 20

func (r Row) Name() string {
	if r.Label != "" {
		return r.Label
	}
	return r.Key
}

// Write renders the report with aligned columns.
func (r Report) Write(w io.Writer) error {
	nameWidth := len("Network")
	for _, row := range r.Rows {
		if n := len(row.Name()); n > nameWidth {
			nameWidth = n
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-*s", nameWidth, "Network")
	for _, win := range r.Windows {
		fmt.Fprintf(&b, "  %*s", colWidth, win.Name)
	}
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "%-*s", nameWidth, strings.Repeat("─", nameWidth))
	for range r.Windows {
		fmt.Fprintf(&b, "  %s", strings.Repeat("─", colWidth))
	}
	fmt.Fprintln(&b)

	for _, row := range r.Rows {
		fmt.Fprintf(&b, "%-*s", nameWidth, row.Name())
		for i := range r.Windows {
			fmt.Fprintf(&b, "  %*s", colWidth, FormatBytes(row.Windows[2*i], row.Windows[2*i+1]))
		}
		fmt.Fprintln(&b)
	}

	// Total across every network, so the columns can be read without adding
	// them up by hand. Skipped when there are no rows, since a row of zeroes
	// says nothing beyond the empty table above it.
	if len(r.Rows) > 0 {
		totals := make([]uint64, 2*len(r.Windows))
		for _, row := range r.Rows {
			for i := range totals {
				totals[i] += row.Windows[i]
			}
		}

		fmt.Fprintf(&b, "%-*s", nameWidth, strings.Repeat("─", nameWidth))
		for range r.Windows {
			fmt.Fprintf(&b, "  %s", strings.Repeat("─", colWidth))
		}
		fmt.Fprintln(&b)

		fmt.Fprintf(&b, "%-*s", nameWidth, "Total")
		for i := range r.Windows {
			fmt.Fprintf(&b, "  %*s", colWidth, FormatBytes(totals[2*i], totals[2*i+1]))
		}
		fmt.Fprintln(&b)
	}

	if r.Total > 0 {
		fmt.Fprintf(&b, "(discarded: %.1f%% — %d samples, %s)\n",
			float64(r.Discarded)/float64(r.Total)*100, r.Discarded, r.reasonSummary())
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func (r Report) reasonSummary() string {
	if len(r.DiscardReasons) == 0 {
		return "none"
	}
	reasons := make([]string, 0, len(r.DiscardReasons))
	for reason, n := range r.DiscardReasons {
		reasons = append(reasons, fmt.Sprintf("%d %s", n, reason))
	}
	sort.Strings(reasons)
	return strings.Join(reasons, ", ")
}

// FormatBytes renders a pair of counts as "↓ 1.8 GB ↑ 210 MB". Units are
// binary, matching what the OS and every hotspot meter reports.
func FormatBytes(rx, tx uint64) string {
	return "↓ " + humanBytes(rx) + " ↑ " + humanBytes(tx)
}

func humanBytes(n uint64) string {
	const unit = 1024
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}

	value := float64(n)
	i := 0
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	// One decimal below 10 keeps "1.8 GB" readable; above it the decimal is
	// noise and would only widen the column. Whole bytes never take a decimal.
	if value < 10 && i > 0 {
		return fmt.Sprintf("%.1f %s", value, units[i])
	}
	return fmt.Sprintf("%.0f %s", value, units[i])
}
