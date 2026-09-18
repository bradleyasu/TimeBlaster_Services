package state

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOverallPrioritisesTheAlarmClock(t *testing.T) {
	// The rule that encodes the project's priority order: only an impaired alarm
	// path is "unhealthy". Everything else is at worst "degraded", because none
	// of it stops the Timeblaster waking you up.
	tests := []struct {
		name         string
		components   map[string]string
		alarmHealthy bool
		want         string
	}{
		{
			name:         "everything working",
			components:   map[string]string{"alarm": StatusOK, "nano": StatusOK, "ersatztv": StatusOK},
			alarmHealthy: true,
			want:         OverallOK,
		},
		{
			name:         "no nano attached",
			components:   map[string]string{"alarm": StatusOK, "nano": StatusDown, "ersatztv": StatusOK},
			alarmHealthy: true,
			want:         OverallDegraded,
		},
		{
			name:         "ersatztv and the television are dead",
			components:   map[string]string{"alarm": StatusOK, "ersatztv": StatusDown, "tv_player": StatusDown},
			alarmHealthy: true,
			want:         OverallDegraded,
		},
		{
			name:         "alarm scheduling is broken",
			components:   map[string]string{"alarm": StatusDown, "nano": StatusOK, "ersatztv": StatusOK},
			alarmHealthy: false,
			want:         OverallUnhealthy,
		},
		{
			name:         "alarm broken outranks everything else being fine",
			components:   map[string]string{"alarm": StatusDown, "nano": StatusOK},
			alarmHealthy: false,
			want:         OverallUnhealthy,
		},
		{
			name:         "unknown components do not degrade",
			components:   map[string]string{"alarm": StatusOK, "ersatztv": StatusUnknown},
			alarmHealthy: true,
			want:         OverallOK,
		},
		{
			name:         "no components at all",
			components:   map[string]string{},
			alarmHealthy: true,
			want:         OverallOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Overall(tc.components, tc.alarmHealthy); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestOverallIgnoresTheAlarmComponentValue(t *testing.T) {
	// The alarm component's own string is not consulted when scanning for
	// degradation: the alarmHealthy flag is the authority, so a down alarm
	// cannot also register as merely "degraded".
	got := Overall(map[string]string{"alarm": StatusDown}, true)
	if got != OverallOK {
		t.Errorf("got %q", got)
	}
}

func TestNewEventIsStamped(t *testing.T) {
	before := time.Now()
	ev := NewEvent(EventAlarmStarted, map[string]int{"alarm_id": 3})
	if ev.Type != EventAlarmStarted {
		t.Errorf("type: %s", ev.Type)
	}
	if ev.At.Before(before) {
		t.Errorf("timestamp: %v", ev.At)
	}
}

func TestEventJSONFieldNames(t *testing.T) {
	// The companion app switches on these names, so they are part of the API.
	b, err := json.Marshal(NewEvent(EventChannelChanged, nil))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "channel_changed" {
		t.Errorf("type: %v", decoded["type"])
	}
	if _, ok := decoded["at"]; !ok {
		t.Error("missing at")
	}
	// Data is omitted when nil rather than sent as null.
	if _, ok := decoded["data"]; ok {
		t.Error("nil data should be omitted")
	}
}

func TestSnapshotJSONIsStable(t *testing.T) {
	snap := Snapshot{
		Version: "1.0.0", Hostname: "timeblaster",
		Clock: Clock{Now: time.Unix(1758220642, 0).UTC(), Timezone: "UTC", Clock24h: true},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "hostname", "clock", "alarms", "audio", "media", "hardware", "wifi", "channels", "inputs"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("snapshot is missing %q", key)
		}
	}
}

func TestNopPublisher(t *testing.T) {
	// Used before the WebSocket hub exists; it must simply not panic.
	NopPublisher{}.Publish(NewEvent(EventTick, nil))
}

func TestTracker(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	tr := NewTracker("1.2.3", "timeblaster", start)

	if got := tr.Uptime(start.Add(90 * time.Second)); got != 90*time.Second {
		t.Errorf("uptime: %v", got)
	}
	if tr.Version() != "1.2.3" || tr.Hostname() != "timeblaster" {
		t.Errorf("tracker: %s %s", tr.Version(), tr.Hostname())
	}
}
