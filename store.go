package main

// SQLite storage.
//
// SQLite rather than a hand-rolled append-only file: it already solves crash
// safety and the rollup queries, and modernc.org/sqlite is pure Go so the
// binary keeps building with CGO_ENABLED=0 and no C toolchain.

import (
	"database/sql"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

// DefaultDBPath is where the launchd agent keeps its database.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "WifiMeter", "usage.db"), nil
}

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// Open creates the database and applies the schema. The parent directory is
// created because install runs before the agent ever samples.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// One writer, held for the life of the process. The sampler goroutine is
	// the only writer, so serialising it here removes any question of lock
	// contention. WAL keeps report reads from blocking it.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	// WAL: report runs as a separate process and must never see a torn DB or
	// block the writer.
	if _, err := s.db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return fmt.Errorf("enabling WAL: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA synchronous=NORMAL;`); err != nil {
		return fmt.Errorf("setting synchronous: %w", err)
	}
	// Wait instead of failing at once when another writer holds the lock.
	// SetMaxOpenConns(1) only serialises connections inside this process, so
	// it does nothing against a second one.
	if _, err := s.db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		return fmt.Errorf("setting busy_timeout: %w", err)
	}

	const schema = `
CREATE TABLE IF NOT EXISTS samples (
  id     INTEGER PRIMARY KEY,
  ts     INTEGER NOT NULL,
  fp     TEXT    NOT NULL,
  rx     INTEGER NOT NULL,
  tx     INTEGER NOT NULL,
  valid  INTEGER NOT NULL,
  reason TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS networks (
  fp         TEXT PRIMARY KEY,
  label      TEXT,
  first_seen INTEGER,
  last_seen  INTEGER,
  gw_ip      TEXT,
  subnet     TEXT,
  gw_mac     TEXT,
  vendor     TEXT
);

CREATE INDEX IF NOT EXISTS idx_samples_fp_ts ON samples(fp, ts);

CREATE TABLE IF NOT EXISTS daily_usage (
  fp      TEXT    NOT NULL,
  day     TEXT    NOT NULL,
  rx      INTEGER NOT NULL,
  tx      INTEGER NOT NULL,
  samples INTEGER NOT NULL,
  PRIMARY KEY (fp, day)
);

CREATE TABLE IF NOT EXISTS daily_discards (
  day    TEXT    NOT NULL,
  fp     TEXT    NOT NULL,
  reason TEXT    NOT NULL,
  count  INTEGER NOT NULL,
  PRIMARY KEY (day, fp, reason)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	return s.migrate()
}

// schemaVersion tracks which migrations have run. An older binary ignores it
// and applies whatever it knows, which is safe because every change so far is
// additive: old code reads the columns it expects and never touches the rest.
const schemaVersion = 1

// migrate runs schema changes an earlier version did not have. Progressive and
// additive only: nothing here may drop or rewrite the samples table, because a
// downgraded binary would then CREATE TABLE IF NOT EXISTS an empty one and
// report nothing rather than failing visibly.
//
// The reason check reads table_info rather than user_version because databases
// created before this mechanism existed sit at user_version 0 with the column
// already present, and must not be altered twice.
func (s *Store) migrate() error {
	rows, err := s.db.Query(`PRAGMA table_info(samples)`)
	if err != nil {
		return fmt.Errorf("reading samples schema: %w", err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("reading samples column: %w", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if !slices.Contains(cols, "reason") {
		// Rows written before this column existed lose nothing: they had no
		// reason recorded either way, and the empty string reads as ReasonNone.
		if _, err := s.db.Exec(`ALTER TABLE samples ADD COLUMN reason TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("adding reason column: %w", err)
		}
	}

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if version < schemaVersion {
		// Not a parameter: PRAGMA user_version takes no bound values.
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return fmt.Errorf("setting user_version: %w", err)
		}
	}
	return nil
}

