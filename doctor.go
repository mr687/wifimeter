package main

// doctor's database checks: sample counts, discard rate, size, agent state.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// DBStats is what doctor reports about stored data.
type DBStats struct {
	Total     int
	Discarded int
	Networks  int
	Oldest    time.Time
	Newest    time.Time
	SizeBytes int64
	AgentPID  int // 0 when the agent is not loaded
}

// DiscardRate is the percentage of samples discarded.
func (s DBStats) DiscardRate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Discarded) / float64(s.Total) * 100
}

// discardWarnPct is far above the sleep and switch losses a working collector
// produces, so crossing it means something is wrong.
const discardWarnPct = 20

var pidRe = regexp.MustCompile(`"PID"\s*=\s*(\d+)`)

// agentPID returns the agent's pid, or 0 when it is not loaded. launchctl
// exits non-zero for a label it does not have.
func agentPID(label string) int {
	out, err := exec.Command("launchctl", "list", label).Output()
	if err != nil {
		return 0
	}
	m := pidRe.FindSubmatch(out)
	if m == nil {
		return 0
	}
	pid, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return pid
}

// collectDBStats gathers figures from the database and launchd. It never
// inserts; opening the database does apply schema and WAL pragmas.
func collectDBStats(dbPath, label string) (DBStats, error) {
	var s DBStats

	if info, err := os.Stat(dbPath); err == nil {
		s.SizeBytes = info.Size()
	}
	s.AgentPID = agentPID(label)

	store, err := Open(dbPath)
	if err != nil {
		return s, err
	}
	defer store.Close()

	if s.Total, s.Discarded, err = store.DiscardStats(); err != nil {
		return s, err
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM networks`).Scan(&s.Networks); err != nil {
		return s, err
	}

	var oldest, newest *int64
	if err := store.db.QueryRow(`SELECT MIN(ts), MAX(ts) FROM samples`).Scan(&oldest, &newest); err != nil {
		return s, err
	}
	if oldest != nil {
		s.Oldest = time.Unix(*oldest, 0)
	}
	if newest != nil {
		s.Newest = time.Unix(*newest, 0)
	}
	return s, nil
}

// writeDBStats renders the database half of doctor.
func writeDBStats(w io.Writer, s DBStats) error {
	fmt.Fprintf(w, "\ndatabase\n")

	if s.Total == 0 {
		fmt.Fprintf(w, "  %-14s none\n", "samples")
	} else {
		fmt.Fprintf(w, "  %-14s %d\n", "samples", s.Total)
		fmt.Fprintf(w, "  %-14s %d (%.1f%%)\n", "discarded", s.Discarded, s.DiscardRate())
		fmt.Fprintf(w, "  %-14s %d\n", "networks", s.Networks)
		fmt.Fprintf(w, "  %-14s %s\n", "oldest", s.Oldest.Format("2006-01-02 15:04"))
		fmt.Fprintf(w, "  %-14s %s\n", "newest", s.Newest.Format("2006-01-02 15:04"))
	}

	fmt.Fprintf(w, "  %-14s %s\n", "size", humanBytes(uint64(s.SizeBytes)))

	switch {
	case s.AgentPID == 0:
		fmt.Fprintf(w, "  %-14s not loaded\n", "agent")
	default:
		fmt.Fprintf(w, "  %-14s running (pid %d)\n", "agent", s.AgentPID)
	}

	if s.Total > 0 && s.DiscardRate() > discardWarnPct {
		fmt.Fprintf(w, "\n  warning: %.0f%% of samples discarded\n", s.DiscardRate())
	}
	return nil
}
