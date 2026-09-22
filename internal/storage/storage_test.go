package storage

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "sub", "timeblaster.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenCreatesDirectoriesAndSchema(t *testing.T) {
	db := openTestDB(t)
	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != len(migrations) {
		t.Errorf("schema version: got %d want %d", v, len(migrations))
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeblaster.db")
	for range 3 {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		db.Close()
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("expected an error")
	}
}

func TestAlarmRoundTrip(t *testing.T) {
	db := openTestDB(t)

	a := alarm.New(6, 30)
	a.Label = "Weekday wake"
	a.RepeatDays = alarm.Weekdays
	a.SoundID = "alarm2"
	a.SnoozeMinutes = 7
	a.AutoStopMinutes = 20

	saved, err := db.SaveAlarm(a)
	if err != nil {
		t.Fatalf("SaveAlarm: %v", err)
	}
	if saved.ID == 0 {
		t.Fatal("no id assigned")
	}

	got, err := db.Alarm(saved.ID)
	if err != nil {
		t.Fatalf("Alarm: %v", err)
	}
	if got.Label != a.Label || got.Hour != 6 || got.Minute != 30 ||
		got.RepeatDays != alarm.Weekdays || got.SoundID != "alarm2" ||
		got.SnoozeMinutes != 7 || got.AutoStopMinutes != 20 || !got.Enabled {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stored: %+v", got)
	}
	// A never-fired alarm reads back as a zero time, not as 1970.
	if !got.LastFired.IsZero() {
		t.Errorf("LastFired should be zero, got %v", got.LastFired)
	}
}

func TestSaveAlarmUpdatesInPlace(t *testing.T) {
	db := openTestDB(t)
	saved, err := db.SaveAlarm(alarm.New(6, 30))
	if err != nil {
		t.Fatal(err)
	}

	saved.Hour = 7
	saved.Enabled = false
	saved.LastFired = time.Unix(1758220642, 0)
	updated, err := db.SaveAlarm(saved)
	if err != nil {
		t.Fatalf("SaveAlarm: %v", err)
	}
	if updated.ID != saved.ID {
		t.Fatalf("update created a new row: %d vs %d", updated.ID, saved.ID)
	}

	got, err := db.Alarm(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hour != 7 || got.Enabled {
		t.Errorf("update lost: %+v", got)
	}
	if !got.LastFired.Equal(time.Unix(1758220642, 0)) {
		t.Errorf("LastFired: %v", got.LastFired)
	}

	all, err := db.Alarms()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("expected one alarm, got %d", len(all))
	}
}

func TestSaveAlarmRejectsInvalid(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SaveAlarm(alarm.Alarm{Hour: 99}); err == nil {
		t.Error("an invalid alarm must not reach the database")
	}
}

