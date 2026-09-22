package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/system"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type recordingObserver struct {
	mu       sync.Mutex
	lists    [][]ersatztv.Channel
	channels []*ersatztv.Channel
}

func (r *recordingObserver) ChannelListChanged(chs []ersatztv.Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lists = append(r.lists, chs)
}

func (r *recordingObserver) ChannelChanged(ch *ersatztv.Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = append(r.channels, ch)
}

func (r *recordingObserver) lastChannel() (*ersatztv.Channel, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.channels) == 0 {
		return nil, false
	}
	return r.channels[len(r.channels)-1], true
}

func (r *recordingObserver) listCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lists)
}

type fixture struct {
	svc     *Service
	etv     *ersatztv.Fake
	player  *mpv.Fake
	obs     *recordingObserver
	clock   *system.FakeClock
	overlay *mpv.OverlayRenderer
}

func newFixture(t *testing.T, channels ...ersatztv.Channel) *fixture {
	t.Helper()
	etv := ersatztv.NewFake(channels...)
	player := mpv.NewFake()
	clk := system.NewFakeClock(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC))
	obs := &recordingObserver{}

	cfg := config.Default()
	overlay := mpv.NewOverlayRenderer(cfg.Overlay, player, clk, testLogger())

	svc := NewService(cfg.ErsatzTV, cfg.MPV, Deps{
		ErsatzTV: etv, Player: player, Overlay: overlay,
		Clock: clk, Logger: testLogger(), Observer: obs,
	})
	return &fixture{svc: svc, etv: etv, player: player, obs: obs, clock: clk, overlay: overlay}
}

func chans() []ersatztv.Channel {
	return []ersatztv.Channel{
		{ID: 1, Number: "1", Name: "Movies"},
		{ID: 2, Number: "2", Name: "Cartoons"},
		{ID: 3, Number: "3", Name: "Sci-Fi"},
	}
}

func TestRefreshLoadsChannels(t *testing.T) {
	f := newFixture(t, chans()...)
	if err := f.svc.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := f.svc.ChannelCount(); got != 3 {
		t.Errorf("channel count: %d", got)
	}
	if f.obs.listCount() != 1 {
		t.Errorf("observer not notified: %d", f.obs.listCount())
	}
	st := f.svc.Status()
	if !st.ErsatzTVReachable || st.ChannelCount != 3 {
		t.Errorf("status: %+v", st)
	}
}

func TestRefreshKeepsChannelsWhenErsatzTVGoesAway(t *testing.T) {
	// ErsatzTV restarting must not blank the television.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 1); err != nil {
		t.Fatal(err)
	}

	f.etv.SetError(errors.New("connection refused"))
	if err := f.svc.Refresh(ctx); err == nil {
		t.Fatal("expected an error")
	}

	if got := f.svc.ChannelCount(); got != 3 {
		t.Errorf("channel list was discarded: %d", got)
	}
	if cur := f.svc.Current(); cur == nil || cur.Number != "2" {
		t.Errorf("current channel was lost: %+v", cur)
	}
	if st := f.svc.Status(); st.ErsatzTVReachable || st.LastError == "" {
		t.Errorf("status: %+v", st)
	}
}

func TestRefreshDropsASelectionThatDisappeared(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectNumber(ctx, "3"); err != nil {
		t.Fatal(err)
	}

	// The user deleted channel 3 in the ErsatzTV web UI.
	f.etv.SetChannels(chans()[:2]...)
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if f.svc.Current() != nil {
		t.Errorf("stale selection kept: %+v", f.svc.Current())
	}
	if !lastLoadIsNoChannel(t, f) {
		t.Error("expected the no-channel image to be shown")
	}
}

func TestSelectBandMapsPositionToChannel(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	for band, wantNumber := range map[int]string{0: "1", 1: "2", 2: "3"} {
		if err := f.svc.SelectBand(ctx, band); err != nil {
			t.Fatalf("SelectBand(%d): %v", band, err)
		}
		cur := f.svc.Current()
		if cur == nil || cur.Number != wantNumber {
			t.Fatalf("band %d selected %+v, want channel %s", band, cur, wantNumber)
		}
	}
}

