package main

// report, label, and doctor.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	days := fs.Int("days", 7, "rolling window in days")
	daily := fs.Bool("daily", false, "break one network down by day")
	label := fs.String("label", "", "network to break down by name")
	fp := fs.String("fp", "", "network to break down by fingerprint key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path, err := DefaultDBPath()
	if err != nil {
		return err
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()

	samples, err := store.SamplesSince(time.Unix(0, 0))
	if err != nil {
		return err
	}

	if *daily {
		return dailyReport(os.Stdout, store, samples, *days, *label, *fp)
	}

	labels, err := store.Labels()
	if err != nil {
		return err
	}

	rep := Aggregate(samples, windowsFor(time.Now(), *days), labels)
	total, discarded, err := store.DiscardStats()
	if err != nil {
		return err
	}
	rep.Total, rep.Discarded = total, discarded
	return rep.Write(os.Stdout)
}

// dailyReport breaks a single network down by day. Breaking every network
// down at once would run to hundreds of lines, so with no --label or --fp it
// picks the network that has carried the most.
func dailyReport(w io.Writer, store *Store, samples []Sample, days int, label, fp string) error {
	key := fp
	if key == "" && label != "" {
		labels, err := store.Labels()
		if err != nil {
			return err
		}
		for k, v := range labels {
			if v == label {
				key = k
				break
			}
		}
		if key == "" {
			return fmt.Errorf("no network labeled %q", label)
		}
	}
	if key == "" {
		key = HeaviestKey(samples)
		if key == "" {
			return fmt.Errorf("no samples recorded yet")
		}
	}

	var scoped []Sample
	for _, s := range samples {
		if s.FPKey == key {
			scoped = append(scoped, s)
		}
	}

	labels, err := store.Labels()
	if err != nil {
		return err
	}
	name := labels[key]
	if name == "" {
		name = key
	}

	now := time.Now()
	dayTotals := Daily(scoped, days, now)
	return WriteDaily(w, name, dayTotals, SummarizeDaily(dayTotals))
}

// windowsFor builds today / N days / all time, counted from local midnight.
func windowsFor(now time.Time, days int) []Window {
	if days < 1 {
		days = 1
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return []Window{
		{Name: "Today", From: midnight},
		{Name: fmt.Sprintf("%d days", days), From: midnight.AddDate(0, 0, -(days - 1))},
		{Name: "All time"},
	}
}

func labelCmd(args []string) error {
	fs := flag.NewFlagSet("label", flag.ExitOnError)
	fp := fs.String("fp", "", "fingerprint to name; defaults to the current network")
	ifaceFlag := fs.String("iface", "", "interface to meter; defaults to the detected Wi-Fi device")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name := fs.Arg(0)
	if name == "" {
		return fmt.Errorf("usage: wifimeter label [--fp KEY] \"name\"")
	}

	key := *fp
	if key == "" {
		_, f, err := SampleLink(context.Background(), ResolveIface(*ifaceFlag))
		if err != nil {
			return fmt.Errorf("cannot identify current network: %w", err)
		}
		if !f.Complete() {
			return fmt.Errorf("current network is not fully identified")
		}
		key = f.Key()
	}

	path, err := DefaultDBPath()
	if err != nil {
		return err
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.SetLabel(key, name); err != nil {
		return err
	}
	fmt.Printf("labeled %s as %q\n", key, name)
	return nil
}

// doctor proves the zero-permission path: every read works without sudo and
// without Location Services. It is the first thing to run when something
// looks wrong, and the only command that needs no stored data.
func doctorCmd(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	ifaceFlag := fs.String("iface", "", "interface to meter; defaults to the detected Wi-Fi device")
	if err := fs.Parse(args); err != nil {
		return err
	}

	iface := ResolveIface(*ifaceFlag)
	reading, fp, err := SampleLink(context.Background(), iface)
	w := os.Stdout

	fmt.Fprintf(w, "interface: %s\n\n", iface)
	fmt.Fprintf(w, "%-14s %-6s %-34s %s\n", "read", "sudo", "value", "status")
	for _, r := range []struct {
		name string
		ok   bool
		val  string
	}{
		{"counters", err == nil, fmt.Sprintf("rx=%d tx=%d", reading.RX, reading.TX)},
		{"link", reading.LinkUp, map[bool]string{true: "active", false: "down"}[reading.LinkUp]},
		{"gateway", fp.GatewayIP != "", fp.GatewayIP},
		{"gateway mac", fp.GatewayMAC != "", fp.GatewayMAC},
		{"subnet", fp.Subnet != "", fp.Subnet},
		{"vendor", true, fp.Vendor},
	} {
		status := "ok"
		if !r.ok {
			status = "FAIL"
		}
		val := r.val
		if val == "" {
			val = "-"
		}
		fmt.Fprintf(w, "%-14s %-6s %-34s %s\n", r.name, "no", val, status)
	}

	if err != nil {
		fmt.Fprintf(w, "\nerror: %v\n", err)
	}
	if !fp.Complete() {
		fmt.Fprintf(w, "\nnetwork is not fully identified; samples will be discarded\n")
	}
	return nil
}
