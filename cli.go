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

	// Raw rows cover the retention window; rolled-up days cover everything
	// older. Retention deletes nothing yet, so this is the same set as before,
	// just sourced from whichever table still holds it.
	samples, err := store.SamplesForReport()
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

// dailyReport breaks usage down by day. With --label or --fp it covers that
// one network; with neither it sums every network into the same day buckets,
// which is the number that answers "did my usage spike this week" regardless
// of where it happened.
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

	scoped := samples
	name := "All networks"
	if key != "" {
		scoped = nil
		for _, s := range samples {
			if s.FPKey == key {
				scoped = append(scoped, s)
			}
		}

		labels, err := store.Labels()
		if err != nil {
			return err
		}
		name = labels[key]
		if name == "" {
			name = key
		}
	} else if len(samples) == 0 {
		return fmt.Errorf("no samples recorded yet")
	}

	dayTotals := Daily(scoped, days, time.Now())
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
		{Name: pluralDays(days), From: midnight.AddDate(0, 0, -(days - 1))},
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

	dbPath, err := DefaultDBPath()
	if err != nil {
		return err
	}
	stats, err := collectDBStats(dbPath, agentLabel)
	if err != nil {
		return err
	}
	return writeDBStats(w, stats)
}

// compactCmd rewrites the database so pages freed by retention go back to the
// filesystem. Deleting rows does not shrink the file on its own.
func compactCmd() error {
	path, err := DefaultDBPath()
	if err != nil {
		return err
	}

	before, _ := os.Stat(path)

	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Compact(); err != nil {
		return err
	}

	after, err := os.Stat(path)
	if err != nil {
		return err
	}
	if before != nil {
		fmt.Printf("compacted %s: %s -> %s\n", path,
			humanBytes(uint64(before.Size())), humanBytes(uint64(after.Size())))
	}
	return nil
}

func pluralDays(n int) string {
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// mergeCmd makes one network report as another, for a hotspot whose gateway
// MAC rotates and so shows up as several fingerprints.
func mergeCmd(args []string) error {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	into := fs.String("into", "", "name of the network to merge into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src := fs.Arg(0)
	if src == "" || *into == "" {
		return fmt.Errorf("usage: wifimeter merge NAME --into NAME")
	}
	if src == *into {
		return fmt.Errorf("cannot merge %q into itself", src)
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

	srcFP, err := store.FPForLabel(src)
	if err != nil {
		return err
	}
	dstFP, err := store.FPForLabel(*into)
	if err != nil {
		return err
	}
	if err := store.MergeNetworks(srcFP, dstFP); err != nil {
		return err
	}
	fmt.Printf("merged %q into %q\n", src, *into)
	return nil
}

// unmergeCmd restores a merged network as its own entry. The old name is not
// restored, so it reports as a raw fingerprint until labeled again.
func unmergeCmd(args []string) error {
	if len(args) < 1 || args[0] == "" {
		return fmt.Errorf("usage: wifimeter unmerge NAME")
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

	// The name is gone once merged, so resolve through the alias directly.
	rows, err := store.Aliased()
	if err != nil {
		return err
	}
	for _, a := range rows {
		if label, ok := store.labelOf(a.AliasOf); ok && label == args[0] {
			if err := store.UnmergeNetwork(a.FP); err != nil {
				return err
			}
			fmt.Printf("unmerged a network from %q\n", args[0])
			return nil
		}
	}
	return fmt.Errorf("no network is merged into %q", args[0])
}