// dayKeyOf buckets a timestamp the same way Daily does, so a rolled-up day and
// a freshly sampled one agree at DST boundaries and across machines. Doing this
// in Go rather than with SQLite's date() matters: localtime in the database
// depends on the TZ of whoever opened it, which is not stable across runs.
func dayKeyOf(t time.Time) string {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Format("2006-01-02")
}

// Rollup recomputes one day from the raw samples and writes the result.
// Replacing rather than adding on conflict is what makes it safe to run again:
// an additive upsert counts the same samples once per pass.
//
// It deletes nothing; RetainDay does that, in the same transaction as the
// rollup so a crash cannot lose a day that was dropped but never folded in.
func (s *Store) RollupDay(day time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning rollup: %w", err)
	}
	defer tx.Rollback()

	if err := s.rollupTx(tx, day); err != nil {
		return err
	}
	return tx.Commit()
}

// RetainDay rolls a day up and then drops its raw rows. The two share one
// transaction: committed together, the daily tables are the only copy of that
// day, and rolled back together, nothing is lost.
//
// Only call this for days already past the retention window. Deleting a day
// that is still accumulating would leave a partial total behind.
func (s *Store) RetainDay(day time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning retention: %w", err)
	}
	defer tx.Rollback()

	if err := s.rollupTx(tx, day); err != nil {
		return err
	}

	from := startOfDay(day).Unix()
	to := startOfDay(day).AddDate(0, 0, 1).Unix()
	if _, err := tx.Exec(`DELETE FROM samples WHERE ts >= ? AND ts < ?`, from, to); err != nil {
		return fmt.Errorf("dropping raw rows for %s: %w", dayKeyOf(day), err)
	}

	return tx.Commit()
}

