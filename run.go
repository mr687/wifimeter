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

// runDaemon samples until told to stop. It keeps its baseline in memory: under
// launchd the process is restarted freely, and the first sample after a start
// is discarded by the gate, so a restart never produces a giant first delta.
func runDaemon(store *Store, interval time.Duration, iface string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var baseline Baseline
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("sampling %s every %s", iface, interval)

	for {
		if err := sampleOnce(ctx, store, &baseline, iface); err != nil && ctx.Err() == nil {
			log.Printf("sample: %v", err)
		}

		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return nil
		case <-ticker.C:
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