func TestSelectBandOutOfRange(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	// A band past the end clamps to the last channel rather than blanking the TV.
	if err := f.svc.SelectBand(ctx, 99); err != nil {
		t.Fatal(err)
	}
	if cur := f.svc.Current(); cur == nil || cur.Number != "3" {
		t.Errorf("clamp: %+v", cur)
	}

	// Band -1 means "no channel".
	if err := f.svc.SelectBand(ctx, -1); err != nil {
		t.Fatal(err)
	}
	if f.svc.Current() != nil {
		t.Errorf("band -1 should deselect: %+v", f.svc.Current())
	}
}

func TestSelectBandWithNoChannels(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 0); err != nil {
		t.Fatalf("SelectBand with no channels should show the static image: %v", err)
	}
	if !lastLoadIsNoChannel(t, f) {
		t.Error("expected the no-channel image")
	}
}

func TestSelectChannelLoadsTheStreamAndDrawsTheOverlay(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	// Becoming ready paints the no-channel screen, so start counting from here.
	f.player.Reset()

	if err := f.svc.SelectBand(ctx, 2); err != nil {
		t.Fatal(err)
	}

	loads := f.player.CallsNamed("loadfile")
	if len(loads) != 1 {
		t.Fatalf("loadfile calls: %+v", loads)
	}
	url, _ := loads[0].Args[1].(string)
	if !strings.HasSuffix(url, "/iptv/channel/3.m3u8") {
		t.Errorf("stream URL: %q", url)
	}
	if loads[0].Args[2] != "replace" {
		t.Errorf("channel changes must replace, not append: %+v", loads[0].Args)
	}

	overlays := f.player.CallsNamed("osd-overlay")
	if len(overlays) != 1 {
		t.Fatalf("overlay calls: %+v", overlays)
	}
	if data, _ := overlays[0].Args[3].(string); !strings.HasSuffix(data, "CH 3") {
		t.Errorf("overlay text: %q", data)
	}
}

func TestSelectingTheSameChannelIsANoOp(t *testing.T) {
	// The channel knob emits a selection on every meaningful move; reloading the
	// stream each time would restart playback for no reason.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 1); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()

	if err := f.svc.SelectBand(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if n := len(f.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("reselecting the same channel issued %d loadfile calls", n)
	}
}

func TestSelectChannelReportsPlayerFailure(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	f.player.NotAlive = true

	if err := f.svc.SelectBand(ctx, 0); err == nil {
		t.Fatal("expected an error when mpv is down")
	}
	if f.svc.Current() != nil {
		t.Error("a failed load must not record a current channel")
	}
	if st := f.svc.Status(); st.LastError == "" || st.PlayerAlive {
		t.Errorf("status: %+v", st)
	}
}

func TestSelectNumberUnknownChannel(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectNumber(ctx, "42"); !errors.Is(err, ErrNoChannels) {
		t.Errorf("got %v", err)
	}
}

func TestShowNoChannelLoadsTheStaticImage(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	// Once the channel list is known, the standby screen is the no-channel one.
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatalf("ShowNoChannel: %v", err)
	}
	if !lastLoadIsNoChannel(t, f) {
		t.Error("expected the configured image")
	}

	// Repeating it must not reload and make the television flicker.
	f.player.Reset()
	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("redundant reload: %d calls", n)
	}
}

func TestShowNoChannelWithoutAConfiguredImageStopsPlayback(t *testing.T) {
	etv := ersatztv.NewFake()
	player := mpv.NewFake()
	cfg := config.Default()
	cfg.MPV.NoChannelImage = ""
	cfg.MPV.BootingImage = ""
	svc := NewService(cfg.ErsatzTV, cfg.MPV, Deps{
		ErsatzTV: etv, Player: player, Clock: system.NewFakeClock(time.Now()), Logger: testLogger(),
	})
	if err := svc.ShowNoChannel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(player.CallsNamed("stop")) != 1 {
		t.Errorf("expected a stop command: %+v", player.Calls())
	}
}

