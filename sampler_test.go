package main

// Tests for sampleWith, wired through the runner seam rather than the shell.
//
// The two bugs found so far both lived in this wiring, not in the parsers:
// netstat was never executed at all, and counters were read from the wrong
// slot. Parser tests passed through both, because fixtures feed the parsers
// directly and bypass the wiring entirely. These cover the layer in between.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubRunner answers from captured output, keyed by command name.
func stubRunner(t *testing.T, out map[string]string) runner {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) (string, error) {
		body, ok := out[name]
		if !ok {
			t.Fatalf("unexpected command %q args=%v", name, args)
		}
		return body, nil
	}
}

// realShaped returns plausible output for each command, in the shapes macOS
// actually prints. Values sit in documentation ranges.
func realShaped() map[string]string {
	return map[string]string{
		"route": `   route to: default
destination: default
       mask: default
    gateway: 192.0.2.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,IFSCOPE>
`,
		"arp": `? (192.0.2.1) at 00:00:5e:00:53:01 on en0 ifscope [ethernet]
`,
		"ifconfig": `en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	ether 00:00:5e:00:53:0a
	inet 192.0.2.100 netmask 0xffffff00 broadcast 192.0.2.255
	media: autoselect
	status: active
`,
		"ipconfig": `<dictionary> {
	SSID : <redacted>
	BSSID : <redacted>
	router (ip_mult): {192.0.2.1}
	subnet_mask (ip): 255.255.255.0
	vendor_specific: ANDROID_METERED
}
`,
		"netstat": `Name  Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
en0   1500  <Link#14>    00:00:5e:00:53:0a  74226540     0  89875713350 33188946     0  10462616078     0
en0   1500  192.0.2/24   192.0.2.100        74226540     -  89875713350 33188946     -  10462616078     -
`,
	}
}

func TestSampleWith(t *testing.T) {
	out := realShaped()
	reading, fp, err := sampleWith(context.Background(), stubRunner(t, out), "en0")
	if err != nil {
		t.Fatal(err)
	}

	if !reading.LinkUp {
		t.Error("link should be up")
	}
	if got, want := reading.RX, uint64(89875713350); got != want {
		t.Errorf("rx = %d, want %d", got, want)
	}
	if got, want := reading.TX, uint64(10462616078); got != want {
		t.Errorf("tx = %d, want %d", got, want)
	}
	if reading.Err != nil {
		t.Errorf("unexpected error: %v", reading.Err)
	}
	if !fp.Complete() {
		t.Errorf("incomplete fingerprint: %+v", fp)
	}
	if got, want := fp.Key(), "192.0.2.1|255.255.255.0|00:00:5e:00:53:01"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if got, want := fp.Vendor, "ANDROID_METERED"; got != want {
		t.Errorf("vendor = %q, want %q", got, want)
	}
}

// netstat must actually be invoked. It once was not, and counters were read
// from ifconfig output instead, which parsed to zero.
func TestSampleWithRunsNetstat(t *testing.T) {
	var seen []string
	record := func(ctx context.Context, name string, args ...string) (string, error) {
		seen = append(seen, name)
		return realShaped()[name], nil
	}

	if _, _, err := sampleWith(context.Background(), record, "en0"); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"route": false, "arp": false, "ifconfig": false, "ipconfig": false, "netstat": false}
	for _, name := range seen {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("%s was never run; saw %v", name, seen)
		}
	}
}

// arp needs the gateway from route, so it cannot be one of the four commands
// launched together.
func TestSampleWithArpUsesGateway(t *testing.T) {
	var arpArgs []string
	run := func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "arp" {
			arpArgs = args
		}
		return realShaped()[name], nil
	}

	if _, _, err := sampleWith(context.Background(), run, "en0"); err != nil {
		t.Fatal(err)
	}
	if len(arpArgs) != 2 || arpArgs[0] != "-n" || arpArgs[1] != "192.0.2.1" {
		t.Errorf("arp args = %v, want [-n 192.0.2.1]", arpArgs)
	}
}

// Counters must come from netstat, not from ifconfig. Giving netstat a
// distinct value proves which one was read.
func TestSampleWithCountersComeFromNetstat(t *testing.T) {
	out := realShaped()
	out["netstat"] = `Name  Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
en0   1500  <Link#14>    00:00:5e:00:53:0a        1     0        4096        1     0        2048     0
`
	reading, _, err := sampleWith(context.Background(), stubRunner(t, out), "en0")
	if err != nil {
		t.Fatal(err)
	}
	if reading.RX != 4096 || reading.TX != 2048 {
		t.Errorf("counters = %d/%d, want 4096/2048 (netstat, not ifconfig)", reading.RX, reading.TX)
	}
}

// A fingerprint failure must not lose the counters: the sample is discarded
// later by the gate, but it still has to be measured.
func TestSampleWithUnresolvableGateway(t *testing.T) {
	out := realShaped()
	out["route"] = "route: writing to routing socket: not in table\n"

	reading, _, err := sampleWith(context.Background(), stubRunner(t, out), "en0")
	if err != nil {
		t.Fatalf("sample should still succeed: %v", err)
	}
	if reading.Err == nil {
		t.Fatal("reading.Err should carry the fingerprint failure")
	}
	if !errors.Is(reading.Err, ErrNoGateway) {
		t.Errorf("err = %v, want ErrNoGateway", reading.Err)
	}
	if reading.RX != 89875713350 {
		t.Errorf("rx = %d, counters lost when the gateway was unknown", reading.RX)
	}
}

// Unparsable counters are fatal for the sample: there is nothing to measure.
func TestSampleWithBadCounters(t *testing.T) {
	out := realShaped()
	out["netstat"] = "Name  Mtu\n"

	if _, _, err := sampleWith(context.Background(), stubRunner(t, out), "en0"); err == nil {
		t.Fatal("expected an error when netstat cannot be parsed")
	} else if !strings.Contains(err.Error(), "netstat") {
		t.Errorf("error should name netstat, got %v", err)
	}
}

// A link that is down must be reported as down, with counters still read.
func TestSampleWithLinkDown(t *testing.T) {
	out := realShaped()
	out["ifconfig"] = strings.Replace(out["ifconfig"], "status: active", "status: inactive", 1)

	reading, _, err := sampleWith(context.Background(), stubRunner(t, out), "en0")
	if err != nil {
		t.Fatal(err)
	}
	if reading.LinkUp {
		t.Error("link should be down")
	}
}

func TestSampleWithPropagatesCommandFailure(t *testing.T) {
	out := realShaped()
	run := func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "netstat" {
			return "", errors.New("boom")
		}
		return out[name], nil
	}

	if _, _, err := sampleWith(context.Background(), run, "en0"); err == nil {
		t.Fatal("expected the netstat failure to propagate")
	}
}
