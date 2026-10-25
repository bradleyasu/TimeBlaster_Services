package ambience

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/screen"
	"github.com/bradsheets/timeblaster/internal/system"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// touch creates files with content under root.
func touch(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func names(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// --- scanning ----------------------------------------------------------------

func TestVideosPlayInNaturalOrder(t *testing.T) {
	// Files numbered by a person: 10 comes after 9, not after 1.
	root := t.TempDir()
	touch(t, root, "ch1/10.mp4", "ch1/2.mp4", "ch1/1.mp4", "ch1/9.MP4", "ch1/01b.mp4")
	got, err := Videos(filepath.Join(root, "ch1"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.mp4", "01b.mp4", "2.mp4", "9.MP4", "10.mp4"}
	if !slices.Equal(names(got), want) {
		t.Errorf("order %v, want %v", names(got), want)
	}
}

func TestVideosSkipsWhatIsNotAVideo(t *testing.T) {
	root := t.TempDir()
	// "._1.mp4" is the resource fork a Mac leaves beside every copied file.
	touch(t, root, "ch1/1.mp4", "ch1/._1.mp4", "ch1/notes.txt", "ch1/thumb.jpg", "ch1/2.mkv")
	if err := os.WriteFile(filepath.Join(root, "ch1/empty.mp4"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := Videos(filepath.Join(root, "ch1"))
	if want := []string{"1.mp4", "2.mkv"}; !slices.Equal(names(got), want) {
		t.Errorf("got %v, want %v", names(got), want)
	}
}

func TestScanFindsChannelFolders(t *testing.T) {
	root := t.TempDir()
	touch(t, root,
		"ch2/1.mp4",
		"ch10/1.mp4",
		"ch1-fire_place/a.mp4",
		"CH03/1.mp4",
		"music/1.mp4",    // not a channel
		"ch4/readme.txt", // no videos: no dead knob position
		"ch0/1.mp4",      // channels start at 1
	)
	if err := os.Mkdir(filepath.Join(root, "ch5"), 0o755); err != nil {
		t.Fatal(err)
	}

	chs, skipped, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, c := range chs {
		got = append(got, c.Number)
	}
	if want := []int{1, 2, 3, 10}; !slices.Equal(got, want) {
		t.Errorf("channels %v, want %v", got, want)
	}
	if chs[0].Name != "Fire Place" {
		t.Errorf("name %q, want Fire Place", chs[0].Name)
	}
	if chs[0].Label() != "AMBIENCE 1 FIRE PLACE" || chs[1].Label() != "AMBIENCE 2" {
		t.Errorf("labels %q, %q", chs[0].Label(), chs[1].Label())
	}
	for _, want := range []string{"music", "ch4", "ch5", "ch0"} {
		if !slices.ContainsFunc(skipped, func(s string) bool { return strings.HasPrefix(s, want+":") }) {
			t.Errorf("%s was not reported as skipped: %v", want, skipped)
		}
	}
}

func TestScanKeepsOneFolderPerNumber(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "ch1-rain/1.mp4", "ch01/1.mp4")
	chs, skipped, _ := Scan(root)
	if len(chs) != 1 || filepath.Base(chs[0].Dir) != "ch01" {
		t.Errorf("kept %+v, want ch01 alone", chs)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "ch1-rain") {
		t.Errorf("the dropped folder was not reported: %v", skipped)
	}
}

func TestScanOfAMissingFolderIsNoChannels(t *testing.T) {
	chs, _, err := Scan(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(chs) != 0 {
		t.Errorf("got %v, %v; want nothing and no error", chs, err)
	}
}

// --- playback ----------------------------------------------------------------

type recorder struct {
	mu       sync.Mutex
	channels [][]Channel
	playing  []*Channel
}

func (r *recorder) AmbienceChannelsChanged(chs []Channel) {
	r.mu.Lock()
	r.channels = append(r.channels, chs)
	r.mu.Unlock()
}

func (r *recorder) AmbienceChannelChanged(ch *Channel) {
	r.mu.Lock()
	r.playing = append(r.playing, ch)
	r.mu.Unlock()
}

func (r *recorder) listChanges() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.channels)
}

type fixture struct {
	m      *Mode
	player *mpv.Fake
	clock  *system.FakeClock
	obs    *recorder
	root   string
}

func newFixture(t *testing.T, files ...string) *fixture {
	t.Helper()
	root := t.TempDir()
	touch(t, root, files...)
	player := mpv.NewFake()
	clk := system.NewFakeClock(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	cfg := config.Default()
	obs := &recorder{}
	m := New(Options{
		Dir: root, RescanInterval: 30 * time.Second, StandbyImage: "/standby.png",
		BannerDuration: 3 * time.Second, StallTimeout: 12 * time.Second,
		StallCheckInterval: 2 * time.Second, StallStartTimeout: 45 * time.Second,
		CommandTimeout: time.Second,
	}, Deps{
		Player: player, Overlay: mpv.NewOverlayRenderer(cfg.Overlay, player, clk, discard()),
		Clock: clk, Logger: discard(), Observer: obs,
	})
	m.Activate()
	return &fixture{m: m, player: player, clock: clk, obs: obs, root: root}
}

// loads returns what was loaded, by file name, with ":loop" marking a video
// looped inside mpv.
func (f *fixture) loads() []string {
	var out []string
	for _, c := range f.player.CallsNamed("loadfile") {
		s := filepath.Base(c.Args[1].(string))
		if len(c.Args) >= 5 && c.Args[4] == "loop-file=inf" {
			s += ":loop"
		}
		out = append(out, s)
	}
	return out
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

func (f *fixture) eof() { f.m.HandlePlayerEvent(mpv.Event{Name: "end-file", Reason: "eof"}) }
func (f *fixture) picture() {
	f.m.HandlePlayerEvent(mpv.Event{Name: "playback-restart"})
}

func TestASingleVideoLoopsInsideMpv(t *testing.T) {
	// One file: a seamless loop by mpv itself, not a reload per pass.
	f := newFixture(t, "ch1/fire.mp4")
	if err := f.m.SelectBand(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := f.loads(); !slices.Equal(got, []string{"fire.mp4:loop"}) {
		t.Errorf("loads %v", got)
	}
	if s := f.m.Showing(); s.Number != "1" {
		t.Errorf("showing %+v", s)
	}
}

func TestSeveralVideosPlayInOrderAndRepeat(t *testing.T) {
	f := newFixture(t, "ch1/2.mp4", "ch1/1.mp4", "ch1/10.mp4")
	if err := f.m.SelectBand(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.picture()
		n := len(f.loads())
		f.eof()
		waitFor(t, "the next video", func() bool { return len(f.loads()) == n+1 })
	}
	want := []string{"1.mp4", "2.mp4", "10.mp4", "1.mp4"}
	if got := f.loads(); !slices.Equal(got, want) {
		t.Errorf("played %v, want %v", got, want)
	}
	// Files in a sequence are not looped individually, or the sequence would
	// never advance.
	for _, l := range f.loads() {
		if strings.HasSuffix(l, ":loop") {
			t.Errorf("a video in a sequence was looped: %v", f.loads())
		}
	}
}

func TestTheChannelKnobPicksTheFolder(t *testing.T) {
	f := newFixture(t, "ch1/a.mp4", "ch2/b.mp4", "ch3/c.mp4")
	ctx := context.Background()
	_ = f.m.SelectBand(ctx, 1)
	_ = f.m.SelectBand(ctx, 1) // the same band again does nothing
	_ = f.m.SelectBand(ctx, 9) // past the end: the last channel
	if got := f.loads(); !slices.Equal(got, []string{"b.mp4:loop", "c.mp4:loop"}) {
		t.Errorf("loads %v", got)
	}
}

func TestNoVideosShowsTheStandbyImageWithAMessage(t *testing.T) {
	f := newFixture(t)
	if err := f.m.SelectBand(context.Background(), -1); err != nil {
		t.Fatal(err)
	}
	if got := f.loads(); !slices.Equal(got, []string{"standby.png"}) {
		t.Errorf("loads %v", got)
	}
	var said bool
	for _, c := range f.player.CallsNamed("osd-overlay") {
		if strings.Contains(c.String(), msgNoVideos) {
			said = true
		}
	}
	if !said {
		t.Error("an empty ambience screen did not say why")
	}
	// Asking again does not reload the same screen and make it flicker.
	_ = f.m.SelectBand(context.Background(), -1)
	if n := len(f.loads()); n != 1 {
		t.Errorf("the standby screen was reloaded: %d loads", n)
	}
}

func TestBrokenVideosAreSkippedThenGivenUpOn(t *testing.T) {
	f := newFixture(t, "ch1/1.mp4", "ch1/2.mp4")
	_ = f.m.SelectBand(context.Background(), 0)

	// Every file fails before producing a picture. Each failure waits out a
	// backoff on the fake clock, so this cannot spin.
	for i := 0; i < 10 && f.m.Showing().Number != ""; i++ {
		n := len(f.loads())
		f.m.HandlePlayerEvent(mpv.Event{Name: "end-file", Reason: "error", Error: "unsupported"})
		// Not immediately: the retry waits out its backoff.
		time.Sleep(5 * time.Millisecond)
		if len(f.loads()) > n && f.m.Showing().Number != "" {
			t.Fatalf("retried without a backoff: %v", f.loads())
		}
		waitFor(t, "the next attempt", func() bool {
			f.clock.Advance(100 * time.Millisecond)
			return len(f.loads()) > n || f.m.Showing().Number == ""
		})
	}
	if f.m.Showing().Number != "" {
		t.Fatal("a channel where nothing plays was never given up on")
	}
	if last := f.loads()[len(f.loads())-1]; last != "standby.png" {
		t.Errorf("gave up onto %s, want the standby image", last)
	}
	if n := len(f.loads()); n > 2+maxFailures+2 {
		t.Errorf("%d loads before giving up; the budget is not being applied", n)
	}
}

func TestAnEndBeforeAnyPictureCountsAsAFailure(t *testing.T) {
	// A truncated file can report a clean end without ever showing a frame.
	// Treated as success it would be reloaded as fast as mpv can open it.
	f := newFixture(t, "ch1/broken.mp4")
	_ = f.m.SelectBand(context.Background(), 0)
	f.eof()
	time.Sleep(20 * time.Millisecond)
	if n := len(f.loads()); n != 1 {
		t.Errorf("reloaded immediately with no backoff: %v", f.loads())
	}
}

func TestStaleWorkDoesNotOverrideANewerSelection(t *testing.T) {
	// The end of channel 1's video arrives just as the knob moves to channel
	// 2. Moving to channel 1's next video must not land on top of channel 2.
	f := newFixture(t, "ch1/a1.mp4", "ch1/a2.mp4", "ch2/b.mp4")
	ctx := context.Background()
	_ = f.m.SelectBand(ctx, 0)
	f.picture()
	f.m.mu.Lock()
	gen := f.m.gen
	f.m.mu.Unlock()

	_ = f.m.SelectBand(ctx, 1)
	f.m.goTo(ctx, gen, 1)
	if got := f.loads(); !slices.Equal(got, []string{"a1.mp4", "b.mp4:loop"}) {
		t.Errorf("loads %v; stale work replaced the new channel", got)
	}
}

func TestAnInactiveModeLeavesThePlayerAlone(t *testing.T) {
	f := newFixture(t, "ch1/a.mp4", "ch1/b.mp4")
	_ = f.m.SelectBand(context.Background(), 0)
	f.m.Deactivate()
	f.player.Reset()

	if err := f.m.SelectBand(context.Background(), 0); !errors.Is(err, screen.ErrInactive) {
		t.Errorf("got %v, want ErrInactive", err)
	}
	f.eof()
	f.m.RestorePlayback(context.Background())
	time.Sleep(20 * time.Millisecond)
	if calls := f.player.Calls(); len(calls) != 0 {
		t.Errorf("an inactive mode touched the player: %v", calls)
	}
	if f.m.Showing().Number != "" {
		t.Error("an inactive mode still claims a channel")
	}
}

func TestLeavingTakesDownAHeldMessage(t *testing.T) {
	// The TV's standby screen draws no overlay of its own, so a "no videos"
	// message left up would sit on top of it.
	f := newFixture(t)
	_ = f.m.SelectBand(context.Background(), -1)
	f.player.Reset()
	f.m.Deactivate()
	hidden := false
	for _, c := range f.player.CallsNamed("osd-overlay") {
		if len(c.Args) > 2 && c.Args[2] == "none" {
			hidden = true
		}
	}
	if !hidden {
		t.Error("the ambience message was left on screen for the next mode")
	}
}

func TestNewFoldersAreAnnounced(t *testing.T) {
	f := newFixture(t, "ch1/a.mp4")
	f.m.rescan(true)
	if f.obs.listChanges() != 0 {
		t.Fatal("a rescan that found nothing new was announced")
	}
	touch(t, f.root, "ch2/b.mp4")
	f.m.rescan(true)
	if f.obs.listChanges() != 1 || f.m.ChannelCount() != 2 {
		t.Errorf("changes=%d count=%d", f.obs.listChanges(), f.m.ChannelCount())
	}
}

func TestVideosAddedToThePlayingChannelStartPlaying(t *testing.T) {
	// One video looping; a second copied in. The knob has not moved, so only
	// the rescan can notice -- and the loop has to give way to a sequence.
	f := newFixture(t, "ch1/1.mp4")
	_ = f.m.SelectBand(context.Background(), 0)
	touch(t, f.root, "ch1/2.mp4")
	f.m.rescan(true)
	if got := f.loads(); !slices.Equal(got, []string{"1.mp4:loop", "1.mp4"}) {
		t.Errorf("loads %v; the new set did not start", got)
	}
}

func TestAStalledVideoIsReloaded(t *testing.T) {
	f := newFixture(t, "ch1/fire.mp4")
	_ = f.m.SelectBand(context.Background(), 0)
	f.picture()
	f.player.SetProp("time-pos", 41.0)

	ctx := context.Background()
	var st stallState
	f.m.checkStall(ctx, &st) // adopts the video
	f.m.checkStall(ctx, &st) // first position
	f.clock.Advance(13 * time.Second)
	f.m.checkStall(ctx, &st) // has not moved for longer than the timeout

	if got := f.loads(); !slices.Equal(got, []string{"fire.mp4:loop", "fire.mp4:loop"}) {
		t.Errorf("loads %v; the stalled video was not reloaded", got)
	}
}

func TestAPausedVideoIsNotAStall(t *testing.T) {
	f := newFixture(t, "ch1/fire.mp4")
	_ = f.m.SelectBand(context.Background(), 0)
	f.player.SetProp("time-pos", 41.0)
	f.player.SetProp("pause", true)

	ctx := context.Background()
	var st stallState
	f.m.checkStall(ctx, &st)
	f.m.checkStall(ctx, &st)
	f.clock.Advance(time.Minute)
	f.m.checkStall(ctx, &st)
	if n := len(f.loads()); n != 1 {
		t.Errorf("a paused video was reloaded: %v", f.loads())
	}
}

func TestAVideoStillStartingIsNotAStall(t *testing.T) {
	// No picture yet: the longer start-up allowance applies, as for the TV.
	f := newFixture(t, "ch1/fire.mp4")
	_ = f.m.SelectBand(context.Background(), 0)
	f.player.SetProp("time-pos", 0.0)

	ctx := context.Background()
	var st stallState
	f.m.checkStall(ctx, &st)
	f.m.checkStall(ctx, &st)
	f.clock.Advance(30 * time.Second)
	f.m.checkStall(ctx, &st)
	if n := len(f.loads()); n != 1 {
		t.Fatalf("a video still starting was reloaded after 30s: %v", f.loads())
	}
	f.clock.Advance(20 * time.Second)
	f.m.checkStall(ctx, &st)
	if n := len(f.loads()); n != 2 {
		t.Errorf("a video that never started was never reloaded: %v", f.loads())
	}
}
