package main

// Sampling: run the commands and turn their output into a Reading.
//
// None of these need privilege. That is the whole point of the design: the
// kernel counters are free and exact, and the network can be identified by its
// gateway without ever asking for Location Services.
//
// route, ifconfig, ipconfig, and netstat are independent so they run
// concurrently; the sample costs as long as the slowest one rather than the
// sum. arp has to wait, because it needs the gateway as an argument.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// FallbackIface is used only when the Wi-Fi interface cannot be detected.
// Wi-Fi is en0 on most Macs but not all: any machine with a USB Ethernet
// adapter, or some Intel models, put it elsewhere. Only the physical Wi-Fi
// interface is metered, since tunnels are encapsulated inside its frames and
// would be double counted, and awdl0/llw0 share the radio without being
// network usage.
const FallbackIface = "en0"

// DetectIface asks the system which device is Wi-Fi, reading
// `networksetup -listallhardwareports`:
//
//	Hardware Port: Wi-Fi
//	Device: en0
//
// Returns "" when it cannot be determined, so callers can fall back.
func DetectIface() string {
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return ""
	}
	wantDevice := false
	for line := range strings.SplitSeq(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if wantDevice {
			if name, ok := strings.CutPrefix(trimmed, "Device: "); ok {
				return strings.TrimSpace(name)
			}
			continue
		}
		wantDevice = trimmed == "Hardware Port: Wi-Fi"
	}
	return ""
}

// ResolveIface returns the explicit choice, else the detected Wi-Fi device,
// else the fallback.
func ResolveIface(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if got := DetectIface(); got != "" {
		return got
	}
	return FallbackIface
}

// runner runs one command and returns its stdout. Seamed so tests can feed
// fixtures without executing anything.
type runner func(ctx context.Context, name string, args ...string) (string, error)

// execRunner is the real runner.
func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return string(out), nil
}

const (
	idxRoute = iota
	idxArp
	idxIfconfig
	idxSummary
	idxNetstat
	nCmd
)

// SampleLink takes one reading of the link and of the byte counters.
func SampleLink(ctx context.Context, iface string) (Reading, Fingerprint, error) {
	return sampleWith(ctx, execRunner, iface)
}

func sampleWith(ctx context.Context, run runner, iface string) (Reading, Fingerprint, error) {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		outs [nCmd]string
		errs [nCmd]error
	)

	runIdx := func(i int, name string, args ...string) {
		defer wg.Done()
		out, err := run(ctx, name, args...)
		mu.Lock()
		defer mu.Unlock()
		outs[i], errs[i] = out, err
	}

	wg.Add(4)
	go runIdx(idxRoute, "route", "-n", "get", "default", "-ifscope", iface)
	go runIdx(idxIfconfig, "ifconfig", iface)
	go runIdx(idxSummary, "ipconfig", "getsummary", iface)
	go runIdx(idxNetstat, "netstat", "-ibn", "-I", iface)
	wg.Wait()

	reading := Reading{At: time.Now()}

	// arp needs the gateway from route, so it cannot start any earlier.
	if gw, err := parseGateway(outs[idxRoute]); err != nil {
		reading.Err = err
	} else {
		wg.Add(1)
		go runIdx(idxArp, "arp", "-n", gw)
		wg.Wait()
	}

	obs, fpErr := Observe(outs[idxRoute], outs[idxArp], outs[idxIfconfig], outs[idxSummary])
	if fpErr != nil && reading.Err == nil {
		reading.Err = fpErr
	}
	reading.LinkUp = obs.LinkUp
	reading.FPKey = obs.FP.Key()

	// A fingerprint failure only costs this interval. A counter failure means
	// we cannot measure at all, so it is reported as an error.
	if errs[idxNetstat] != nil {
		return reading, obs.FP, fmt.Errorf("netstat: %w", errs[idxNetstat])
	}
	counters, err := parseCounters(outs[idxNetstat])
	if err != nil {
		return reading, obs.FP, fmt.Errorf("netstat: %w", err)
	}
	reading.Counters = counters

	return reading, obs.FP, nil
}
