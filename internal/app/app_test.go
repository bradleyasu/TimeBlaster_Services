package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/audio"
	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/logging"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/storage"
	"github.com/bradsheets/timeblaster/internal/system"
	"github.com/bradsheets/timeblaster/internal/web"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

// These tests build the entire application graph — scheduler, audio, input
// routing, media, web, WebSocket — with every hardware dependency substituted.
// The whole thing runs on a development machine with no Pi, no Nano, no mpv, no
// ErsatzTV and no sound card.

type harness struct {
	app    *App
	clock  *system.FakeClock
	nano   *hardware.FakeNano
	player *mpv.Fake
	etv    *ersatztv.Fake
	audio  *audio.FakePlayer
	wifi   *wifi.FakeManager
	store  storage.Store
	runner *system.FakeRunner
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	dir := t.TempDir()
	soundsDir := filepath.Join(dir, "sounds")
	if err := os.MkdirAll(soundsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alarm1.mp3", "alarm2.mp3"} {
		body := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), make([]byte, 2048)...)
		if err := os.WriteFile(filepath.Join(soundsDir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cardsPath := filepath.Join(dir, "cards")
	if err := os.WriteFile(cardsPath, []byte(
		" 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0\n"+
			"                      vc4-hdmi-0\n"+
			" 1 [Device         ]: USB-Audio - USB Audio Device\n"+
			"                      Generic USB Audio Device\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Storage.DatabasePath = filepath.Join(dir, "timeblaster.db")
	cfg.Storage.AlarmSoundsDir = soundsDir
	cfg.Storage.RuntimeDir = filepath.Join(dir, "run")
	cfg.Web.ListenAddress = "127.0.0.1:0"
	cfg.General.Timezone = "UTC"
	cfg.Audio.AlarmDevice = "USB"
	if mutate != nil {
		mutate(&cfg)
	}

	clock := system.NewFakeClock(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC))
	runner := system.NewFakeRunner()
	runner.Outputs["amixer"] = []byte("Simple mixer control 'PCM',0\n")

	h := &harness{
		clock:  clock,
		nano:   hardware.NewFakeNano(),
		player: mpv.NewFake(),
		etv: ersatztv.NewFake(
			ersatztv.Channel{ID: 1, Number: "1", Name: "Movies"},
			ersatztv.Channel{ID: 2, Number: "2", Name: "Cartoons"},
			ersatztv.Channel{ID: 3, Number: "3", Name: "Sci-Fi"},
			ersatztv.Channel{ID: 4, Number: "4", Name: "News"},
		),
		audio:  audio.NewFakePlayer(),
		wifi:   wifi.NewFakeManager(),
		runner: runner,
	}

	probe := system.ALSAProbe{CardsPath: cardsPath, Runner: runner}
	application, err := New(cfg, logging.Discard(), "test", Deps{
		Clock: clock, Runner: runner,
		Nano: h.nano, Player: h.player, ErsatzTV: h.etv,
		AudioPlayer: h.audio, WiFi: h.wifi, ALSAProbe: &probe,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.app = application
	h.store = application.store
	t.Cleanup(func() { application.shutdown() })
	return h
}

// syncChannels performs the refresh the running app would do, without starting
// the background loop.
func (h *harness) syncChannels(t *testing.T) {
	t.Helper()
	if err := h.app.media.Refresh(context.Background()); err != nil {
		t.Fatalf("channel refresh: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestApplicationBuildsWithNoHardware(t *testing.T) {
	h := newHarness(t, nil)

	if h.app.scheduler == nil || h.app.audio == nil || h.app.web == nil {
		t.Fatal("subsystems were not built")
	}
	health := h.app.Health()
	if health.Status == "" || len(health.Components) == 0 {
		t.Fatalf("health: %+v", health)
	}
	// The alarm path must be healthy even though nothing else is real.
	if health.Components["alarm"] != state.StatusOK {
		t.Errorf("alarm component: %s", health.Components["alarm"])
	}
}

func TestChannelKnobSelectsChannelsEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	h.syncChannels(t)

	// Four channels: the knob's travel is divided into four bands.
	if got := h.app.router.ChannelCount(); got != 4 {
		t.Fatalf("channel count: %d", got)
	}

	// Knob at 90 % of travel => band 3 => channel 4.
	h.app.router.PotReport(input.PotChannel, 3800)
	waitFor(t, "channel selection", func() bool {
		cur := h.app.media.Current()
		return cur != nil && cur.Number == "4"
	})

	// Knob back to 10 % => band 0 => channel 1.
	h.app.router.PotReport(input.PotChannel, 400)
	waitFor(t, "channel change", func() bool {
		cur := h.app.media.Current()
		return cur != nil && cur.Number == "1"
	})

	// The stream URL was loaded and a change overlay drawn.
	loads := h.player.CallsNamed("loadfile")
	if len(loads) < 2 {
		t.Fatalf("loadfile calls: %+v", loads)
	}
	if len(h.player.CallsNamed("osd-overlay")) == 0 {
		t.Error("no channel overlay was drawn")
	}
}

func TestChannelCountIsDynamic(t *testing.T) {
	h := newHarness(t, nil)
	h.syncChannels(t)
	h.app.router.PotReport(input.PotChannel, 3800) // fully clockwise
	waitFor(t, "initial selection", func() bool { return h.app.media.Current() != nil })

	// The user adds channels in ErsatzTV. The knob's travel re-divides with no
	// restart and nothing hardcoded.
	h.etv.SetChannels(
		ersatztv.Channel{Number: "1", Name: "A"}, ersatztv.Channel{Number: "2", Name: "B"},
		ersatztv.Channel{Number: "3", Name: "C"}, ersatztv.Channel{Number: "4", Name: "D"},
		ersatztv.Channel{Number: "5", Name: "E"}, ersatztv.Channel{Number: "6", Name: "F"},
		ersatztv.Channel{Number: "7", Name: "G"}, ersatztv.Channel{Number: "8", Name: "H"},
	)
	h.syncChannels(t)

	waitFor(t, "re-banding", func() bool { return h.app.router.ChannelCount() == 8 })
	// The same physical position now means the last of eight channels.
	waitFor(t, "reselection", func() bool {
		cur := h.app.media.Current()
		return cur != nil && cur.Number == "8"
	})
}

func TestVolumeKnobDrivesTheAlarmSpeaker(t *testing.T) {
	h := newHarness(t, nil)

	if _, authoritative := h.app.audio.Volume(); authoritative {
		t.Fatal("volume must not be authoritative before the knob reports")
	}

	h.app.router.PotReport(input.PotAlarmVolume, 3276) // ~80 %
	waitFor(t, "volume change", func() bool {
		_, authoritative := h.app.audio.Volume()
		return authoritative
	})

	pct, _ := h.app.audio.Volume()
	if pct < 78 || pct > 82 {
		t.Errorf("volume: %d", pct)
	}
	// It must reach the USB card's hardware mixer.
	waitFor(t, "amixer call", func() bool {
		for _, c := range h.runner.Calls() {
			if c.Name == "amixer" && strings.Contains(c.String(), "sset PCM") {
				return true
			}
		}
		return false
	})
}

func TestBigRedButtonDismissesARingingAlarm(t *testing.T) {
	h := newHarness(t, nil)

	a := alarm.New(6, 30)
	a.RepeatDays = alarm.EveryDay
	a.SoundID = "alarm1"
	saved, err := h.app.scheduler.Save(a)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.app.scheduler.Trigger(saved.ID); err != nil {
		t.Fatal(err)
	}

	// Audio starts on the alarm speaker and the Nano is told.
	waitFor(t, "alarm audio", func() bool { return len(h.audio.Plays()) > 0 })
	play, _ := h.audio.LastPlay()
	if !strings.HasSuffix(play.Path, "alarm1.mp3") {
		t.Errorf("played %s", play.Path)
	}
	if !play.Opts.Loop {
		t.Error("an alarm must loop until dismissed")
	}
	if play.Opts.Device != "alsa/hw:CARD=Device,DEV=0" {
		t.Errorf("alarm audio must target the USB speaker, got %q", play.Opts.Device)
	}
	waitFor(t, "nano alarm state", func() bool {
		c, ok := h.nano.LastOfKind("alarm")
		return ok && c.Flag
	})

	// Starting an alarm must not disturb television playback.
	if len(h.player.CallsNamed("loadfile")) != 0 {
		t.Error("the alarm touched the television player")
	}

	// The big red button.
	h.app.router.ButtonEdge(protocol.ButtonAlarmOff, true)
	h.clock.Advance(150 * time.Millisecond)
	h.app.router.ButtonEdge(protocol.ButtonAlarmOff, false)

	waitFor(t, "dismissal", func() bool { return h.app.scheduler.Active() == nil })
	waitFor(t, "audio stop", func() bool {
		hd := h.audio.LastHandle()
		return hd != nil && hd.Stopped()
	})
	waitFor(t, "nano alarm cleared", func() bool {
		c, ok := h.nano.LastOfKind("alarm")
		return ok && !c.Flag
	})
}

func TestBigRedButtonDoesNothingWhenNoAlarmIsRinging(t *testing.T) {
	h := newHarness(t, nil)
	h.nano.Reset()

	h.app.router.ButtonEdge(protocol.ButtonAlarmOff, true)
	h.clock.Advance(150 * time.Millisecond)
	h.app.router.ButtonEdge(protocol.ButtonAlarmOff, false)
	time.Sleep(50 * time.Millisecond)

	if len(h.audio.Plays()) != 0 {
		t.Error("the button started audio with no alarm active")
	}
	if len(h.player.CallsNamed("loadfile")) != 0 {
		t.Error("the button touched the television")
	}
}

func TestWiFiButtonNeedsAFiveSecondHold(t *testing.T) {
	h := newHarness(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.app.router.Run(ctx) }()

	// A short press does nothing at all.
	h.app.router.ButtonEdge(protocol.ButtonWiFi, true)
	h.clock.Advance(200 * time.Millisecond)
	h.app.router.ButtonEdge(protocol.ButtonWiFi, false)
	time.Sleep(50 * time.Millisecond)

	for _, call := range h.wifi.Calls() {
		if call == "enter_setup" {
			t.Fatal("a short press entered setup mode")
		}
	}

	// A five-second hold does.
	h.app.router.ButtonEdge(protocol.ButtonWiFi, true)
	waitFor(t, "poll timer", func() bool { return h.clock.Waiters() > 0 })
	h.clock.Advance(6 * time.Second)

	waitFor(t, "setup mode", func() bool {
		for _, call := range h.wifi.Calls() {
			if call == "enter_setup" {
				return true
			}
		}
		return false
	})

	// The user is told on the 7-segment display before the network goes away.
	waitFor(t, "SETUP on the display", func() bool {
		c, ok := h.nano.LastOfKind("display-text")
		return ok && c.Text == "SETUP"
	})
}

func TestAlarmRingsWithEverythingElseBroken(t *testing.T) {
	// The central promise of the design: ErsatzTV down, mpv dead, the Nano
	// unplugged, the network gone — the alarm still fires and can still be
	// dismissed.
	h := newHarness(t, nil)

	h.etv.SetError(errors.New("ersatztv is not running"))
	h.player.NotAlive = true
	h.nano.SetConnected(false)
	h.nano.Err = errors.New("no Nano attached")
	h.wifi.Err = wifi.ErrHelperUnavailable

	a := alarm.New(6, 30)
	a.RepeatDays = alarm.EveryDay
	saved, err := h.app.scheduler.Save(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.scheduler.Trigger(saved.ID); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	waitFor(t, "alarm audio", func() bool { return len(h.audio.Plays()) > 0 })
	if h.app.scheduler.Active() == nil {
		t.Fatal("no active alarm")
	}

	// And the big red button still works.
	h.app.router.ButtonEdge(protocol.ButtonAlarmOff, true)
	h.clock.Advance(150 * time.Millisecond)
	h.app.router.ButtonEdge(protocol.ButtonAlarmOff, false)
	waitFor(t, "dismissal", func() bool { return h.app.scheduler.Active() == nil })

	// Health reports the damage honestly, but not as "unhealthy": the alarm
	// clock works, which is what that word is reserved for.
	health := h.app.Health()
	if health.Status != state.OverallDegraded {
		t.Errorf("status: %s (components %+v)", health.Status, health.Components)
	}
	if health.Components["alarm"] != state.StatusOK {
		t.Errorf("the alarm subsystem should still be OK: %s", health.Components["alarm"])
	}
}

func TestAlarmStillRingsWithNoSoundFiles(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {})
	// Someone deleted the MP3s.
	entries, err := os.ReadDir(h.app.cfg.Storage.AlarmSoundsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(h.app.cfg.Storage.AlarmSoundsDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.app.library.Refresh(); err != nil {
		t.Fatal(err)
	}

	a := alarm.New(6, 30)
	a.SoundID = "alarm1"
	saved, err := h.app.scheduler.Save(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.scheduler.Trigger(saved.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "fallback tone", func() bool { return len(h.audio.Plays()) > 0 })
	play, _ := h.audio.LastPlay()
	if !strings.HasSuffix(play.Path, "fallback-tone.wav") {
		t.Errorf("expected the built-in tone, played %s", play.Path)
	}
}

func TestNanoReconnectResetsDerivedInputState(t *testing.T) {
	h := newHarness(t, nil)
	h.syncChannels(t)
	h.app.router.PotReport(input.PotChannel, 3800)
	waitFor(t, "selection", func() bool { return h.app.media.Current() != nil })

	// The cable is pulled and plugged back in.
	h.app.LinkDown(errors.New("cable removed"))
	if _, primed := h.app.router.Position(input.PotChannel); primed {
		t.Error("input state survived a disconnect")
	}

	h.app.LinkUp("/dev/timeblaster-nano")
	if got := h.app.router.ChannelCount(); got != 4 {
		t.Errorf("channel count after reconnect: %d", got)
	}
	// The Nano's first report re-establishes the truth.
	h.app.router.PotReport(input.PotChannel, 400)
	waitFor(t, "reselection", func() bool {
		cur := h.app.media.Current()
		return cur != nil && cur.Number == "1"
	})
}

func TestSettingsPersistAndApply(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	saved, err := h.app.UpdateSettings(ctx, webSettings("America/New_York", true, 30, "alarm2", false))
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if saved.Timezone != "America/New_York" || !saved.Clock24h || saved.DisplayBrightness != 30 {
		t.Fatalf("saved: %+v", saved)
	}

	// The scheduler's timezone changed, so alarms move with it.
	if h.app.scheduler.Location().String() != "America/New_York" {
		t.Errorf("scheduler timezone: %s", h.app.scheduler.Location())
	}
	// The Nano was told about the brightness and resynced for the new timezone.
	if c, ok := h.nano.LastOfKind("brightness"); !ok || c.Value != 30 {
		t.Errorf("brightness: %+v", c)
	}
	if len(h.nano.CommandsOfKind("time")) == 0 {
		t.Error("the Nano was not resynced after the timezone change")
	}

	// And they survive a restart: the values come back from the database.
	if got := storage.GetString(h.store, storage.KeyTimezone, ""); got != "America/New_York" {
		t.Errorf("persisted timezone: %q", got)
	}
	if got := h.app.Settings(); got.DefaultSoundID != "alarm2" {
		t.Errorf("persisted sound: %+v", got)
	}
}

func TestSettingsRejectInvalidValues(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	if _, err := h.app.UpdateSettings(ctx, webSettings("Mars/Olympus", false, 50, "", true)); err == nil {
		t.Error("an unknown timezone should be rejected")
	}
	if _, err := h.app.UpdateSettings(ctx, webSettings("UTC", false, 50, "does-not-exist", true)); err == nil {
		t.Error("a missing sound should be rejected")
	}
}

func TestVolumeIsNotRestoredFromStorageAtStartup(t *testing.T) {
	// The physical knob is authoritative; a stale stored value must never be
	// replayed over it.
	h := newHarness(t, nil)
	if err := storage.SetInt(h.store, storage.KeyLastKnownVolume, 95); err != nil {
		t.Fatal(err)
	}

	pct, authoritative := h.app.audio.Volume()
	if authoritative {
		t.Error("volume should not be authoritative before the knob reports")
	}
	if pct != h.app.cfg.Audio.StartupVolumePercent {
		t.Errorf("startup volume should come from the config, not storage: %d", pct)
	}
}

func TestSnapshotAndHealthOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	h.syncChannels(t)
	handler := h.app.web.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("state: %d", rec.Code)
	}
	var snap state.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Channels) != 4 {
		t.Errorf("channels: %+v", snap.Channels)
	}
	if snap.Clock.Timezone != "UTC" {
		t.Errorf("timezone: %s", snap.Clock.Timezone)
	}
	if snap.Inputs.ChannelKnob != -1 {
		t.Errorf("an unreported knob should read -1, got %v", snap.Inputs.ChannelKnob)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health: %d", rec.Code)
	}
}

func TestAlarmCreatedOverHTTPIsScheduledAndPersisted(t *testing.T) {
	h := newHarness(t, nil)
	handler := h.app.web.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/alarms",
		strings.NewReader(`{"hour":7,"minute":15,"label":"Work","repeat_days":62}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	// The scheduler knows about it immediately.
	alarms := h.app.scheduler.List()
	if len(alarms) != 1 || alarms[0].Hour != 7 {
		t.Fatalf("scheduler: %+v", alarms)
	}
	if next := h.app.scheduler.Next(); next == nil {
		t.Error("the new alarm was not scheduled")
	}

	// And it is in the database, not just in memory.
	stored, err := h.store.Alarms()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Label != "Work" {
		t.Errorf("stored: %+v", stored)
	}
}

func TestShutdownSilencesEverything(t *testing.T) {
	h := newHarness(t, nil)

	a := alarm.New(6, 30)
	saved, err := h.app.scheduler.Save(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.scheduler.Trigger(saved.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alarm audio", func() bool { return len(h.audio.Plays()) > 0 })

	h.app.shutdown()

	if hd := h.audio.LastHandle(); hd == nil || !hd.Stopped() {
		t.Error("shutdown left the alarm speaker playing")
	}
	if c, ok := h.nano.LastOfKind("alarm"); !ok || c.Flag {
		t.Error("shutdown left the Nano believing an alarm is active")
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	h := newHarness(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.app.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestSupervisorRestartsAPanickingSubsystem(t *testing.T) {
	// A bug in one subsystem must not take the daemon — and the alarm clock —
	// down with it.
	sup := newSupervisor(logging.Discard())

	starts := make(chan struct{}, 8)
	sup.add("flaky", false, func(ctx context.Context) error {
		select {
		case starts <- struct{}{}:
		default:
		}
		panic("subsystem bug")
	})
	sup.add("steady", false, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_ = sup.run(ctx)

	if len(starts) < 2 {
		t.Errorf("the panicking subsystem was restarted %d times, want at least 2", len(starts))
	}
}

func TestSupervisorCriticalFailureStopsTheDaemon(t *testing.T) {
	sup := newSupervisor(logging.Discard())
	boom := errors.New("the database is gone")
	sup.add("alarm-scheduler", true, func(context.Context) error { return boom })
	sup.add("web", false, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })

	err := sup.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "alarm-scheduler") {
		t.Fatalf("got %v", err)
	}
}

func TestSupervisorNonCriticalFailureIsTolerated(t *testing.T) {
	sup := newSupervisor(logging.Discard())
	sup.add("tv-player", false, func(context.Context) error { return errors.New("mpv is not installed") })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := sup.run(ctx)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a non-critical failure should not stop the daemon: %v", err)
	}
	if _, ok := sup.failures()["tv-player"]; !ok {
		t.Error("the failure should be recorded for the health endpoint")
	}
}

// webSettings is a small constructor so the tests read clearly.
func webSettings(tz string, clock24 bool, brightness int, sound string, overlay bool) web.Settings {
	return web.Settings{
		Timezone: tz, Clock24h: clock24,
		DisplayBrightness: brightness, DefaultSoundID: sound, OverlayEnabled: overlay,
	}
}

func TestNewWithExistingAlarmsDoesNotPanic(t *testing.T) {
	// Loading the alarm set during construction recomputes the next occurrence
	// and publishes a schedule-changed event — before the WebSocket hub exists.
	// This is the ordinary second-boot path on a device that has any alarms at
	// all, so it must not depend on construction order.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "timeblaster.db")

	seed, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	a := alarm.New(6, 30)
	a.RepeatDays = alarm.Weekdays
	a.Label = "Survives a restart"
	if _, err := seed.SaveAlarm(a); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	soundsDir := filepath.Join(dir, "sounds")
	if err := os.MkdirAll(soundsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Storage.DatabasePath = dbPath
	cfg.Storage.AlarmSoundsDir = soundsDir
	cfg.Storage.RuntimeDir = filepath.Join(dir, "run")
	cfg.Web.ListenAddress = "127.0.0.1:0"
	cfg.General.Timezone = "UTC"

	cardsPath := filepath.Join(dir, "cards")
	if err := os.WriteFile(cardsPath, []byte(" 1 [Device         ]: USB-Audio - USB Audio Device\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := system.ALSAProbe{CardsPath: cardsPath, Runner: system.NewFakeRunner()}

	application, err := New(cfg, logging.Discard(), "test", Deps{
		Clock:  system.NewFakeClock(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)),
		Runner: system.NewFakeRunner(),
		Nano:   hardware.NewFakeNano(), Player: mpv.NewFake(), ErsatzTV: ersatztv.NewFake(),
		AudioPlayer: audio.NewFakePlayer(), WiFi: wifi.NewFakeManager(), ALSAProbe: &probe,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer application.shutdown()

	alarms := application.scheduler.List()
	if len(alarms) != 1 || alarms[0].Label != "Survives a restart" {
		t.Fatalf("alarms did not survive the restart: %+v", alarms)
	}
	if application.scheduler.Next() == nil {
		t.Error("the restored alarm was not scheduled")
	}
}

func TestSystemTimezoneNameFromEnvironment(t *testing.T) {
	// time.Local reports itself as "Local", which is useless in the companion
	// app. Resolving the real IANA name is what makes the UI show something a
	// person recognises.
	t.Setenv("TZ", "America/New_York")
	if got := SystemTimezoneName(); got != "America/New_York" {
		t.Errorf("got %q", got)
	}

	t.Setenv("TZ", "")
	// With no TZ set the result depends on the host; it must at least be a
	// loadable zone name or empty, never something that breaks scheduling.
	if name := SystemTimezoneName(); name != "" {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("SystemTimezoneName returned an unloadable zone %q: %v", name, err)
		}
	}
}