func (s *Store) rollupTx(tx *sql.Tx, day time.Time) error {
	key := dayKeyOf(day)
	from := startOfDay(day).Unix()
	to := startOfDay(day).AddDate(0, 0, 1).Unix()

	if _, err := tx.Exec(`
INSERT INTO daily_usage (fp, day, rx, tx, samples)
SELECT fp, ?1, SUM(rx), SUM(tx), COUNT(*)
FROM samples
WHERE ts >= ?2 AND ts < ?3 AND valid = 1
GROUP BY fp
ON CONFLICT(fp, day) DO UPDATE SET
  rx      = excluded.rx,
  tx      = excluded.tx,
  samples = excluded.samples`,
		key, from, to); err != nil {
		return fmt.Errorf("rolling up usage for %s: %w", key, err)
	}

	if _, err := tx.Exec(`
INSERT INTO daily_discards (day, fp, reason, count)
SELECT ?1, fp, reason, COUNT(*)
FROM samples
WHERE ts >= ?2 AND ts < ?3 AND valid = 0
GROUP BY fp, reason
ON CONFLICT(day, fp, reason) DO UPDATE SET
  count = excluded.count`,
		key, from, to); err != nil {
		return fmt.Errorf("rolling up discards for %s: %w", key, err)
	}
	return nil
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// Checkpoint folds the WAL into the database file and truncates it. Without
// this the sidecar grows unbounded: an uncheckpointed WAL held roughly half
// the on-disk size of a year's samples.
func (s *Store) Checkpoint() error {
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpointing wal: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// InsertSample records one sample. Discarded samples are recorded too, with
// zero deltas and their reason, so doctor can report a discard rate instead of
// a lossy collector looking identical to a clean one.
func (s *Store) InsertSample(samp Sample) error {
	valid := 0
	if samp.Valid {
		valid = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO samples (ts, fp, rx, tx, valid, reason) VALUES (?, ?, ?, ?, ?, ?)`,
		samp.At.Unix(), samp.FPKey, samp.RX, samp.TX, valid, string(samp.Reason),
	)
	return err
}

// UpsertNetwork records that a fingerprint was seen, and fills in its
// components the first time. last_seen advances on every sighting.
func (s *Store) UpsertNetwork(fp Fingerprint, at time.Time) error {
	_, err := s.db.Exec(`
INSERT INTO networks (fp, first_seen, last_seen, gw_ip, subnet, gw_mac, vendor)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(fp) DO UPDATE SET
  last_seen = excluded.last_seen,
  gw_ip     = COALESCE(networks.gw_ip, excluded.gw_ip),
  subnet    = COALESCE(networks.subnet, excluded.subnet),
  gw_mac    = COALESCE(networks.gw_mac, excluded.gw_mac),
  vendor    = COALESCE(networks.vendor, excluded.vendor)`,
		fp.Key(), at.Unix(), at.Unix(), fp.GatewayIP, fp.Subnet, fp.GatewayMAC, fp.Vendor,
	)
	return err
}

// SetLabel names a fingerprint. The label is what report prints in place of
// the raw key.
func (s *Store) SetLabel(fpKey, label string) error {
	res, err := s.db.Exec(`
INSERT INTO networks (fp, label, first_seen, last_seen)
VALUES (?, ?, ?, ?)
ON CONFLICT(fp) DO UPDATE SET label = excluded.label`,
		fpKey, label, time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return errors.New("network not found")
	}
	return nil
}

// Labels returns every named fingerprint, for report to display.
func (s *Store) Labels() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT fp, label FROM networks WHERE label IS NOT NULL AND label != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var fp, label string
		if err := rows.Scan(&fp, &label); err != nil {
			return nil, err
		}
		out[fp] = label
	}
	return out, rows.Err()
}

// SamplesSince returns every sample at or after the given time, oldest first.
func (s *Store) SamplesSince(from time.Time) ([]Sample, error) {
	rows, err := s.db.Query(
		`SELECT ts, fp, rx, tx, valid, reason FROM samples WHERE ts >= ? ORDER BY ts`,
		from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Sample
	for rows.Next() {
		var (
			ts     int64
			valid  int
			reason string
			s      Sample
		)
		if err := rows.Scan(&ts, &s.FPKey, &s.RX, &s.TX, &valid, &reason); err != nil {
			return nil, err
		}
		s.At = time.Unix(ts, 0).UTC()
		s.Valid = valid != 0
		s.Reason = Reason(reason)
		out = append(out, s)
	}
	return out, rows.Err()
}

// uncoveredRaw returns raw samples for days no rollup has claimed. Rolled days
// are served from daily_usage instead, so counting both would double them.
func (s *Store) uncoveredRaw(rolled []RolledDay) ([]Sample, error) {
	raw, err := s.SamplesSince(time.Unix(0, 0))
	if err != nil {
		return nil, err
	}
	if len(rolled) == 0 {
		return raw, nil
	}

	covered := make(map[[2]string]bool, len(rolled))
	for _, d := range rolled {
		covered[[2]string{d.FPKey, d.Day}] = true
	}

	loc := time.Now().Location()
	out := make([]Sample, 0, len(raw))
	for _, sm := range raw {
		if covered[[2]string{sm.FPKey, dayKeyOf(sm.At.In(loc))}] {
			continue
		}
		out = append(out, sm)
	}
	return out, nil
}

// SamplesForReport returns everything the report should count. A rolled-up
// (fp, day) supersedes the raw rows for that day, and every other raw row is
// passed through, so days that have not been rolled up yet are still reported
// from source rather than silently vanishing.
//
// No cutoff on the raw side: retention is what bounds it, and until then
// filtering by date would drop history the rollup has not reached.
//
// A rolled day becomes one valid sample stamped at local noon. Daily() buckets
// by calendar day, so the time only has to land inside the right one, and noon
// is clear of any DST shift a midnight stamp could straddle.
func (s *Store) SamplesForReport() ([]Sample, error) {
	rolled, err := s.DailyUsage()
	if err != nil {
		return nil, err
	}
	raw, err := s.uncoveredRaw(rolled)
	if err != nil {
		return nil, err
	}

	loc := time.Now().Location()
	out := make([]Sample, 0, len(raw)+len(rolled))
	out = append(out, raw...)
	for _, d := range rolled {
		day, err := time.ParseInLocation("2006-01-02", d.Day, loc)
		if err != nil {
			return nil, fmt.Errorf("parsing rolled day %q: %w", d.Day, err)
		}
		out = append(out, Sample{
			At:    day.Add(12 * time.Hour),
			FPKey: d.FPKey,
			RX:    d.RX,
			TX:    d.TX,
			Valid: true,
		})
	}

	// Discarded samples for rolled days live only in daily_discards, so they
	// have to come back as samples or the footer cannot count or name them.
	discards, err := s.DailyDiscardsByDay()
	if err != nil {
		return nil, err
	}
	for _, d := range discards {
		day, err := time.ParseInLocation("2006-01-02", d.Day, loc)
		if err != nil {
			return nil, fmt.Errorf("parsing rolled day %q: %w", d.Day, err)
		}
		for range d.Count {
			out = append(out, Sample{
				At:     day.Add(12 * time.Hour),
				FPKey:  d.FPKey,
				Reason: Reason(d.Reason),
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// DiscardStats counts samples and discards for the report footer, in SQL.
// Rolled days contribute their stored counts rather than their collapsed rows,
// so the rate stays true as history ages: counting rows would climb toward
// 100% because a rolled day renders as one sample however many produced it.
func (s *Store) DiscardStats() (total, discarded int, err error) {
	row := s.db.QueryRow(`
SELECT
  (SELECT COUNT(*) FROM samples)
  + (SELECT COALESCE(SUM(samples), 0) FROM daily_usage)
  + (SELECT COALESCE(SUM(count), 0) FROM daily_discards),
  (SELECT COUNT(*) FROM samples WHERE valid = 0)
  + (SELECT COALESCE(SUM(count), 0) FROM daily_discards)`)
	err = row.Scan(&total, &discarded)
	return total, discarded, err
}

// RolledDay is one network's usage for one local day, as stored in
// daily_usage.
type RolledDay struct {
	FPKey   string
	Day     string
	RX      uint64
	TX      uint64
	Samples int
}

// DailyUsage returns every rolled-up day. Populated from Phase 2 onward and
// empty until a rollup runs; nothing reads it yet.
func (s *Store) DailyUsage() ([]RolledDay, error) {
	rows, err := s.db.Query(`SELECT fp, day, rx, tx, samples FROM daily_usage ORDER BY day, fp`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RolledDay
	for rows.Next() {
		var d RolledDay
		if err := rows.Scan(&d.FPKey, &d.Day, &d.RX, &d.TX, &d.Samples); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RolledDiscard is one day's discards for one network and reason.
type RolledDiscard struct {
	Day    string
	FPKey  string
	Reason string
	Count  int
}

// DailyDiscardsByDay returns rolled-up discards with their day, so a reader can
// rebuild the samples they stand for.
func (s *Store) DailyDiscardsByDay() ([]RolledDiscard, error) {
	rows, err := s.db.Query(`SELECT day, fp, reason, count FROM daily_discards ORDER BY day, fp, reason`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RolledDiscard
	for rows.Next() {
		var d RolledDiscard
		if err := rows.Scan(&d.Day, &d.FPKey, &d.Reason, &d.Count); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DailyDiscards returns rolled-up discard counts keyed by reason.
func (s *Store) DailyDiscards() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT reason, SUM(count) FROM daily_discards GROUP BY reason`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var (
			reason string
			count  int
		)
		if err := rows.Scan(&reason, &count); err != nil {
			return nil, err
		}
		out[reason] = count
	}
	return out, rows.Err()
}

// OldestSampleDay returns the day of the earliest raw sample, or the zero time
// when there are none. Retention walks forward from here so each pass makes
// progress on the backlog instead of re-covering the same recent days.
func (s *Store) OldestSampleDay() (time.Time, error) {
	var ts sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(ts) FROM samples`).Scan(&ts); err != nil {
		return time.Time{}, err
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	return time.Unix(ts.Int64, 0), nil
}
