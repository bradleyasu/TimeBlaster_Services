// Package state defines the runtime state Timeblaster reports and the events it
// broadcasts.
//
// It is the single vocabulary shared by the HTTP API, the WebSocket stream and
// the companion app, which keeps "what the device is doing" from being described
// three slightly different ways.
//
// Nothing here is persisted. Durable state lives in internal/storage; this is the
// live picture, rebuilt from the subsystems on every read.
package state

import (
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/audio"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/media"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

// EventType names a broadcast event.
type EventType string

// Event types pushed over the WebSocket. The companion app subscribes once and
// is told about changes rather than polling, which matters on a phone.
const (
	// EventState carries a full Snapshot, sent immediately on connection.
	EventState EventType = "state"
	// EventTick carries the current time, so the app's clock stays in step with
	// the Pi rather than with the phone.
	EventTick EventType = "tick"
	// EventAlarmStarted, EventAlarmStopped and EventAlarmSnoozed track the
	// ringing alarm.
	EventAlarmStarted EventType = "alarm_started"
	EventAlarmStopped EventType = "alarm_stopped"
	EventAlarmSnoozed EventType = "alarm_snoozed"
	// EventAlarmsChanged means the alarm list was edited.
	EventAlarmsChanged EventType = "alarms_changed"
	// EventScheduleChanged carries the next scheduled alarm.
	EventScheduleChanged EventType = "schedule_changed"
	// EventChannelChanged carries the newly selected channel.
	EventChannelChanged EventType = "channel_changed"
	// EventChannelsChanged carries a new channel list.
	EventChannelsChanged EventType = "channels_changed"
	// EventVolumeChanged carries the alarm speaker volume.
	EventVolumeChanged EventType = "volume_changed"
	// EventHardwareChanged reports the Nano link coming up or going down.
	EventHardwareChanged EventType = "hardware_changed"
	// EventWiFiChanged reports entering or leaving setup mode.
	EventWiFiChanged EventType = "wifi_changed"
	// EventSettingsChanged reports a settings update.
	EventSettingsChanged EventType = "settings_changed"
)

// Event is one broadcast message.
type Event struct {
	Type EventType `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data,omitempty"`
}

// NewEvent builds an event stamped with the current time.
func NewEvent(t EventType, data any) Event {
	return Event{Type: t, At: time.Now(), Data: data}
}

// Publisher broadcasts events. The WebSocket hub implements it; a no-op
// implementation is used before the hub exists and in tests.
type Publisher interface {
	Publish(Event)
}

// NopPublisher discards events.
type NopPublisher struct{}

// Publish discards the event.
func (NopPublisher) Publish(Event) {}

// Clock describes the device's time settings.
type Clock struct {
	// Now is the Pi's current time. The Pi is authoritative: the companion app
	// displays this rather than the phone's clock.
	Now time.Time `json:"now"`
	// Timezone is the IANA name in effect.
	Timezone string `json:"timezone"`
	// UTCOffsetSeconds makes it easy for a browser to render local time without
	// shipping a timezone database.
	UTCOffsetSeconds int `json:"utc_offset_seconds"`
	// Clock24h is the display preference.
	Clock24h bool `json:"clock_24h"`
	// Uptime is how long the daemon has been running.
	Uptime time.Duration `json:"uptime_seconds"`
}

// AlarmState describes the alarm subsystem.
type AlarmState struct {
	Active *alarm.Active   `json:"active"`
	Next   *alarm.Upcoming `json:"next"`
	Count  int             `json:"count"`
	// EnabledCount is how many alarms are armed, which is the number the app's
	// summary line shows.
	EnabledCount int `json:"enabled_count"`
}

// Snapshot is the complete live picture of the device.
type Snapshot struct {
	Version  string             `json:"version"`
	Hostname string             `json:"hostname"`
	Clock    Clock              `json:"clock"`
	Alarms   AlarmState         `json:"alarms"`
	Audio    audio.Health       `json:"audio"`
	Media    media.Status       `json:"media"`
	Hardware hardware.Status    `json:"hardware"`
	WiFi     wifi.Status        `json:"wifi"`
	Channels []ersatztv.Channel `json:"channels"`
	// Inputs reports the live position of the physical controls, which makes the
	// companion app useful for diagnosing a knob without a screwdriver.
	Inputs Inputs `json:"inputs"`
}

// Inputs describes the physical controls' current positions.
type Inputs struct {
	// ChannelKnob is 0.0-1.0, or -1 when it has never reported.
	ChannelKnob float64 `json:"channel_knob"`
	// VolumeKnob is 0.0-1.0, or -1 when it has never reported.
	VolumeKnob float64 `json:"volume_knob"`
	// ReservedKnobs are pots 2 and 3, currently unassigned.
	// TODO: name these once pots 2 and 3 have functions.
	ReservedKnobs []float64 `json:"reserved_knobs"`
}

// Health is the compact component report served at /api/health.
//
// It deliberately exposes no SSIDs, no paths outside the application's own
// directories and no credentials: it is the endpoint most likely to be left
// reachable, so it says what is working and nothing more.
type Health struct {
	// Status is "ok" when every critical component is healthy, "degraded" when a
	// non-critical one is not, and "unhealthy" when the alarm path is impaired.
	Status     string            `json:"status"`
	Version    string            `json:"version"`
	Uptime     string            `json:"uptime"`
	Components map[string]string `json:"components"`
	Details    HealthDetails     `json:"details"`
}

// HealthDetails carries the few values worth seeing alongside the summary.
type HealthDetails struct {
	NanoConnected       bool   `json:"nano_connected"`
	ErsatzTVReachable   bool   `json:"ersatztv_reachable"`
	PlayerAlive         bool   `json:"tv_player_alive"`
	AlarmDeviceReady    bool   `json:"alarm_audio_device_available"`
	AlarmSubsystemReady bool   `json:"alarm_subsystem_healthy"`
	WiFiHelperReachable bool   `json:"wifi_helper_reachable"`
	WiFiMode            string `json:"wifi_mode"`
	CurrentChannel      string `json:"current_channel,omitempty"`
	ChannelCount        int    `json:"channel_count"`
	AlarmCount          int    `json:"alarm_count"`
	AlarmRinging        bool   `json:"alarm_ringing"`
}

// Component health values.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusDown     = "down"
	StatusUnknown  = "unknown"
)

