// Package store persists disc jobs, clips, rename batches and settings in
// SQLite. Each row keeps its full record as JSON next to the columns that are
// queried, so the model can grow without migrations for every field.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the database.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serializes read-modify-write of disc documents
}

var migrations = []string{
	`CREATE TABLE discs (
		id TEXT PRIMARY KEY,
		state TEXT NOT NULL,
		drive_id TEXT,
		created_at TEXT NOT NULL,
		doc TEXT NOT NULL
	);
	CREATE TABLE fingerprints (fp TEXT NOT NULL, disc_id TEXT NOT NULL, side TEXT NOT NULL, PRIMARY KEY (fp, disc_id));
	CREATE TABLE clips (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		disc_id TEXT NOT NULL,
		side TEXT NOT NULL,
		num INTEGER NOT NULL,
		doc TEXT NOT NULL
	);
	CREATE INDEX clips_disc ON clips(disc_id, side, num);
	CREATE TABLE previews (id TEXT PRIMARY KEY, created_at TEXT NOT NULL, doc TEXT NOT NULL);
	CREATE TABLE batches (id TEXT PRIMARY KEY, created_at TEXT NOT NULL, doc TEXT NOT NULL);
	CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
}

// Open opens or creates the database and runs pending migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion is the number of applied migrations.
func (s *Store) SchemaVersion() int {
	var v int
	_ = s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v
}

func (s *Store) migrate() error {
	v := s.SchemaVersion()
	if v > len(migrations) {
		return fmt.Errorf("database schema %d is newer than this build (%d); use camdvd rollback or upgrade", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Backup writes a consistent copy of the database to path.
func (s *Store) Backup(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

// NewDiscID returns a short random id like d-7f3a9c.
func NewDiscID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "d-" + hex.EncodeToString(b)
}

// NewID returns a random hex id.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ErrNotFound is returned for unknown ids.
var ErrNotFound = errors.New("not found")

// CreateDisc inserts a new disc.
func (s *Store) CreateDisc(d *Disc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	d.CreatedAt, d.UpdatedAt = now, now
	doc, _ := json.Marshal(d)
	_, err := s.db.Exec(`INSERT INTO discs (id, state, drive_id, created_at, doc) VALUES (?, ?, ?, ?, ?)`,
		d.ID, d.State, d.DriveID, now.UTC().Format(time.RFC3339Nano), doc)
	return err
}

// Disc loads a disc.
func (s *Store) Disc(id string) (*Disc, error) {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM discs WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var d Disc
	return &d, json.Unmarshal([]byte(doc), &d)
}

// UpdateDisc loads a disc, applies fn and saves it, atomically with respect
// to other updates. fn may return an error to abort.
func (s *Store) UpdateDisc(id string, fn func(*Disc) error) (*Disc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.Disc(id)
	if err != nil {
		return nil, err
	}
	if err := fn(d); err != nil {
		return d, err
	}
	d.UpdatedAt = time.Now()
	doc, _ := json.Marshal(d)
	if _, err := s.db.Exec(`UPDATE discs SET state = ?, drive_id = ?, doc = ? WHERE id = ?`, d.State, d.DriveID, doc, d.ID); err != nil {
		return nil, err
	}
	for _, sd := range d.Sides {
		if sd.Fingerprint != "" {
			if _, err := s.db.Exec(`INSERT OR IGNORE INTO fingerprints (fp, disc_id, side) VALUES (?, ?, ?)`, sd.Fingerprint, d.ID, sd.Letter); err != nil {
				return nil, err
			}
		}
	}
	return d, nil
}

// Discs lists discs, newest first; states filters when non-empty.
func (s *Store) Discs(states ...State) ([]*Disc, error) {
	rows, err := s.db.Query(`SELECT doc FROM discs ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	want := map[State]bool{}
	for _, st := range states {
		want[st] = true
	}
	var out []*Disc
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var d Disc
		if err := json.Unmarshal([]byte(doc), &d); err != nil {
			return nil, err
		}
		if len(want) == 0 || want[d.State] {
			out = append(out, &d)
		}
	}
	return out, rows.Err()
}

