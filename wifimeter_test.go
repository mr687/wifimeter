package main

// Golden tests over captured command output.
//
// Every fixture in testdata/inputs is the verbatim stdout of one of the four
// commands the daemon runs, so these tests exercise the same parsing the daemon
// will do. Run with -update to rewrite the goldens after an intentional change.

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "inputs", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(data)
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("updating golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (run go test -update): %v", name, err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

func TestFingerprint(t *testing.T) {
	cases := []struct {
		name, route, arp, ifconfig, summary, golden string
		wantErr                                     error
	}{
		{
			name:     "hotspot",
			route:    "route_hotspot.txt",
			arp:      "arp_hotspot.txt",
			ifconfig: "ifconfig_active.txt",
			summary:  "summary_hotspot.txt",
			golden:   "fingerprint_hotspot.txt",
		},
		{
			name:     "home behind warp",
			route:    "route_warp.txt",
			arp:      "arp_warp.txt",
			ifconfig: "ifconfig_active.txt",
			summary:  "summary_home.txt",
			golden:   "fingerprint_home.txt",
		},
		{
			name:     "uppercase mac normalizes",
			route:    "route_warp.txt",
			arp:      "arp_upper.txt",
			ifconfig: "ifconfig_active.txt",
			summary:  "summary_home.txt",
			golden:   "fingerprint_upper.txt",
		},
		{
			name:     "short mac octets are padded",
			route:    "route_warp.txt",
			arp:      "arp_shortmac.txt",
			ifconfig: "ifconfig_active.txt",
			summary:  "summary_home.txt",
			golden:   "fingerprint_shortmac.txt",
		},
		{
			name:     "no gateway",
			route:    "route_none.txt",
			arp:      "arp_hotspot.txt",
			ifconfig: "ifconfig_active.txt",
			summary:  "summary_hotspot.txt",
			wantErr:  ErrNoGateway,
		},
		{
			name:     "arp incomplete",
			route:    "route_hotspot.txt",
			arp:      "arp_incomplete.txt",
			ifconfig: "ifconfig_active.txt",
			summary:  "summary_hotspot.txt",
			wantErr:  ErrNoMAC,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, err := Observe(fixture(t, tc.route), fixture(t, tc.arp),
				fixture(t, tc.ifconfig), fixture(t, tc.summary))

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !obs.FP.Complete() {
				t.Errorf("incomplete fingerprint: %+v", obs.FP)
			}
			got := "key: " + obs.FP.Key() + "\nlinkup: "
			if obs.LinkUp {
				got += "true"
			} else {
				got += "false"
			}
			got += "\n"
			checkGolden(t, tc.golden, got)
		})
	}
}

// Two networks sharing 203.0.113.1 must not collapse into one: the MAC is what
// keeps them apart.
func TestFingerprintCollision(t *testing.T) {
	a, err := Observe("    gateway: 203.0.113.1\n", "? (203.0.113.1) at 00:00:5e:00:53:aa on en0\n",
		"status: active\n", "subnet_mask (ip): 255.255.255.0\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Observe("    gateway: 203.0.113.1\n", "? (203.0.113.1) at 00:00:5e:00:53:bb on en0\n",
		"status: active\n", "subnet_mask (ip): 255.255.255.0\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.FP.Key() == b.FP.Key() {
		t.Fatalf("networks collided on %q", a.FP.Key())
	}
}

