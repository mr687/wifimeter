package main

// The daemon loop.

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// runDaemon samples until told to stop. The baseline is in-memory, so a
// restart begins with none: the gate discards the first sample per start,
// which is what keeps a respawn from producing a giant first delta.
func runDaemon(store *Store, interval time.Duration, iface string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The agent restarts often under KeepAlive, and each start is a cheap
	// chance to fold the WAL back in before it grows to hundreds of megabytes.
	if err := store.Checkpoint(); err != nil {
		log.Printf("checkpoint: %v", err)
	}

	var baseline Baseline
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	rollup := time.NewTicker(RollupInterval)
	defer rollup.Stop()

	log.Printf("sampling %s every %s", iface, interval)
	for {
		if err := sampleOnce(ctx, store, &baseline, iface); err != nil && ctx.Err() == nil {
			log.Printf("sample: %v", err)
		}

		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return nil
		case <-rollup.C:
			if err := rollupClosedDays(store, time.Now()); err != nil && ctx.Err() == nil {
				log.Printf("rollup: %v", err)
			}
		case <-ticker.C:
		}
	}
}

// rollupClosedDays folds every complete past day into the daily tables. Only
// days entirely behind the retention window are eligible: today is still
// accumulating, and raw rows newer than that are what the report reads.
func rollupClosedDays(store *Store, now time.Time) error {
	today := startOfDay(now)
	for d := today.AddDate(0, 0, -1); ; d = d.AddDate(0, 0, -1) {
		if today.Sub(d) > MaxRollupLookback {
			return nil
		}
		if err := store.RollupDay(d); err != nil {
			return err
		}
	}
}

func sampleOnce(ctx context.Context, store *Store, baseline *Baseline, iface string) error {
	reading, fp, err := SampleLink(ctx, iface)
	if err != nil {
		return err
	}

	samp := baseline.Apply(reading, DefaultInterval)

	if err := store.InsertSample(samp); err != nil {
		return err
	}
	// Only record the network when we could actually identify it.
	if fp.Complete() {
		if err := store.UpsertNetwork(fp, reading.At); err != nil {
			return err
		}
	}
	return nil
}
