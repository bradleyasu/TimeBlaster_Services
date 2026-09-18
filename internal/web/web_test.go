package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/audio"
	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/media"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- fakes -------------------------------------------------------------------

type fakeAlarms struct {
	mu      sync.Mutex
	items   map[int64]alarm.Alarm
	nextID  int64
	active  *alarm.Active
	next    *alarm.Upcoming
	saveErr error
}

func newFakeAlarms(seed ...alarm.Alarm) *fakeAlarms {
	f := &fakeAlarms{items: map[int64]alarm.Alarm{}}
	for _, a := range seed {
		_, _ = f.Save(a)
	}
	return f
}

func (f *fakeAlarms) List() []alarm.Alarm {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]alarm.Alarm, 0, len(f.items))
	for _, a := range f.items {
		out = append(out, a)
	}
	alarm.SortAlarms(out)
	return out
}

func (f *fakeAlarms) Get(id int64) (alarm.Alarm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.items[id]
	if !ok {
		return alarm.Alarm{}, alarm.ErrNotFound
	}
	return a, nil
}

func (f *fakeAlarms) Save(a alarm.Alarm) (alarm.Alarm, error) {
	a = a.Normalize()
	if err := a.Validate(); err != nil {
		return alarm.Alarm{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return alarm.Alarm{}, f.saveErr
	}
	if a.ID == 0 {
		f.nextID++
		a.ID = f.nextID
	}
	f.items[a.ID] = a
	return a, nil
}

func (f *fakeAlarms) Delete(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; !ok {
		return alarm.ErrNotFound
	}
	delete(f.items, id)
	return nil
}

func (f *fakeAlarms) SetEnabled(id int64, enabled bool) (alarm.Alarm, error) {
	a, err := f.Get(id)
	if err != nil {
		return alarm.Alarm{}, err
	}
	a.Enabled = enabled
	return f.Save(a)
}

func (f *fakeAlarms) Active() *alarm.Active {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func (f *fakeAlarms) Next() *alarm.Upcoming {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next
}

func (f *fakeAlarms) Dismiss() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil {
		return alarm.ErrNoneActive
	}
	f.active = nil
	return nil
}

func (f *fakeAlarms) Snooze() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil {
		return alarm.ErrNoneActive
	}
	if f.active.State != alarm.StateRinging {
		return alarm.ErrNotRinging
	}
	f.active.State = alarm.StateSnoozed
	return nil
}

func (f *fakeAlarms) Trigger(id int64) error {
	a, err := f.Get(id)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = &alarm.Active{AlarmID: a.ID, State: alarm.StateRinging}
	return nil
}

func (f *fakeAlarms) Location() *time.Location { return time.UTC }

func (f *fakeAlarms) setActive(a *alarm.Active) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = a
}

type fakeAudio struct {
	mu            sync.Mutex
	sounds        []audio.Sound
	volume        int
	authoritative bool
	testErr       error
	volumeErr     error
	previewed     string
	stopped       bool
}

func (f *fakeAudio) Sounds() []audio.Sound { return f.sounds }

func (f *fakeAudio) TestAlarm(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.testErr != nil {
		return f.testErr
	}
	f.previewed = id
	return nil
}

func (f *fakeAudio) StopAlarm() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *fakeAudio) SetAlarmVolume(_ context.Context, pct int, physical bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumeErr != nil {
		return f.volumeErr
	}
	f.volume = pct
	if physical {
		f.authoritative = true
	}
	return nil
}

func (f *fakeAudio) Volume() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volume, f.authoritative
}

func (f *fakeAudio) Health() audio.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return audio.Health{DeviceAvailable: true, VolumePercent: f.volume, SoundCount: len(f.sounds)}
}

type fakeMedia struct {
	mu        sync.Mutex
	channels  []ersatztv.Channel
	current   *ersatztv.Channel
	selectErr error
	refreshed bool
	cleared   bool
}

func (f *fakeMedia) Channels() []ersatztv.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.channels
}

func (f *fakeMedia) Current() *ersatztv.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func (f *fakeMedia) SelectNumber(_ context.Context, number string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.selectErr != nil {
		return f.selectErr
	}
	for _, c := range f.channels {
		if c.Number == number {
			cp := c
			f.current = &cp
			return nil
		}
	}
	return media.ErrNoChannels
}

func (f *fakeMedia) ShowNoChannel(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current, f.cleared = nil, true
	return nil
}

