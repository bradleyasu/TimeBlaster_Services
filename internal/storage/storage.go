// Package storage is Timeblaster's durable state: alarm definitions and user
// settings, held in a SQLite database.
//
// SQLite is used rather than a JSON file because the write path must survive the
// power being pulled mid-write — this is an appliance with a physical off switch
// and no battery. The database is opened in WAL mode with synchronous=FULL, which
// makes every committed transaction durable at the cost of a few milliseconds we
// have in abundance.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"

	// modernc.org/sqlite is a pure-Go SQLite. It needs no cgo, so the daemon
	// cross-compiles from a development Mac to the Pi's arm64 with nothing but the
	// Go toolchain.
	_ "modernc.org/sqlite"
)

// Settings keys. They are collected here so no caller invents a stray key.
const (
	KeyTimezone  = "timezone"
	KeyClock24h  = "clock_24h"
	KeyDisplayOn = "display_on"
	// KeyLegacyDisplayBrightness is the pre-on/off key. It is still read once,
	// so a device that stored a brightness level keeps its display in the state
	// the user left it in rather than silently reverting to the default.
	KeyLegacyDisplayBrightness = "display_brightness"
	KeyDefaultSoundID          = "default_sound_id"
	KeyLastChannelNumber       = "last_channel_number"
	KeyRestoreChannel          = "restore_channel_on_boot"
	KeyChannelOverlayOn        = "channel_overlay_enabled"
	// KeyLastKnownVolume records the last volume the knob reported. It is a
	// diagnostic only and is deliberately never replayed at startup: the physical
	// potentiometer is the authority on volume.
	KeyLastKnownVolume = "alarm_volume_last_seen"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("storage: not found")

// Store is the durable-state interface the rest of the application depends on.
// Keeping it an interface is what lets the web layer and the scheduler be tested
// against an in-memory implementation.
type Store interface {
	alarm.Store

	GetSetting(key string) (string, bool, error)
	SetSetting(key, value string) error
	Settings() (map[string]string, error)

	Close() error
}

// DB is the SQLite-backed Store.
type DB struct {
	db   *sql.DB
	mu   sync.Mutex // serialises writes; SQLite allows one writer at a time
	path string
}

// Open opens (creating if necessary) the database at path and applies migrations.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("storage: empty database path")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("storage: creating %s: %w", dir, err)
		}
	}

	// _pragma parameters are applied to every pooled connection.
	dsn := "file:" + path + "?" + "_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: opening %s: %w", path, err)
	}
	// SQLite tolerates concurrent readers but one writer; a small pool keeps
	// lock contention predictable on a device with a handful of goroutines.
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("storage: connecting to %s: %w", path, err)
	}

	d := &DB{db: sqlDB, path: path}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Path reports the database file location, for logs and the health endpoint.
func (d *DB) Path() string { return d.path }

// Close releases the database.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// migration is one ordered schema change. Migrations are append-only: never edit
// an existing one, because devices in the field have already applied it.
type migration struct {
	version int
	stmts   []string
}

var migrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS alarms (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				label          TEXT    NOT NULL DEFAULT '',
				hour           INTEGER NOT NULL,
				minute         INTEGER NOT NULL,
				enabled        INTEGER NOT NULL DEFAULT 1,
				repeat_days    INTEGER NOT NULL DEFAULT 0,
				one_shot_date  TEXT    NOT NULL DEFAULT '',
				sound_id       TEXT    NOT NULL DEFAULT '',
				snooze_min     INTEGER NOT NULL DEFAULT 9,
				auto_stop_min  INTEGER NOT NULL DEFAULT 15,
				last_fired     INTEGER NOT NULL DEFAULT 0,
				created_at     INTEGER NOT NULL DEFAULT 0,
				updated_at     INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_alarms_enabled ON alarms(enabled)`,
			`CREATE TABLE IF NOT EXISTS settings (
				key        TEXT PRIMARY KEY,
				value      TEXT NOT NULL,
				updated_at INTEGER NOT NULL DEFAULT 0
			)`,
		},
	},
}

func (d *DB) migrate() error {
	if _, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("storage: creating migration table: %w", err)
	}

	var current int
	if err := d.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("storage: reading schema version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := d.db.Begin()
		if err != nil {
			return fmt.Errorf("storage: starting migration %d: %w", m.version, err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("storage: migration %d failed: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().Unix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("storage: recording migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("storage: committing migration %d: %w", m.version, err)
		}
	}
	return nil
}

// SchemaVersion reports the applied schema version, which the health endpoint
// surfaces so a partially upgraded device is obvious.
func (d *DB) SchemaVersion() (int, error) {
	var v int
	err := d.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

const alarmColumns = `id, label, hour, minute, enabled, repeat_days, one_shot_date,
	sound_id, snooze_min, auto_stop_min, last_fired, created_at, updated_at`

// Alarms returns every stored alarm, sorted for display.
func (d *DB) Alarms() ([]alarm.Alarm, error) {
	rows, err := d.db.Query(`SELECT ` + alarmColumns + ` FROM alarms`)
	if err != nil {
		return nil, fmt.Errorf("storage: listing alarms: %w", err)
	}
	defer rows.Close()

	var out []alarm.Alarm
	for rows.Next() {
		a, err := scanAlarm(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: reading alarms: %w", err)
	}
	alarm.SortAlarms(out)
	return out, nil
}

// Alarm returns one alarm by id.
func (d *DB) Alarm(id int64) (alarm.Alarm, error) {
	row := d.db.QueryRow(`SELECT `+alarmColumns+` FROM alarms WHERE id = ?`, id)
	a, err := scanAlarm(row)
	if errors.Is(err, sql.ErrNoRows) {
		return alarm.Alarm{}, fmt.Errorf("%w: alarm %d", ErrNotFound, id)
	}
	return a, err
}

// SaveAlarm inserts or updates an alarm and returns the stored row, including the
// assigned id.
func (d *DB) SaveAlarm(a alarm.Alarm) (alarm.Alarm, error) {
	if err := a.Validate(); err != nil {
		return alarm.Alarm{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	}

	if a.ID == 0 {
		res, err := d.db.Exec(`INSERT INTO alarms
			(label, hour, minute, enabled, repeat_days, one_shot_date, sound_id,
			 snooze_min, auto_stop_min, last_fired, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.Label, a.Hour, a.Minute, boolToInt(a.Enabled), a.RepeatDays, a.OneShotDate,
			a.SoundID, a.SnoozeMinutes, a.AutoStopMinutes,
			unixOrZero(a.LastFired), a.CreatedAt.Unix(), a.UpdatedAt.Unix())
		if err != nil {
			return alarm.Alarm{}, fmt.Errorf("storage: inserting alarm: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return alarm.Alarm{}, fmt.Errorf("storage: reading new alarm id: %w", err)
		}
		a.ID = id
		return a, nil
	}

	res, err := d.db.Exec(`UPDATE alarms SET
			label = ?, hour = ?, minute = ?, enabled = ?, repeat_days = ?,
			one_shot_date = ?, sound_id = ?, snooze_min = ?, auto_stop_min = ?,
			last_fired = ?, updated_at = ?
		WHERE id = ?`,
		a.Label, a.Hour, a.Minute, boolToInt(a.Enabled), a.RepeatDays, a.OneShotDate,
		a.SoundID, a.SnoozeMinutes, a.AutoStopMinutes,
		unixOrZero(a.LastFired), a.UpdatedAt.Unix(), a.ID)
	if err != nil {
		return alarm.Alarm{}, fmt.Errorf("storage: updating alarm %d: %w", a.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return alarm.Alarm{}, fmt.Errorf("%w: alarm %d", ErrNotFound, a.ID)
	}
	return a, nil
}

// DeleteAlarm removes an alarm.
func (d *DB) DeleteAlarm(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	res, err := d.db.Exec(`DELETE FROM alarms WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("storage: deleting alarm %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: alarm %d", ErrNotFound, id)
	}
	return nil
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanAlarm(s scanner) (alarm.Alarm, error) {
	var (
		a         alarm.Alarm
		enabled   int
		lastFired int64
		createdAt int64
		updatedAt int64
	)
	err := s.Scan(&a.ID, &a.Label, &a.Hour, &a.Minute, &enabled, &a.RepeatDays,
		&a.OneShotDate, &a.SoundID, &a.SnoozeMinutes, &a.AutoStopMinutes,
		&lastFired, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return alarm.Alarm{}, err
		}
		return alarm.Alarm{}, fmt.Errorf("storage: scanning alarm: %w", err)
	}
	a.Enabled = enabled != 0
	a.LastFired = timeOrZero(lastFired)
	a.CreatedAt = timeOrZero(createdAt)
	a.UpdatedAt = timeOrZero(updatedAt)
	return a, nil
}

// GetSetting reads a setting, reporting whether it was present.
func (d *DB) GetSetting(key string) (string, bool, error) {
	var v string
	err := d.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("storage: reading setting %q: %w", key, err)
	}
	return v, true, nil
}

// SetSetting writes a setting, replacing any existing value.
func (d *DB) SetSetting(key, value string) error {
	if key == "" {
		return errors.New("storage: empty setting key")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("storage: writing setting %q: %w", key, err)
	}
	return nil
}

// Settings returns every stored setting.
func (d *DB) Settings() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("storage: listing settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("storage: scanning setting: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// --- typed setting helpers ---------------------------------------------------

// GetBool reads a boolean setting, returning def when it is absent or malformed.
func GetBool(s Store, key string, def bool) bool {
	v, ok, err := s.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// SetBool writes a boolean setting.
func SetBool(s Store, key string, v bool) error {
	return s.SetSetting(key, strconv.FormatBool(v))
}

// GetInt reads an integer setting, returning def when it is absent or malformed.
func GetInt(s Store, key string, def int) int {
	v, ok, err := s.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// SetInt writes an integer setting.
func SetInt(s Store, key string, v int) error {
	return s.SetSetting(key, strconv.Itoa(v))
}

// GetString reads a string setting, returning def when it is absent or empty.
func GetString(s Store, key, def string) string {
	v, ok, err := s.GetSetting(key)
	if err != nil || !ok || v == "" {
		return def
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

var _ Store = (*DB)(nil)
