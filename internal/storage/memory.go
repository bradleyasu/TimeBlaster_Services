package storage

import (
	"fmt"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
)

// Memory is an in-memory Store. It backs tests in packages that cannot open a
// database, and it is what the daemon falls back to if the SQLite file cannot be
// opened at all — a Timeblaster that rings alarms but forgets them on reboot is
// still far better than one that refuses to start.
type Memory struct {
	mu       sync.Mutex
	nextID   int64
	alarms   map[int64]alarm.Alarm
	settings map[string]string
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{alarms: map[int64]alarm.Alarm{}, settings: map[string]string{}}
}

// Alarms returns every alarm, sorted for display.
func (m *Memory) Alarms() ([]alarm.Alarm, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]alarm.Alarm, 0, len(m.alarms))
	for _, a := range m.alarms {
		out = append(out, a)
	}
	alarm.SortAlarms(out)
	return out, nil
}

// SaveAlarm inserts or updates an alarm.
func (m *Memory) SaveAlarm(a alarm.Alarm) (alarm.Alarm, error) {
	if err := a.Validate(); err != nil {
		return alarm.Alarm{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if a.ID == 0 {
		m.nextID++
		a.ID = m.nextID
	} else if _, ok := m.alarms[a.ID]; !ok && a.ID > m.nextID {
		m.nextID = a.ID
	}
	m.alarms[a.ID] = a
	return a, nil
}

// DeleteAlarm removes an alarm.
func (m *Memory) DeleteAlarm(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.alarms[id]; !ok {
		return fmt.Errorf("%w: alarm %d", ErrNotFound, id)
	}
	delete(m.alarms, id)
	return nil
}

// GetSetting reads a setting.
func (m *Memory) GetSetting(key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.settings[key]
	return v, ok, nil
}

// SetSetting writes a setting.
func (m *Memory) SetSetting(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[key] = value
	return nil
}

// Settings returns every setting.
func (m *Memory) Settings() (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.settings))
	for k, v := range m.settings {
		out[k] = v
	}
	return out, nil
}

// Close is a no-op.
func (m *Memory) Close() error { return nil }

var _ Store = (*Memory)(nil)
