package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	hot := Fingerprint{GatewayIP: "192.0.2.1", Subnet: "255.255.255.0", GatewayMAC: "00:00:5e:00:53:01", Vendor: "ANDROID_METERED"}
	home := Fingerprint{GatewayIP: "198.51.100.1", Subnet: "255.255.255.0", GatewayMAC: "00:00:5e:00:53:02"}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertNetwork(hot, now); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertNetwork(home, now); err != nil {
		t.Fatal(err)
	}
	// Second sighting must advance last_seen, not duplicate the row.
	if err := s.UpsertNetwork(hot, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	samples := []Sample{
		{At: now, FPKey: hot.Key(), RX: 100, TX: 10, Valid: true},
		{At: now.Add(10 * time.Second), FPKey: hot.Key(), RX: 200, TX: 20, Valid: true},
		{At: now.Add(20 * time.Second), FPKey: home.Key(), RX: 50, TX: 5, Valid: true},
		{At: now.Add(30 * time.Second), FPKey: hot.Key(), Valid: false, Reason: ReasonSleep},
		{At: now.Add(40 * time.Second), FPKey: home.Key(), Valid: false, Reason: ReasonSwitch},
	}
	for _, samp := range samples {
		if err := s.InsertSample(samp); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.SetLabel(hot.Key(), "Phone hotspot"); err != nil {
		t.Fatal(err)
	}

	labels, err := s.Labels()
	if err != nil {
		t.Fatal(err)
	}
	if labels[hot.Key()] != "Phone hotspot" {
		t.Errorf("label = %q, want Phone hotspot", labels[hot.Key()])
	}

	got, err := s.SamplesSince(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(samples) {
		t.Fatalf("got %d samples, want %d", len(got), len(samples))
	}
	if got[0].RX != 100 || !got[0].Valid {
		t.Errorf("first sample = %+v", got[0])
	}
	if got[3].Valid {
		t.Error("discarded sample round-tripped as valid")
	}
	// The reason is what doctor splits discard rates by, so it has to survive
	// the trip rather than coming back empty.
	if got[3].Reason != ReasonSleep {
		t.Errorf("sample 3 reason = %q, want %q", got[3].Reason, ReasonSleep)
	}
	if got[4].Reason != ReasonSwitch {
		t.Errorf("sample 4 reason = %q, want %q", got[4].Reason, ReasonSwitch)
	}

	total, discarded, err := s.DiscardStats()
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || discarded != 2 {
		t.Errorf("stats = total %d discarded %d, want 5/2", total, discarded)
	}
}

// Reopening must see prior data: the agent restarts constantly under KeepAlive.
func TestStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSample(Sample{At: now, FPKey: "k", RX: 7, TX: 3, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.SamplesSince(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RX != 7 {
		t.Fatalf("reopen lost data: %+v", got)
	}
}

// A database written before the reason column existed must gain it on open.
// CREATE TABLE IF NOT EXISTS cannot do this, so migrate has to.
func TestMigrateAddsReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")

	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-column schema by hand.
	if _, err := old.db.Exec(`DROP TABLE samples`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`CREATE TABLE samples (
  id     INTEGER PRIMARY KEY,
  ts     INTEGER NOT NULL,
  fp     TEXT    NOT NULL,
  rx     INTEGER NOT NULL,
  tx     INTEGER NOT NULL,
  valid  INTEGER NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if _, err := old.db.Exec(
		`INSERT INTO samples (ts, fp, rx, tx, valid) VALUES (?, ?, ?, ?, ?)`,
		now.Unix(), "k", 5, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening runs migrate against the old table.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.SamplesSince(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d samples, want 1", len(got))
	}
	// The pre-existing row has no reason recorded; it must read as none rather
	// than failing the scan.
	if got[0].Reason != ReasonNone {
		t.Errorf("migrated row reason = %q, want %q", got[0].Reason, ReasonNone)
	}

	// And new writes through the migrated table must keep their reason.
	if err := s.InsertSample(Sample{At: now.Add(10 * time.Second), FPKey: "k", Valid: false, Reason: ReasonLinkDown}); err != nil {
		t.Fatal(err)
	}
	got, err = s.SamplesSince(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Reason != ReasonLinkDown {
		t.Errorf("post-migration reason = %q, want %q", got[1].Reason, ReasonLinkDown)
	}
}

func TestCheckpointTruncatesWal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	walPath := path + "-wal"

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for i := range 500 {
		sm := Sample{At: now.Add(time.Duration(i) * 10 * time.Second), FPKey: "k", RX: 100, TX: 10, Valid: true}
		if err := s.InsertSample(sm); err != nil {
			t.Fatal(err)
		}
	}

	before, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("no wal after inserts: %v", err)
	}
	if before.Size() == 0 {
		t.Fatal("wal empty after inserts; nothing to checkpoint")
	}

	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("wal missing after checkpoint: %v", err)
	}
	if after.Size() >= before.Size() {
		t.Errorf("wal %d -> %d, want it to shrink", before.Size(), after.Size())
	}

	got, err := s.SamplesSince(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 500 {
		t.Errorf("got %d samples after checkpoint, want 500", len(got))
	}
}

func TestUserVersionSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
}

// A rolled-up day must carry exactly what the raw samples for that day held.
// This is the invariant the read path will depend on once it starts reading
// daily_usage instead of samples, so it is asserted before anything does.
func TestRollupMatchesRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local)
	hot := "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"
	quiet := "198.51.100.1|255.255.255.0|00:00:5e:00:53:02"

	// Four valid samples and three discards, spread across two networks, all
	// inside the target day.
	inserts := []Sample{
		{At: day.Add(3 * time.Hour), FPKey: hot, RX: 700, TX: 70, Valid: true},
		{At: day.Add(9 * time.Hour), FPKey: hot, RX: 300, TX: 30, Valid: true},
		{At: day.Add(15 * time.Hour), FPKey: quiet, RX: 50, TX: 5, Valid: true},
		{At: day.Add(23 * time.Hour), FPKey: hot, RX: 100, TX: 10, Valid: true},
		{At: day.Add(4 * time.Hour), FPKey: hot, Valid: false, Reason: ReasonSleep},
		{At: day.Add(5 * time.Hour), FPKey: hot, Valid: false, Reason: ReasonSleep},
		{At: day.Add(6 * time.Hour), FPKey: quiet, Valid: false, Reason: ReasonSwitch},
	}
	for _, sm := range inserts {
		if err := s.InsertSample(sm); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.RollupDay(day); err != nil {
		t.Fatal(err)
	}

	got, err := s.DailyUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d daily rows, want 2", len(got))
	}

	want := map[string]DayTotal{
		hot:   {RX: 1100, TX: 110},
		quiet: {RX: 50, TX: 5},
	}
	for _, d := range got {
		w, ok := want[d.FPKey]
		if !ok {
			t.Errorf("unexpected fp %q in daily_usage", d.FPKey)
			continue
		}
		if d.RX != w.RX || d.TX != w.TX {
			t.Errorf("%s = rx %d tx %d, want rx %d tx %d", d.FPKey, d.RX, d.TX, w.RX, w.TX)
		}
		if d.Samples != 3 && d.FPKey == hot {
			t.Errorf("hot samples counted = %d, want 3", d.Samples)
		}
	}

	disc, err := s.DailyDiscards()
	if err != nil {
		t.Fatal(err)
	}
	if disc["sleep/wake"] != 2 {
		t.Errorf("sleep/wake discards = %d, want 2", disc["sleep/wake"])
	}
	if disc["network-switch"] != 1 {
		t.Errorf("network-switch discards = %d, want 1", disc["network-switch"])
	}

	// Raw rows survive: this phase populates, it does not retain.
	raw, err := s.SamplesSince(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != len(inserts) {
		t.Errorf("got %d raw samples, want %d; rollup must not delete", len(raw), len(inserts))
	}
}

// Running a rollup twice must not double count, or every agent restart would
// inflate the totals it is meant to be summarising.
func TestRollupIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local)
	if err := s.InsertSample(Sample{At: day.Add(3 * time.Hour), FPKey: "k", RX: 500, TX: 50, Valid: true}); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := s.RollupDay(day); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.DailyUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].RX != 500 {
		t.Errorf("rx = %d after three rollups, want 500", got[0].RX)
	}
	if got[0].Samples != 1 {
		t.Errorf("samples = %d after three rollups, want 1", got[0].Samples)
	}
}

// Samples just outside the target day must land in their own bucket or none at
// all, never in the day being rolled up.
func TestRollupRespectsDayBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local)
	before := day.Add(-time.Second)
	after := day.AddDate(0, 0, 1)

	if err := s.InsertSample(Sample{At: before, FPKey: "k", RX: 111, TX: 11, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSample(Sample{At: day.Add(time.Hour), FPKey: "k", RX: 222, TX: 22, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSample(Sample{At: after, FPKey: "k", RX: 333, TX: 33, Valid: true}); err != nil {
		t.Fatal(err)
	}

	if err := s.RollupDay(day); err != nil {
		t.Fatal(err)
	}

	got, err := s.DailyUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Day != "2026-09-09" {
		t.Errorf("day = %q, want 2026-09-09", got[0].Day)
	}
	if got[0].RX != 222 {
		t.Errorf("rx = %d, want 222 (the neighbouring days must stay out)", got[0].RX)
	}
}

// The whole point of this phase: for data that has been rolled up, the report
// must render exactly as it did when it read the raw rows. Same bytes, not
// merely the same totals.
func TestReportIdenticalAfterRollup(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 30, 0, 0, time.UTC)
	hot := "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"
	home := "198.51.100.1|255.255.255.0|00:00:5e:00:53:02"

	day := now.Add(-3 * time.Hour)
	week := now.Add(-3 * 24 * time.Hour)
	old := now.Add(-30 * 24 * time.Hour)

	samples := []Sample{
		{At: day, FPKey: hot, RX: 1_800_000_000, TX: 210_000_000, Valid: true},
		{At: week, FPKey: hot, RX: 4_400_000_000, TX: 570_000_000, Valid: true},
		{At: day, FPKey: home, RX: 42_000_000_000, TX: 3_100_000_000, Valid: true},
		{At: week, FPKey: home, RX: 168_000_000_000, TX: 11_000_000_000, Valid: true},
		{At: old, FPKey: home, RX: 900_000_000_000, TX: 74_000_000_000, Valid: true},
		{At: day, FPKey: home, Valid: false, Reason: ReasonSleep},
		{At: day, FPKey: home, Valid: false, Reason: ReasonSleep},
		{At: day, FPKey: hot, Valid: false, Reason: ReasonSwitch},
	}

	labels := map[string]string{hot: "Phone hotspot", home: "Home"}
	before := Aggregate(samples, windowsFor(now, 7), labels)

	// Same data through a database, with every day rolled up.
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, sm := range samples {
		if err := s.InsertSample(sm); err != nil {
			t.Fatal(err)
		}
	}
	// Roll up in the local zone, which is what both the daemon and
	// SamplesForReport bucket by. Passing the UTC fixture times would key the
	// days by UTC while the reader keys by local, and nothing would match.
	for _, at := range []time.Time{day, week, old} {
		if err := s.RollupDay(at.In(time.Local)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.SamplesForReport()
	if err != nil {
		t.Fatal(err)
	}
	after := Aggregate(got, windowsFor(now, 7), labels)

	var sbBefore, sbAfter strings.Builder
	if err := before.Write(&sbBefore); err != nil {
		t.Fatal(err)
	}
	if err := after.Write(&sbAfter); err != nil {
		t.Fatal(err)
	}
	if sbBefore.String() != sbAfter.String() {
		t.Errorf("report changed after rollup:\n--- raw ---\n%s\n--- rolled ---\n%s", sbBefore.String(), sbAfter.String())
	}
}
