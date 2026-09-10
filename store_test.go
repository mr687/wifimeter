package main

import (
	"os"
	"path/filepath"
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