func (f *fakeMedia) Status() media.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return media.Status{ErsatzTVReachable: true, ChannelCount: len(f.channels), PlayerAlive: true}
}

func (f *fakeMedia) RefreshSoon() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = true
}

type fakeHardware struct{ connected bool }

func (f *fakeHardware) Status() hardware.Status { return hardware.Status{Connected: f.connected} }
func (f *fakeHardware) Connected() bool         { return f.connected }

type fakeInputs struct{}

func (fakeInputs) Position(int) (float64, bool) { return 0.5, true }

type fakeSettings struct {
	mu       sync.Mutex
	settings Settings
	err      error
}

func (f *fakeSettings) Settings() Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings
}

func (f *fakeSettings) UpdateSettings(_ context.Context, s Settings) (Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Settings{}, f.err
	}
	f.settings = s
	return s, nil
}

type fakeSnapshots struct{ deps *Deps }

func (f *fakeSnapshots) Snapshot() state.Snapshot {
	return state.Snapshot{
		Version: "test", Hostname: "timeblaster",
		Clock: state.Clock{Now: time.Now(), Timezone: "UTC"},
	}
}

func (f *fakeSnapshots) Health() state.Health {
	return state.Health{
		Status: state.OverallOK, Version: "test", Uptime: "1m",
		Components: map[string]string{"alarm": state.StatusOK, "nano": state.StatusDown},
	}
}

// --- fixture -----------------------------------------------------------------

type fixture struct {
	srv      *Server
	handler  http.Handler
	alarms   *fakeAlarms
	audio    *fakeAudio
	media    *fakeMedia
	wifi     *wifi.FakeManager
	settings *fakeSettings
}

func newFixture(t *testing.T, seed ...alarm.Alarm) *fixture {
	t.Helper()
	cfg := config.Default()

	f := &fixture{
		alarms: newFakeAlarms(seed...),
		audio: &fakeAudio{
			sounds: []audio.Sound{{ID: "alarm1", Name: "Alarm 1"}, {ID: "alarm2", Name: "Alarm 2"}},
			volume: 40,
		},
		media: &fakeMedia{channels: []ersatztv.Channel{
			{ID: 1, Number: "1", Name: "Movies"},
			{ID: 2, Number: "2", Name: "Cartoons"},
		}},
		wifi:     wifi.NewFakeManager(),
		settings: &fakeSettings{settings: Settings{Timezone: "UTC", DisplayBrightness: 75, DefaultSoundID: "alarm1"}},
	}

	srv, err := NewServer(cfg.Web, Deps{
		Config: cfg, Alarms: f.alarms, Audio: f.audio, Media: f.media,
		Hardware: &fakeHardware{connected: true}, Inputs: fakeInputs{},
		WiFi: f.wifi, Settings: f.settings, Snapshots: &fakeSnapshots{},
		Logger: testLogger(), Version: "test",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	f.srv, f.handler = srv, srv.Handler()
	return f
}

func (f *fixture) do(t *testing.T, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
}

// --- tests -------------------------------------------------------------------

func TestHealthEndpoint(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var h state.Health
	decode(t, rec, &h)
	if h.Status != state.OverallOK || h.Components["alarm"] != state.StatusOK {
		t.Errorf("health: %+v", h)
	}
	// The endpoint must not leak anything sensitive.
	for _, forbidden := range []string{"password", "passphrase", "psk", "/etc/", "/var/lib"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), forbidden) {
			t.Errorf("health response leaks %q: %s", forbidden, rec.Body)
		}
	}
}

func TestStateEndpoint(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/state", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var snap state.Snapshot
	decode(t, rec, &snap)
	if snap.Hostname != "timeblaster" {
		t.Errorf("snapshot: %+v", snap)
	}
}

