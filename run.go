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

// rollupClosedDays folds complete past days into the daily tables and drops the
// raw rows for any of them already past the retention window.
//
// It walks back from the oldest raw day rather than from yesterday, so each pass
// makes progress on history instead of re-covering the same recent week: a fixed
// lookback from today would never reach a database with months of backlog.
//
// Deleting and rolling up share a transaction, so a crash mid-pass cannot leave
// a day dropped but never folded in. MaxRollupLookback bounds each pass, so one
// call stays short no matter how much history is waiting.
func rollupClosedDays(store *Store, now time.Time) error {
	today := startOfDay(now)
	cut := RetentionCut(now)

	oldest, err := store.OldestSampleDay()
	if err != nil {
		return err
	}
	if oldest.IsZero() {
		return nil
	}

	for d := startOfDay(oldest); d.Before(today); d = d.AddDate(0, 0, 1) {
		// Days still inside the retention window are folded in but kept: the
		// report reads them from source, and they are not complete yet.
		if d.Before(cut) {
			if err := store.RetainDay(d); err != nil {
				return err
			}
			continue
		}
		if err := store.RollupDay(d); err != nil {
			return err
		}
	}
	return nil
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
