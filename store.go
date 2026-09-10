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
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "modernc.org/sqlite"
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
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	return s.migrate()
}

// migrate adds columns to databases created by an earlier version. The schema
// above uses CREATE TABLE IF NOT EXISTS, which silently skips a table that
// already exists and so can never add a column to it.
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

// DiscardStats counts samples and discards, for the report footer.
func (s *Store) DiscardStats() (total, discarded int, err error) {
	row := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(valid = 0), 0) FROM samples`)
	err = row.Scan(&total, &discarded)
	return total, discarded, err
}
