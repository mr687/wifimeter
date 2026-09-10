package main

// Delta accounting, which is where the correctness of this tool lives.
//
// The kernel counters are cumulative and monotonic while the interface stays
// up. Everything that breaks that monotonicity has to be detected and dropped:
// a counter reset, a network switch, or a laptop that spent an hour asleep with
// a download running. A dropped sample costs ten seconds of data. A phantom
// multi-gigabyte sample ruins the day's totals, so the gate leans hard toward
// dropping.

import "time"

// DefaultInterval is the sampling period the launchd agent runs at.
const DefaultInterval = 10 * time.Second

// sleepFactor is how far past the interval a gap may stretch before we call it
// sleep or wake rather than a slow tick.
const sleepFactor = 5

// RollupInterval is how often the daemon folds complete days into the daily
// tables. Hourly is far more often than a day closes, and the work per pass is
// one day's worth of rows.
const RollupInterval = time.Hour

// MaxRollupLookback bounds a rollup pass so an agent starting on a database
// with years of unrolled history does not walk all of it in one go. Each pass
// reaches this far back; earlier days wait for the next pass.
const MaxRollupLookback = 7 * 24 * time.Hour

// RawRetentionDays is how long unrolled samples are kept. Everything older is
// served from daily_usage, which is why the rollup must cover a day before
// retention can drop it.
const RawRetentionDays = 7

// RetentionCut is the oldest timestamp still served from raw samples.
func RetentionCut(now time.Time) time.Time {
	return startOfDay(now).AddDate(0, 0, -(RawRetentionDays - 1))
}

// Reason marks why a sample was discarded. Keeping the reason lets doctor
// report a discard rate and separate sleep losses from switch losses, so a
// silently lossy collector does not look the same as a clean one.
type Reason string

const (
	ReasonNone      Reason = ""
	ReasonLinkDown  Reason = "link-down"
	ReasonNoGateway Reason = "no-gateway"
	ReasonFirst     Reason = "first-sample"
	ReasonReset     Reason = "counter-reset"
	ReasonSwitch    Reason = "network-switch"
	ReasonSleep     Reason = "sleep/wake"
)

// Reading is one sampled view of the interface.
type Reading struct {
	Counters
	At     time.Time
	FPKey  string
	LinkUp bool
	Err    error // fingerprint failure: no gateway, or gateway not yet in ARP
}

// Sample is what gets stored. Discarded samples are stored too, with zero
// deltas and a reason, rather than being dropped silently.
type Sample struct {
	At     time.Time
	FPKey  string
	RX     uint64
	TX     uint64
	Valid  bool
	Reason Reason
}

// Baseline is the previous reading. It lives in memory only; a restart starts
// empty, and ReasonFirst discards that first interval so it cannot attribute
// the interface's whole history to one sample.
type Baseline struct {
	Counters
	Set   bool
	At    time.Time
	FPKey string
}

// Apply folds a reading into the baseline and returns the sample to store. It
// mutates the baseline deliberately: every path out of here leaves the baseline
// holding the current reading, so the next interval always has something to
// compare against and a bad interval cannot poison the next one.
func (b *Baseline) Apply(r Reading, interval time.Duration) Sample {
	s := Sample{At: r.At, FPKey: r.FPKey}

	switch {
	case !r.LinkUp:
		s.Reason = ReasonLinkDown
	case r.Err != nil:
		s.Reason = ReasonNoGateway
	case !b.Set:
		// Nothing to compare against yet. Emitting a delta here would
		// attribute the interface's whole history to one interval.
		s.Reason = ReasonFirst
	case r.FPKey != b.FPKey:
		// Counters survive a network switch, but the bytes from the last
		// interval belong to the network we just left and cannot be split
		// after the fact.
		s.Reason = ReasonSwitch
	case r.RX < b.RX || r.TX < b.TX:
		s.Reason = ReasonReset
	case r.At.Sub(b.At) > sleepFactor*interval:
		s.Reason = ReasonSleep
	default:
		s.RX, s.TX = r.RX-b.RX, r.TX-b.TX
		s.Valid = true
	}

	b.Set = true
	b.At = r.At
	b.Counters = r.Counters
	b.FPKey = r.FPKey
	return s
}