func TestRestorePlaybackReloadsAfterAPlayerRestart(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 1); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()

	// mpv crashed and the supervisor handed back a fresh, idle process.
	f.svc.RestorePlayback(ctx)

	loads := f.player.CallsNamed("loadfile")
	if len(loads) != 1 {
		t.Fatalf("loadfile calls: %+v", loads)
	}
	if url, _ := loads[0].Args[1].(string); !strings.Contains(url, "/channel/2.") {
		t.Errorf("wrong channel restored: %q", url)
	}
	if cur := f.svc.Current(); cur == nil || cur.Number != "2" {
		t.Errorf("current: %+v", cur)
	}
}

func TestRestorePlaybackShowsTheStaticImageWhenNothingWasSelected(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	f.svc.RestorePlayback(ctx)
	if !lastLoadIsNoChannel(t, f) {
		t.Error("expected the no-channel image")
	}
}

func TestBootingScreenShowsUntilTheChannelListLoads(t *testing.T) {
	// ErsatzTV takes tens of seconds to come up. Until it has, telling the user
	// to turn the channel knob would be inviting them to do something that
	// cannot work, so the booting screen stays.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	f.etv.SetError(errors.New("ersatztv is still starting"))

	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	if !lastLoadIs(t, f, "booting.png") {
		t.Fatalf("expected the booting screen, got %+v", f.player.CallsNamed("loadfile"))
	}

	// A failed refresh must not end the booting state.
	_ = f.svc.Refresh(ctx)
	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	if !lastLoadIs(t, f, "booting.png") {
		t.Error("a failed refresh should leave the booting screen up")
	}

	// ErsatzTV finishes starting. Refresh itself does not touch the screen --
	// deciding here as well raced the channel knob -- it announces the new list
	// and the caller acts on it.
	f.etv.SetError(nil)
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if f.obs.listCount() == 0 {
		t.Fatal("the new channel list was not announced")
	}

	// Acting on it swaps the screen, because the service is now ready.
	if err := f.svc.SelectBand(ctx, -1); err != nil {
		t.Fatal(err)
	}
	if !lastLoadIsNoChannel(t, f) {
		t.Errorf("expected the no-channel screen once ready, got %+v",
			f.player.CallsNamed("loadfile"))
	}
}

func TestRefreshDoesNotTouchTheScreen(t *testing.T) {
	// The regression this guards: Refresh announced the channel list, the knob
	// picked channel 1 asynchronously, and Refresh then found Current() still
	// nil and replaced it with the standby image three milliseconds later.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("Refresh loaded %d files; deciding what is on screen is the caller's job", n)
	}
}

func TestBootingScreenGivesUpAfterTheDeadline(t *testing.T) {
	// If ErsatzTV never comes up the device is not booting, something is broken.
	// Claiming to still be booting forever would be a lie.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	f.etv.SetError(errors.New("ersatztv is not installed"))

	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	if !lastLoadIs(t, f, "booting.png") {
		t.Fatal("expected the booting screen")
	}

	f.clock.Advance(2 * time.Minute)
	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	if !lastLoadIsNoChannel(t, f) {
		t.Errorf("expected the no-channel screen past the deadline, got %+v",
			f.player.CallsNamed("loadfile"))
	}
}