// DiscsByFingerprint returns discs that have a side with this fingerprint.
func (s *Store) DiscsByFingerprint(fp string) ([]*Disc, error) {
	rows, err := s.db.Query(`SELECT disc_id FROM fingerprints WHERE fp = ?`, fp)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []*Disc
	for _, id := range ids {
		if d, err := s.Disc(id); err == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// DeleteDisc removes a disc and its clips (the library is untouched).
func (s *Store) DeleteDisc(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range []string{`DELETE FROM clips WHERE disc_id = ?`, `DELETE FROM fingerprints WHERE disc_id = ?`, `DELETE FROM discs WHERE id = ?`} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// AddClip inserts a clip and sets its id.
func (s *Store) AddClip(c *Clip) error {
	doc, _ := json.Marshal(c)
	r, err := s.db.Exec(`INSERT INTO clips (disc_id, side, num, doc) VALUES (?, ?, ?, ?)`, c.DiscID, c.Side, c.Num, doc)
	if err != nil {
		return err
	}
	c.ID, err = r.LastInsertId()
	if err != nil {
		return err
	}
	return s.SaveClip(c)
}

// SaveClip writes a clip back.
func (s *Store) SaveClip(c *Clip) error {
	doc, _ := json.Marshal(c)
	_, err := s.db.Exec(`UPDATE clips SET side = ?, num = ?, doc = ? WHERE id = ?`, c.Side, c.Num, doc, c.ID)
	return err
}

// Clip loads one clip.
func (s *Store) Clip(id int64) (*Clip, error) {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM clips WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var c Clip
	return &c, json.Unmarshal([]byte(doc), &c)
}

// Clips lists a disc's clips in side and recording order.
func (s *Store) Clips(discID string) ([]*Clip, error) {
	rows, err := s.db.Query(`SELECT doc FROM clips WHERE disc_id = ? ORDER BY side, num`, discID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Clip
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var c Clip
		if err := json.Unmarshal([]byte(doc), &c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// DeleteClips removes a side's clips, for re-extraction.
func (s *Store) DeleteClips(discID, side string) error {
	_, err := s.db.Exec(`DELETE FROM clips WHERE disc_id = ? AND side = ?`, discID, side)
	return err
}

// SavePreview stores a rename preview.
func (s *Store) SavePreview(id string, v any) error {
	doc, _ := json.Marshal(v)
	_, err := s.db.Exec(`INSERT INTO previews (id, created_at, doc) VALUES (?, ?, ?)`, id, time.Now().UTC().Format(time.RFC3339Nano), doc)
	_, _ = s.db.Exec(`DELETE FROM previews WHERE created_at < ?`, time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339Nano))
	return err
}

// Preview loads a rename preview into v.
func (s *Store) Preview(id string, v any) error {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM previews WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(doc), v)
}

// SaveBatch stores or updates a rename batch.
func (s *Store) SaveBatch(b *RenameBatch) error {
	doc, _ := json.Marshal(b)
	_, err := s.db.Exec(`INSERT INTO batches (id, created_at, doc) VALUES (?, ?, ?) ON CONFLICT(id) DO UPDATE SET doc = excluded.doc`,
		b.ID, b.CreatedAt.UTC().Format(time.RFC3339Nano), doc)
	return err
}

// Batch loads a rename batch.
func (s *Store) Batch(id string) (*RenameBatch, error) {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM batches WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var b RenameBatch
	return &b, json.Unmarshal([]byte(doc), &b)
}

// Batches lists recent rename batches, newest first.
func (s *Store) Batches(limit int) ([]*RenameBatch, error) {
	rows, err := s.db.Query(`SELECT doc FROM batches ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RenameBatch
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var b RenameBatch
		if err := json.Unmarshal([]byte(doc), &b); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// Setting reads a setting into v; it reports false when unset.
func (s *Store) Setting(key string, v any) (bool, error) {
	var doc string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(doc), v)
}

// SetSetting stores a setting.
func (s *Store) SetSetting(key string, v any) error {
	doc, _ := json.Marshal(v)
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, doc)
	return err
}

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