// Overall status values.
const (
	OverallOK        = "ok"
	OverallDegraded  = "degraded"
	OverallUnhealthy = "unhealthy"
)

// Overall derives the summary status from the component map.
//
// The rule encodes the project's priority order: the alarm clock is the reason
// the device exists, so only an impaired alarm path makes the device
// "unhealthy". A dead television, an absent Nano or an unreachable ErsatzTV are
// all merely "degraded".
func Overall(components map[string]string, alarmHealthy bool) string {
	if !alarmHealthy {
		return OverallUnhealthy
	}
	for name, s := range components {
		if name == "alarm" {
			continue
		}
		if s == StatusDown || s == StatusDegraded {
			return OverallDegraded
		}
	}
	return OverallOK
}

// Tracker holds the small amount of live state that no single subsystem owns.
type Tracker struct {
	mu        sync.RWMutex
	startedAt time.Time
	version   string
	hostname  string
}

// NewTracker creates a tracker.
func NewTracker(version, hostname string, startedAt time.Time) *Tracker {
	return &Tracker{startedAt: startedAt, version: version, hostname: hostname}
}

// Uptime reports how long the daemon has been running.
func (t *Tracker) Uptime(now time.Time) time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return now.Sub(t.startedAt)
}

// Version reports the build version.
func (t *Tracker) Version() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.version
}

// Hostname reports the configured hostname.
func (t *Tracker) Hostname() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.hostname
}