func TestStandbyScreenIsNotReloadedRedundantly(t *testing.T) {
	// Reloading the same image on every failed refresh would make the television
	// flicker for no reason.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	f.etv.SetError(errors.New("down"))

	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()
	for range 3 {
		if err := f.svc.ShowNoChannel(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("redundant reloads: %d", n)
	}
}

func TestRestorePlaybackFallsBackWhenTheChannelWillNotLoad(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 0); err != nil {
		t.Fatal(err)
	}

	// The stream now fails, but the static image still loads.
	f.player.OnCommand = func(c mpv.Call) (json.RawMessage, error) {
		if c.Name() == "loadfile" {
			if url, _ := c.Args[1].(string); strings.Contains(url, "iptv") {
				return nil, errors.New("stream unavailable")
			}
		}
		return nil, nil
	}
	f.player.Reset()
	f.svc.RestorePlayback(ctx)

	if f.svc.Current() != nil {
		t.Errorf("current should be cleared: %+v", f.svc.Current())
	}
	if !lastLoadIsNoChannel(t, f) {
		t.Error("expected a fallback to the no-channel image")
	}
}

func TestHandlePlayerEventReloadsAFailedStream(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 0); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()

	f.svc.HandlePlayerEvent(mpv.Event{Name: "end-file", Reason: "error", Error: "loading failed"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.player.CallsNamed("loadfile")) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	loads := f.player.CallsNamed("loadfile")
	if len(loads) == 0 {
		t.Fatal("a failed stream was not reloaded")
	}
	if url, _ := loads[0].Args[1].(string); !strings.Contains(url, "/channel/1.") {
		t.Errorf("reloaded the wrong thing: %q", url)
	}
}

func TestHandlePlayerEventIgnoresDeliberateStops(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 0); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()

	for _, ev := range []mpv.Event{
		{Name: "end-file", Reason: "stop"},
		{Name: "end-file", Reason: "quit"},
		{Name: "end-file", Reason: "redirect"},
		{Name: "playback-restart"},
	} {
		f.svc.HandlePlayerEvent(ev)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(f.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("benign events triggered %d reloads", n)
	}
}