func TestParseCounters(t *testing.T) {
	got, err := parseCounters(fixture(t, "netstat_en0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got.RX != 987654321 || got.TX != 123456789 {
		t.Errorf("got rx=%d tx=%d, want 987654321/123456789", got.RX, got.TX)
	}

	if _, err := parseCounters(fixture(t, "netstat_empty.txt")); err == nil {
		t.Error("empty netstat output should not parse")
	}

	// A counter near the uint64 ceiling must parse exactly, not overflow.
	wrap, err := parseCounters(fixture(t, "netstat_wrap.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if wrap.RX != 18446744073709551615 {
		t.Errorf("rx = %d, want max uint64", wrap.RX)
	}
}

func TestDeltaGate(t *testing.T) {
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	hot := "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"
	home := "198.51.100.1|255.255.255.0|00:00:5e:00:53:02"

	cases := []struct {
		name       string
		prev, cur  Reading
		wantValid  bool
		wantReason Reason
	}{
		{
			name:      "normal interval",
			prev:      Reading{Counters: Counters{RX: 100, TX: 50}, At: base, FPKey: hot, LinkUp: true},
			cur:       Reading{Counters: Counters{RX: 300, TX: 80}, At: base.Add(10 * time.Second), FPKey: hot, LinkUp: true},
			wantValid: true,
		},
		{
			name:       "link down",
			prev:       Reading{Counters: Counters{RX: 100, TX: 50}, At: base, FPKey: hot, LinkUp: true},
			cur:        Reading{Counters: Counters{RX: 300, TX: 80}, At: base.Add(10 * time.Second), FPKey: hot, LinkUp: false},
			wantReason: ReasonLinkDown,
		},
		{
			name:       "counter reset",
			prev:       Reading{Counters: Counters{RX: 900, TX: 50}, At: base, FPKey: hot, LinkUp: true},
			cur:        Reading{Counters: Counters{RX: 100, TX: 80}, At: base.Add(10 * time.Second), FPKey: hot, LinkUp: true},
			wantReason: ReasonReset,
		},
		{
			name:       "network switch",
			prev:       Reading{Counters: Counters{RX: 100, TX: 50}, At: base, FPKey: hot, LinkUp: true},
			cur:        Reading{Counters: Counters{RX: 300, TX: 80}, At: base.Add(10 * time.Second), FPKey: home, LinkUp: true},
			wantReason: ReasonSwitch,
		},
		{
			name:       "sleep gap",
			prev:       Reading{Counters: Counters{RX: 100, TX: 50}, At: base, FPKey: hot, LinkUp: true},
			cur:        Reading{Counters: Counters{RX: 2 << 30, TX: 80}, At: base.Add(20 * time.Minute), FPKey: hot, LinkUp: true},
			wantReason: ReasonSleep,
		},
		{
			name:       "no gateway",
			prev:       Reading{Counters: Counters{RX: 100, TX: 50}, At: base, FPKey: hot, LinkUp: true},
			cur:        Reading{Counters: Counters{RX: 300, TX: 80}, At: base.Add(10 * time.Second), LinkUp: true, Err: ErrNoGateway},
			wantReason: ReasonNoGateway,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b Baseline
			if tc.prev.LinkUp {
				b.Set = true
				b.At = tc.prev.At
				b.Counters = tc.prev.Counters
				b.FPKey = tc.prev.FPKey
			}
			got := b.Apply(tc.cur, DefaultInterval)
			if got.Valid != tc.wantValid {
				t.Fatalf("valid = %v, want %v (reason %q)", got.Valid, tc.wantValid, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if !tc.wantValid && (got.RX != 0 || got.TX != 0) {
				t.Errorf("discarded sample carried bytes: rx=%d tx=%d", got.RX, got.TX)
			}
		})
	}

	t.Run("first sample", func(t *testing.T) {
		var b Baseline
		got := b.Apply(Reading{RX: 5 << 30, TX: 1 << 30, At: base, FPKey: hot, LinkUp: true}, DefaultInterval)
		if got.Valid || got.Reason != ReasonFirst {
			t.Fatalf("first sample should be discarded, got valid=%v reason=%q", got.Valid, got.Reason)
		}
		if got.RX != 0 {
			t.Errorf("first sample attributed %d bytes", got.RX)
		}
		if !b.Set {
			t.Error("baseline should be established after first sample")
		}
	})
}

// A discarded sample must still leave a usable baseline, or one bad interval
// would poison the next one too.
func TestDiscardStillRebaselines(t *testing.T) {
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	key := "203.0.113.1|255.255.255.0|00:00:5e:00:53:aa"

	var b Baseline
	b.Set = true
	b.At = base
	b.Counters = Counters{RX: 1000, TX: 1000}
	b.FPKey = key

	b.Apply(Reading{RX: 5000, TX: 2000, At: base.Add(10 * time.Second), FPKey: key, LinkUp: true}, DefaultInterval)
	if b.RX != 5000 || b.TX != 2000 {
		t.Fatalf("baseline not advanced: rx=%d tx=%d", b.RX, b.TX)
	}

	got := b.Apply(Reading{RX: 5200, TX: 2100, At: base.Add(20 * time.Second), FPKey: key, LinkUp: true}, DefaultInterval)
	if !got.Valid || got.RX != 200 || got.TX != 100 {
		t.Errorf("got valid=%v rx=%d tx=%d, want true 200 100", got.Valid, got.RX, got.TX)
	}
}

func TestReport(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 30, 0, 0, time.UTC)
	windows := windowsFor(now, 7)
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
	rep := Aggregate(samples, windows, labels)

	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "report.txt", sb.String())
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		rx, tx uint64
		want   string
	}{
		{0, 0, "↓ 0 B ↑ 0 B"},
		{512, 1024, "↓ 512 B ↑ 1.0 KB"},
		{1_800_000_000, 210_000_000, "↓ 1.7 GB ↑ 200 MB"},
		{42_000_000_000, 3_100_000_000, "↓ 39 GB ↑ 2.9 GB"},
	}
	for _, tc := range cases {
		if got := FormatBytes(tc.rx, tc.tx); got != tc.want {
			t.Errorf("FormatBytes(%d, %d) = %q, want %q", tc.rx, tc.tx, got, tc.want)
		}
	}
}

// arp on macOS leaves the leading zero off single hex digits, so a gateway
// MAC arrives as 8:0:5e:2:53:7. net.ParseMAC rejects that form, and a gateway
// with any byte under 0x10 would otherwise fail to parse and lose every
// sample from its network.
func TestParseGatewayMACPadding(t *testing.T) {
	cases := []struct{ in, want string }{
		{"? (198.51.100.1) at 8:0:5e:2:53:7 on en0 ifscope [ethernet]", "08:00:5e:02:53:07"},
		{"? (198.51.100.1) at 00:00:5e:00:53:0a on en0 ifscope [ethernet]", "00:00:5e:00:53:0a"},
		{"? (198.51.100.1) at 00:00:5E:00:53:0A on en0 ifscope [ethernet]", "00:00:5e:00:53:0a"},
	}
	for _, tc := range cases {
		got, err := parseGatewayMAC(tc.in)
		if err != nil {
			t.Fatalf("parseGatewayMAC(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("parseGatewayMAC(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := parseGatewayMAC("? (198.51.100.1) at (incomplete) on en0 ifscope [ethernet]"); err == nil {
		t.Error("incomplete arp entry should not parse")
	}
}