func TestSaveAlarmUnknownIDIsNotFound(t *testing.T) {
	db := openTestDB(t)
	a := alarm.New(6, 0)
	a.ID = 4242
	if _, err := db.SaveAlarm(a); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestDeleteAlarm(t *testing.T) {
	db := openTestDB(t)
	saved, err := db.SaveAlarm(alarm.New(6, 30))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAlarm(saved.ID); err != nil {
		t.Fatalf("DeleteAlarm: %v", err)
	}
	if _, err := db.Alarm(saved.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Alarm after delete: %v", err)
	}
	if err := db.DeleteAlarm(saved.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
}

func TestAlarmsAreSorted(t *testing.T) {
	db := openTestDB(t)
	for _, hm := range [][2]int{{7, 0}, {6, 0}, {6, 30}} {
		if _, err := db.SaveAlarm(alarm.New(hm[0], hm[1])); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.Alarms()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"06:00", "06:30", "07:00"}
	for i, w := range want {
		if got[i].TimeString() != w {
			t.Fatalf("order: %v", times(got))
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	db := openTestDB(t)

	if _, ok, err := db.GetSetting(KeyTimezone); err != nil || ok {
		t.Fatalf("absent setting: ok=%v err=%v", ok, err)
	}
	if err := db.SetSetting(KeyTimezone, "America/New_York"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := db.GetSetting(KeyTimezone)
	if err != nil || !ok || v != "America/New_York" {
		t.Fatalf("got %q, %v, %v", v, ok, err)
	}

	// Writing again replaces rather than duplicating.
	if err := db.SetSetting(KeyTimezone, "UTC"); err != nil {
		t.Fatal(err)
	}
	all, err := db.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[KeyTimezone] != "UTC" {
		t.Errorf("settings: %v", all)
	}

	if err := db.SetSetting("", "x"); err == nil {
		t.Error("an empty key must be rejected")
	}
}

func TestTypedSettingHelpers(t *testing.T) {
	db := openTestDB(t)

	if got := GetBool(db, KeyClock24h, true); !got {
		t.Error("missing bool should return the default")
	}
	if err := SetBool(db, KeyClock24h, false); err != nil {
		t.Fatal(err)
	}
	if got := GetBool(db, KeyClock24h, true); got {
		t.Error("stored bool not read back")
	}

	if got := GetInt(db, KeyLegacyDisplayBrightness, 75); got != 75 {
		t.Errorf("missing int: %d", got)
	}
	if err := SetInt(db, KeyLegacyDisplayBrightness, 30); err != nil {
		t.Fatal(err)
	}
	if got := GetInt(db, KeyLegacyDisplayBrightness, 75); got != 30 {
		t.Errorf("stored int: %d", got)
	}

	// A corrupt value falls back to the default rather than failing a boot.
	if err := db.SetSetting(KeyLegacyDisplayBrightness, "bright-ish"); err != nil {
		t.Fatal(err)
	}
	if got := GetInt(db, KeyLegacyDisplayBrightness, 75); got != 75 {
		t.Errorf("malformed int should fall back: %d", got)
	}
	if err := db.SetSetting(KeyClock24h, "yes-please"); err != nil {
		t.Fatal(err)
	}
	if got := GetBool(db, KeyClock24h, true); !got {
		t.Error("malformed bool should fall back")
	}

	if got := GetString(db, KeyDefaultSoundID, "alarm1"); got != "alarm1" {
		t.Errorf("missing string: %q", got)
	}
	if err := db.SetSetting(KeyDefaultSoundID, ""); err != nil {
		t.Fatal(err)
	}
	if got := GetString(db, KeyDefaultSoundID, "alarm1"); got != "alarm1" {
		t.Errorf("empty string should fall back: %q", got)
	}
}

func TestConcurrentWritesAreSerialised(t *testing.T) {
	db := openTestDB(t)
	var wg sync.WaitGroup
	errCh := make(chan error, 32)

	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := db.SaveAlarm(alarm.New(i%24, 0)); err != nil {
				errCh <- err
			}
			if err := db.SetSetting("k", "v"); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent write failed: %v", err)
	}

	got, err := db.Alarms()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 16 {
		t.Errorf("expected 16 alarms, got %d", len(got))
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeblaster.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := alarm.New(6, 30)
	a.Label = "Persisted"
	if _, err := db.SaveAlarm(a); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(KeyTimezone, "Europe/London"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	alarms, err := db2.Alarms()
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Label != "Persisted" {
		t.Errorf("alarms after reopen: %+v", alarms)
	}
	if got := GetString(db2, KeyTimezone, ""); got != "Europe/London" {
		t.Errorf("setting after reopen: %q", got)
	}
}

func TestCloseIsSafeOnNil(t *testing.T) {
	var db *DB
	if err := db.Close(); err != nil {
		t.Errorf("Close on nil: %v", err)
	}
}

func TestMemoryStoreMatchesDBSemantics(t *testing.T) {
	m := NewMemory()

	saved, err := m.SaveAlarm(alarm.New(6, 30))
	if err != nil || saved.ID == 0 {
		t.Fatalf("SaveAlarm: %+v, %v", saved, err)
	}
	saved.Hour = 8
	if _, err := m.SaveAlarm(saved); err != nil {
		t.Fatal(err)
	}
	all, err := m.Alarms()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Hour != 8 {
		t.Errorf("alarms: %+v", all)
	}

	if err := m.DeleteAlarm(saved.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteAlarm(saved.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
	if _, err := m.SaveAlarm(alarm.Alarm{Hour: 99}); err == nil {
		t.Error("invalid alarm accepted")
	}

	if err := m.SetSetting("k", "v"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := m.GetSetting("k"); !ok || v != "v" {
		t.Errorf("setting: %q, %v", v, ok)
	}
	if err := m.Close(); err != nil {
		t.Error(err)
	}
}

func times(as []alarm.Alarm) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.TimeString()
	}
	return out
}