func TestDeliberateStopsOnTheStandbyImageDoNotReload(t *testing.T) {
	// "stop" and "quit" are our own doing -- they are what a channel change
	// looks like from mpv's side. Reloading the standby image in response would
	// fight whatever we were about to play.
	f := newFixture(t, chans()...)
	if err := f.svc.ShowNoChannel(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()

	for _, reason := range []string{"stop", "quit", "redirect"} {
		f.svc.HandlePlayerEvent(mpv.Event{Name: "end-file", Reason: reason})
	}
	time.Sleep(50 * time.Millisecond)

	if n := len(f.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("a deliberate stop triggered %d reloads", n)
	}
}

func TestRunRetriesWithBackoffThenSettles(t *testing.T) {
	f := newFixture(t, chans()...)
	f.etv.SetError(errors.New("connection refused"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.svc.Run(ctx) }()

	// Several failed attempts, each waiting on the fake clock.
	for range 3 {
		waitForWaiter(t, f.clock)
		f.clock.Advance(2 * time.Minute)
	}
	if f.etv.CallCount() < 3 {
		t.Errorf("expected repeated retries, got %d", f.etv.CallCount())
	}

	// ErsatzTV finishes starting up.
	f.etv.SetError(nil)
	waitForWaiter(t, f.clock)
	f.clock.Advance(2 * time.Minute)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.svc.ChannelCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	if f.svc.ChannelCount() != 3 {
		t.Errorf("channels were not picked up after recovery: %d", f.svc.ChannelCount())
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRefreshSoonWakesTheLoop(t *testing.T) {
	f := newFixture(t, chans()...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = f.svc.Run(ctx) }()

	waitForWaiter(t, f.clock)
	before := f.etv.CallCount()
	f.svc.RefreshSoon()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.etv.CallCount() <= before {
		time.Sleep(time.Millisecond)
	}
	if f.etv.CallCount() <= before {
		t.Error("RefreshSoon did not trigger a refresh")
	}
}

func TestChannelNumberInt(t *testing.T) {
	if n, ok := ChannelNumberInt("12"); !ok || n != 12 {
		t.Errorf("got %d, %v", n, ok)
	}
	if _, ok := ChannelNumberInt("2.1"); ok {
		t.Error("a sub-channel number is not an integer")
	}
}

func lastLoadIs(t *testing.T, f *fixture, suffix string) bool {
	t.Helper()
	loads := f.player.CallsNamed("loadfile")
	if len(loads) == 0 {
		return false
	}
	url, _ := loads[len(loads)-1].Args[1].(string)
	return strings.HasSuffix(url, suffix)
}

func lastLoadIsNoChannel(t *testing.T, f *fixture) bool {
	t.Helper()
	return lastLoadIs(t, f, "no-channel.png")
}

func waitForWaiter(t *testing.T, clk *system.FakeClock) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if clk.Waiters() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no timer was registered")
}

func TestStandbyImageBeforeThePlayerIsUpIsNotAnError(t *testing.T) {
	// At startup the daemon puts something on screen before mpv has finished
	// claiming the display. That is normal and self-correcting, so it must not
	// produce a red line in every boot -- but it must still be reported to the
	// caller, and recorded, so a genuinely stuck player is still visible.
	f := newFixture(t, chans()...)
	f.player.NotAlive = true

	err := f.svc.ShowNoChannel(context.Background())
	if !errors.Is(err, mpv.ErrNotConnected) {
		t.Fatalf("got %v, want mpv.ErrNotConnected", err)
	}
	if f.svc.Status().LastError == "" {
		t.Error("the failure should still be recorded in status")
	}

	// And once mpv connects, the supervisor's restore puts it right.
	f.player.NotAlive = false
	f.svc.RestorePlayback(context.Background())
	if !lastLoadIs(t, f, "booting.png") {
		t.Errorf("expected the standby screen after the player came up, got %+v",
			f.player.CallsNamed("loadfile"))
	}
}

func TestStandbyImageEndingIsRestoredNotIgnored(t *testing.T) {
	// The bug this guards: mpv shows an image for image-display-duration and
	// then goes idle, so the television went black a few seconds after the
	// standby screen appeared. The daemon has to notice and put it back.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()

	f.svc.HandlePlayerEvent(mpv.Event{Name: "end-file", Reason: "eof"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.player.CallsNamed("loadfile")) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !lastLoadIsNoChannel(t, f) {
		t.Fatalf("the standby image was not restored: %+v", f.player.CallsNamed("loadfile"))
	}
}

func TestDefaultMPVArgsKeepImagesOnScreenForever(t *testing.T) {
	// A five-second standby screen is worse than none: it looks like the device
	// crashed. Pin the option that prevents it.
	args := strings.Join(config.Default().MPV.Args, " ")
	if !strings.Contains(args, "--image-display-duration=inf") {
		t.Errorf("mpv defaults must hold a still image indefinitely: %s", args)
	}
}

func TestPlaybackRestartReleasesTheChannelBanner(t *testing.T) {
	// The banner holds from the moment a channel is requested until the picture
	// arrives, so the wiring from mpv's event through to the overlay has to be
	// connected -- without it the banner would sit there until max_hold.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if !f.overlay.Awaiting() {
		t.Fatal("the banner should be holding for the picture")
	}

	f.svc.HandlePlayerEvent(mpv.Event{Name: "playback-restart"})

	if f.overlay.Awaiting() {
		t.Error("playback starting should have released the banner")
	}
}

func TestPlaybackRestartIsNotMistakenForAStreamEnding(t *testing.T) {
	// It arrives on the same event stream as end-file and must not trigger the
	// stream-failure recovery path.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 0); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()

	f.svc.HandlePlayerEvent(mpv.Event{Name: "playback-restart"})
	time.Sleep(50 * time.Millisecond)

	if n := len(f.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("playback-restart triggered %d reloads", n)
	}
	if cur := f.svc.Current(); cur == nil || cur.Number != "1" {
		t.Errorf("current channel disturbed: %+v", cur)
	}
}