func TestAlarmCRUD(t *testing.T) {
	f := newFixture(t)

	// Create.
	rec := f.do(t, http.MethodPost, "/api/alarms",
		`{"hour":6,"minute":30,"label":"Wake up","repeat_days":62,"sound_id":"alarm2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created alarmDTO
	decode(t, rec, &created)
	if created.ID == 0 || created.Hour != 6 || created.SoundID != "alarm2" {
		t.Fatalf("created: %+v", created)
	}
	if created.RepeatLabel != "weekdays" {
		t.Errorf("repeat label: %q", created.RepeatLabel)
	}
	if created.NextOccurrence == nil {
		t.Error("the next occurrence should be computed for the app")
	}

	// Read.
	rec = f.do(t, http.MethodGet, "/api/alarms", "")
	var list struct{ Alarms []alarmDTO }
	decode(t, rec, &list)
	if len(list.Alarms) != 1 {
		t.Fatalf("list: %+v", list)
	}

	// Update: a partial body must not clear the untouched fields.
	rec = f.do(t, http.MethodPut, "/api/alarms/1", `{"hour":7}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	var updated alarmDTO
	decode(t, rec, &updated)
	if updated.Hour != 7 {
		t.Errorf("hour: %d", updated.Hour)
	}
	if updated.Label != "Wake up" || updated.RepeatDays != 62 || !updated.Enabled {
		t.Errorf("a partial update clobbered other fields: %+v", updated)
	}

	// Toggle.
	rec = f.do(t, http.MethodPost, "/api/alarms/1/enabled", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: %d %s", rec.Code, rec.Body)
	}
	decode(t, rec, &updated)
	if updated.Enabled {
		t.Error("alarm should be disabled")
	}

	// Delete.
	rec = f.do(t, http.MethodDelete, "/api/alarms/1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec = f.do(t, http.MethodGet, "/api/alarms/1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", rec.Code)
	}
}

func TestAlarmValidationErrors(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, body string
		wantStatus int
	}{
		{"missing time", `{"label":"x"}`, http.StatusBadRequest},
		{"hour out of range", `{"hour":99,"minute":0}`, http.StatusBadRequest},
		{"minute out of range", `{"hour":6,"minute":99}`, http.StatusBadRequest},
		{"unknown field", `{"hour":6,"minute":0,"colour":"green"}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
		{"bad repeat mask", `{"hour":6,"minute":0,"repeat_days":9999}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/api/alarms", tc.body)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
		})
	}
}

