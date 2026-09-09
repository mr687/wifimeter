package main

import (
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

	total, discarded, err := s.DiscardStats()
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || discarded != 1 {
		t.Errorf("stats = total %d discarded %d, want 4/1", total, discarded)
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