func TestAlarmBadPathID(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/alarms/abc", "/api/alarms/0", "/api/alarms/-1"} {
		if rec := f.do(t, http.MethodGet, path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestDismissAndSnooze(t *testing.T) {
	f := newFixture(t, alarm.New(6, 30))

	// Nothing ringing.
	if rec := f.do(t, http.MethodPost, "/api/alarm/dismiss", ""); rec.Code != http.StatusConflict {
		t.Errorf("dismiss with nothing active: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/api/alarm/snooze", ""); rec.Code != http.StatusConflict {
		t.Errorf("snooze with nothing active: %d", rec.Code)
	}

	f.alarms.setActive(&alarm.Active{AlarmID: 1, State: alarm.StateRinging})
	if rec := f.do(t, http.MethodPost, "/api/alarm/snooze", ""); rec.Code != http.StatusOK {
		t.Fatalf("snooze: %d %s", rec.Code, rec.Body)
	}
	// Snoozing twice is a conflict, not a silent success.
	if rec := f.do(t, http.MethodPost, "/api/alarm/snooze", ""); rec.Code != http.StatusConflict {
		t.Errorf("double snooze: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/api/alarm/dismiss", ""); rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	if f.alarms.Active() != nil {
		t.Error("alarm still active")
	}
}

func TestTriggerAlarm(t *testing.T) {
	f := newFixture(t, alarm.New(6, 30))
	if rec := f.do(t, http.MethodPost, "/api/alarms/1/trigger", ""); rec.Code != http.StatusOK {
		t.Fatalf("trigger: %d %s", rec.Code, rec.Body)
	}
	if f.alarms.Active() == nil {
		t.Error("alarm was not triggered")
	}
	if rec := f.do(t, http.MethodPost, "/api/alarms/99/trigger", ""); rec.Code != http.StatusNotFound {
		t.Errorf("trigger unknown: %d", rec.Code)
	}
}

func TestSoundsAndPreview(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/api/sounds", "")
	var body struct{ Sounds []audio.Sound }
	decode(t, rec, &body)
	if len(body.Sounds) != 2 {
		t.Fatalf("sounds: %+v", body)
	}

	if rec := f.do(t, http.MethodPost, "/api/sounds/alarm2/preview", ""); rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if f.audio.previewed != "alarm2" {
		t.Errorf("previewed: %q", f.audio.previewed)
	}

	// A preview during a real alarm is a conflict, not a failure to be retried.
	f.audio.testErr = audio.ErrBusy
	if rec := f.do(t, http.MethodPost, "/api/sounds/alarm1/preview", ""); rec.Code != http.StatusConflict {
		t.Errorf("busy preview: %d", rec.Code)
	}
	f.audio.testErr = audio.ErrSoundNotFound
	if rec := f.do(t, http.MethodPost, "/api/sounds/gone/preview", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing sound: %d", rec.Code)
	}

	f.audio.testErr = nil
	if rec := f.do(t, http.MethodPost, "/api/sounds/preview/stop", ""); rec.Code != http.StatusOK {
		t.Errorf("stop: %d", rec.Code)
	}
	if !f.audio.stopped {
		t.Error("playback was not stopped")
	}
}

func TestVolumeEndpoint(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/api/volume", "")
	var v struct {
		Percent           int  `json:"percent"`
		KnobAuthoritative bool `json:"knob_authoritative"`
		SoftwareControl   bool `json:"software_control"`
	}
	decode(t, rec, &v)
	if v.Percent != 40 {
		t.Errorf("percent: %d", v.Percent)
	}
	// The default configuration keeps the knob authoritative, and the app is told
	// so it can present the value read-only rather than offering a losing slider.
	if v.SoftwareControl {
		t.Error("software control should be off by default")
	}

	if rec := f.do(t, http.MethodPut, "/api/volume", `{"percent":200}`); rec.Code != http.StatusBadRequest {
		t.Errorf("out of range: %d", rec.Code)
	}

	f.audio.volumeErr = errors.New("software volume control is disabled")
	if rec := f.do(t, http.MethodPut, "/api/volume", `{"percent":30}`); rec.Code != http.StatusConflict {
		t.Errorf("rejected software volume should be a conflict: %d", rec.Code)
	}

	f.audio.volumeErr = nil
	if rec := f.do(t, http.MethodPut, "/api/volume", `{"percent":30}`); rec.Code != http.StatusOK {
		t.Fatalf("set volume: %d %s", rec.Code, rec.Body)
	}
	if pct, authoritative := f.audio.Volume(); pct != 30 || authoritative {
		t.Errorf("a software change must not become authoritative: %d, %v", pct, authoritative)
	}
}

func TestChannelEndpoints(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/api/channels", "")
	var list struct {
		Channels []ersatztv.Channel
		Status   media.Status
	}
	decode(t, rec, &list)
	if len(list.Channels) != 2 || !list.Status.ErsatzTVReachable {
		t.Fatalf("channels: %+v", list)
	}

	if rec := f.do(t, http.MethodPost, "/api/channels/select", `{"number":"2"}`); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body)
	}
	if cur := f.media.Current(); cur == nil || cur.Number != "2" {
		t.Errorf("current: %+v", cur)
	}

	if rec := f.do(t, http.MethodPost, "/api/channels/select", `{"number":"99"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/api/channels/select", `{"number":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty channel: %d", rec.Code)
	}

	if rec := f.do(t, http.MethodPost, "/api/channels/clear", ""); rec.Code != http.StatusOK {
		t.Errorf("clear: %d", rec.Code)
	}
	if !f.media.cleared {
		t.Error("channel was not cleared")
	}

	if rec := f.do(t, http.MethodPost, "/api/channels/refresh", ""); rec.Code != http.StatusAccepted {
		t.Errorf("refresh: %d", rec.Code)
	}
	if !f.media.refreshed {
		t.Error("refresh was not requested")
	}
}

func TestSettingsEndpoints(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/api/settings", "")
	var s Settings
	decode(t, rec, &s)
	if s.DisplayBrightness != 75 {
		t.Fatalf("settings: %+v", s)
	}

	// A partial update must leave the rest alone.
	rec = f.do(t, http.MethodPut, "/api/settings", `{"clock_24h":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	decode(t, rec, &s)
	if !s.Clock24h || s.DisplayBrightness != 75 || s.DefaultSoundID != "alarm1" {
		t.Errorf("partial update clobbered fields: %+v", s)
	}

	for _, bad := range []string{`{"display_brightness":250}`, `{"timezone":"Mars/Olympus"}`} {
		if rec := f.do(t, http.MethodPut, "/api/settings", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

func TestWiFiEndpoints(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/api/wifi/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}

	rec = f.do(t, http.MethodGet, "/api/wifi/networks", "")
	var nets struct{ Networks []wifi.Network }
	decode(t, rec, &nets)
	if len(nets.Networks) == 0 {
		t.Error("no networks returned")
	}

	if rec := f.do(t, http.MethodPost, "/api/wifi/setup", ""); rec.Code != http.StatusOK {
		t.Fatalf("enter setup: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodDelete, "/api/wifi/setup", ""); rec.Code != http.StatusOK {
		t.Fatalf("exit setup: %d %s", rec.Code, rec.Body)
	}

	if rec := f.do(t, http.MethodPost, "/api/wifi/connect", `{"ssid":"Home","passphrase":"hunter22"}`); rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}

	// Credentials are validated before they can reach nmcli.
	for _, bad := range []string{
		`{"ssid":""}`,
		`{"ssid":"Home\nevil"}`,
		`{"ssid":"Home","passphrase":"abc"}`,
	} {
		if rec := f.do(t, http.MethodPost, "/api/wifi/connect", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

func TestWiFiConnectFailureSurfacesTheResult(t *testing.T) {
	f := newFixture(t)
	f.wifi.SetConnectError("The password was not accepted.")

	rec := f.do(t, http.MethodPost, "/api/wifi/connect", `{"ssid":"Home","passphrase":"wrongpass"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: %d", rec.Code)
	}
	var result wifi.ConnectResult
	decode(t, rec, &result)
	if result.Success || result.Message == "" {
		t.Errorf("result: %+v", result)
	}
}

func TestWiFiHelperUnavailable(t *testing.T) {
	f := newFixture(t)
	f.wifi.Err = wifi.ErrHelperUnavailable
	for _, path := range []string{"/api/wifi/status", "/api/wifi/networks"} {
		if rec := f.do(t, http.MethodGet, path, ""); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{"/", "/index.html", "/app.js", "/app.css", "/manifest.webmanifest", "/sw.js"} {
		rec := f.do(t, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}

	// The service worker must not be cached, or a stale one pins the app.
	rec := f.do(t, http.MethodGet, "/sw.js", "")
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("sw.js Cache-Control: %q", cc)
	}
}

func TestUnknownRoutesFallBackToTheApp(t *testing.T) {
	f := newFixture(t)

	// A client-side route serves the app shell.
	rec := f.do(t, http.MethodGet, "/settings", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "TIMEBLASTER") {
		t.Errorf("client route: %d", rec.Code)
	}
	// A genuinely missing asset is a 404, not a page of HTML.
	if rec := f.do(t, http.MethodGet, "/nope.js", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset: %d", rec.Code)
	}
}

func TestAPIRoutesWinOverStaticFallback(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/alarms", "")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("API route served static content: %q", ct)
	}
}

func TestSecurityHeadersAndCORSPreflight(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/health", "")
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}

	rec = f.do(t, http.MethodOptions, "/api/alarms", "")
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight: %d", rec.Code)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	f := newFixture(t)
	huge := `{"label":"` + strings.Repeat("x", maxBodyBytes+1024) + `","hour":6,"minute":0}`
	if rec := f.do(t, http.MethodPost, "/api/alarms", huge); rec.Code != http.StatusBadRequest {
		t.Errorf("status: %d", rec.Code)
	}
}

func TestHandlerPanicIsContained(t *testing.T) {
	// A panic in a handler must not take the daemon — and the alarm clock — down.
	cfg := config.Default()
	srv, err := NewServer(cfg.Web, Deps{
		Config: cfg, Alarms: newFakeAlarms(), Audio: &fakeAudio{},
		Media: &fakeMedia{}, Hardware: &fakeHardware{}, Inputs: fakeInputs{},
		WiFi: wifi.NewFakeManager(), Settings: &fakeSettings{},
		Snapshots: panicSnapshots{}, Logger: testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d", rec.Code)
	}
}

type panicSnapshots struct{}

func (panicSnapshots) Snapshot() state.Snapshot { panic("boom") }
func (panicSnapshots) Health() state.Health     { return state.Health{} }

func TestNewServerRejectsMissingLogger(t *testing.T) {
	if _, err := NewServer(config.Default().Web, Deps{}); err == nil {
		t.Error("expected an error")
	}
}

func TestServerRunShutsDownGracefully(t *testing.T) {
	cfg := config.Default().Web
	cfg.ListenAddress = "127.0.0.1:0"
	f := newFixture(t)
	f.srv.cfg = cfg
	f.srv.http.Addr = cfg.ListenAddress

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.srv.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}
