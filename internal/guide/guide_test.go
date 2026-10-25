package guide

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/screen"
	"github.com/bradsheets/timeblaster/internal/system"
)

var et = time.FixedZone("EDT", -4*3600)

// at is a time today in EDT.
func at(h, m int) time.Time { return time.Date(2026, 10, 24, h, m, 0, 0, et) }

func prog(title string, h0, m0, h1, m1 int) ersatztv.Programme {
	return ersatztv.Programme{Title: title, Start: at(h0, m0), Stop: at(h1, m1)}
}

func lineup(n int) []ersatztv.GuideChannel {
	var out []ersatztv.GuideChannel
	for i := 1; i <= n; i++ {
		out = append(out, ersatztv.GuideChannel{
			Number: fmt.Sprint(i), Name: fmt.Sprintf("Channel %d", i),
			Programmes: []ersatztv.Programme{prog(fmt.Sprintf("Show %d", i), 21, 0, 22, 0)},
		})
	}
	return out
}

// lines returns the overlay's event lines containing s.
func lines(ass, s string) []string {
	var out []string
	for _, l := range strings.Split(ass, "\n") {
		if strings.Contains(l, s) {
			out = append(out, l)
		}
	}
	return out
}

// --- layout -------------------------------------------------------------------

func TestTheGridStartsAtTheHalfHourAndSpansNinetyMinutes(t *testing.T) {
	ass := Render(Frame{Now: at(21, 42), Channels: lineup(1)})
	for _, slot := range []string{"9:30 PM", "10:00 PM", "10:30 PM"} {
		if len(lines(ass, "}"+slot)) == 0 {
			t.Errorf("no %s column heading", slot)
		}
	}
	if len(lines(ass, "}11:00 PM")) != 0 {
		t.Error("the grid runs past ninety minutes")
	}
}

func TestAProgrammeThatBeganEarlierIsMarked(t *testing.T) {
	// 9:00-10:00, seen at 9:42: the grid starts at 9:30, so the cell starts at
	// the grid's left edge with the old guide's arrow.
	ass := Render(Frame{Now: at(21, 42), Channels: lineup(1)})
	if len(lines(ass, "}◄ Show 1")) != 1 {
		t.Errorf("no continuation arrow:\n%s", ass)
	}
}

func TestProgrammesOutsideTheWindowAreNotDrawn(t *testing.T) {
	ch := ersatztv.GuideChannel{Number: "1", Name: "A", Programmes: []ersatztv.Programme{
		prog("Gone", 19, 0, 20, 0),
		prog("Now", 21, 30, 22, 0),
		prog("Tomorrow", 23, 30, 23, 59),
	}}
	ass := Render(Frame{Now: at(21, 42), Channels: []ersatztv.GuideChannel{ch}})
	if len(lines(ass, "Gone")) != 0 || len(lines(ass, "Tomorrow")) != 0 {
		t.Error("a programme outside the window was drawn")
	}
	if len(lines(ass, "}Now")) == 0 {
		t.Error("the programme on now is missing")
	}
}

func TestLongTitlesAreCutToFitTheirCell(t *testing.T) {
	long := strings.Repeat("Extraordinarily Long Title ", 5)
	ch := ersatztv.GuideChannel{Number: "1", Name: "A", Programmes: []ersatztv.Programme{prog(long, 21, 30, 22, 0)}}
	ass := Render(Frame{Now: at(21, 42), Channels: []ersatztv.GuideChannel{ch}})
	cell := lines(ass, "Extraordinarily")
	// The cell row and the "now on" panel both show it.
	if len(cell) == 0 {
		t.Fatal("the title is missing")
	}
	for _, l := range cell {
		shown := l[strings.LastIndex(l, "}")+1:]
		if !strings.HasSuffix(shown, "…") {
			t.Errorf("a cut title is not marked: %q", shown)
		}
		if len([]rune(shown)) >= len([]rune(long)) {
			t.Errorf("the title was not shortened: %q", shown)
		}
	}
	// A thirty-minute cell is a third of the grid's width.
	if n := chars(timeW/3-2*gap-16, 24); len([]rune(cell[0][strings.LastIndex(cell[0], "}")+1:])) > n {
		t.Errorf("the cell text is wider than %d characters", n)
	}
}

func TestTitlesCannotInjectMarkup(t *testing.T) {
	// Titles come from ErsatzTV's library, not from us.
	ch := ersatztv.GuideChannel{Number: "1", Name: "A", Programmes: []ersatztv.Programme{
		prog(`Evil {\fs400}`, 21, 30, 22, 0)}}
	ass := Render(Frame{Now: at(21, 42), Channels: []ersatztv.GuideChannel{ch}})
	if strings.Contains(ass, `{\fs400}`) {
		t.Error("a title's markup reached the overlay unescaped")
	}
}

func TestTheCornerChannelIsHighlighted(t *testing.T) {
	ass := Render(Frame{Now: at(21, 42), Channels: lineup(3), Playing: "2"})
	if len(lines(ass, assColor(colHighlight))) == 0 {
		t.Error("no programme cell is highlighted")
	}
	if len(lines(ass, "}NOW ON 2  CHANNEL 2")) != 1 {
		t.Errorf("the panel does not name the corner channel:\n%s", ass)
	}
	if len(lines(ass, "}Show 2")) != 1 {
		t.Error("the panel does not show what is on")
	}
}

func TestPagingShowsFiveChannelsAtATime(t *testing.T) {
	chs := lineup(7)
	if Pages(len(chs)) != 2 {
		t.Fatalf("pages = %d, want 2", Pages(len(chs)))
	}
	first := Render(Frame{Now: at(21, 42), Channels: chs, Page: 0})
	second := Render(Frame{Now: at(21, 42), Channels: chs, Page: 1})
	if len(lines(first, "}CHANNEL 5")) != 1 || len(lines(first, "}CHANNEL 6")) != 0 {
		t.Error("page 1 does not hold channels 1-5")
	}
	if len(lines(second, "}CHANNEL 6")) != 1 || len(lines(second, "}CHANNEL 1")) != 0 {
		t.Error("page 2 does not hold channels 6-7")
	}
	if Render(Frame{Now: at(21, 42), Channels: chs, Page: 2}) != first {
		t.Error("paging does not wrap round")
	}
	if PageOf(chs, "7") != 1 || PageOf(chs, "9") != -1 {
		t.Error("PageOf is wrong")
	}
}

func TestTheClockFollowsThe24HourSetting(t *testing.T) {
	ass := Render(Frame{Now: at(21, 42), Clock24h: true, Channels: lineup(1)})
	if len(lines(ass, "}21:42")) != 1 || len(lines(ass, "}21:30")) != 1 {
		t.Error("24-hour time is not used")
	}
	if len(lines(ass, "}PM")) != 0 {
		t.Error("PM shown in 24-hour mode")
	}
}

func TestNoChannelsSaysSo(t *testing.T) {
	if len(lines(Render(Frame{Now: at(21, 42)}), "}NO CHANNELS")) != 1 {
		t.Error("an empty guide does not say why")
	}
}

// --- the mode ----------------------------------------------------------------

type fakeTV struct {
	mu       sync.Mutex
	calls    []string
	quiet    bool
	playing  string
	guide    ersatztv.Guide
	guideErr error
	guides   int
}

func (f *fakeTV) log(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeTV) Activate()    { f.log("activate") }
func (f *fakeTV) Deactivate()  { f.log("deactivate") }
func (f *fakeTV) SetQuiet(q bool) {
	f.mu.Lock()
	f.quiet = q
	f.mu.Unlock()
}
func (f *fakeTV) SelectBand(_ context.Context, band int) error {
	f.mu.Lock()
	f.playing = fmt.Sprint(band + 1)
	f.mu.Unlock()
	f.log(fmt.Sprintf("select %d", band))
	return nil
}
func (f *fakeTV) RestorePlayback(context.Context) { f.log("restore") }
func (f *fakeTV) HandlePlayerEvent(ev mpv.Event)  { f.log("event " + ev.Name) }
func (f *fakeTV) ChannelCount() int               { return len(f.guide.Channels) }
func (f *fakeTV) Showing() screen.Showing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return screen.Showing{Number: f.playing}
}
func (f *fakeTV) Guide(context.Context, time.Duration) (ersatztv.Guide, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.guides++
	return f.guide, f.guideErr
}
func (f *fakeTV) has(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

type fixture struct {
	m      *Mode
	tv     *fakeTV
	player *mpv.Fake
	clock  *system.FakeClock
}

func newFixture(t *testing.T, channels int) *fixture {
	t.Helper()
	tv := &fakeTV{guide: ersatztv.Guide{Channels: lineup(channels)}}
	player := mpv.NewFake()
	clk := system.NewFakeClock(at(21, 42))
	m := New(Options{
		PageInterval: 8 * time.Second, RefreshInterval: time.Minute,
		Location: func() *time.Location { return et },
	}, Deps{Player: player, TV: tv, Clock: clk, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return &fixture{m: m, tv: tv, player: player, clock: clk}
}

func (f *fixture) margins() (right, bottom any) {
	var r, b any
	for _, c := range f.player.CallsNamed("set_property") {
		switch c.Args[1] {
		case "video-margin-ratio-right":
			r = c.Args[2]
		case "video-margin-ratio-bottom":
			b = c.Args[2]
		}
	}
	return r, b
}

func (f *fixture) draws() []mpv.Call {
	var out []mpv.Call
	for _, c := range f.player.CallsNamed("osd-overlay") {
		if c.Args[1] == OverlayID && c.Args[2] == "ass-events" {
			out = append(out, c)
		}
	}
	return out
}

func TestTheGuideTakesTheScreen(t *testing.T) {
	f := newFixture(t, 3)
	f.m.Activate()
	if err := f.m.SelectBand(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if r, b := f.margins(); r != 0.5 || b != 0.5 {
		t.Errorf("the picture was not put in the corner: right=%v bottom=%v", r, b)
	}
	if !f.tv.has("activate") || !f.tv.has("select 1") {
		t.Errorf("the corner channel was not played: %v", f.tv.calls)
	}
	if !f.tv.quiet {
		t.Error("the TV's full-screen channel card would cover the guide")
	}
	if len(f.draws()) == 0 {
		t.Fatal("the guide was not drawn")
	}
	last := f.draws()[len(f.draws())-1].Args[3].(string)
	if len(lines(last, "}NOW ON 2")) != 1 {
		t.Error("the guide does not follow the corner channel")
	}
}

func TestLeavingRestoresTheFullPicture(t *testing.T) {
	// The next mode must inherit a full-size picture and a clean screen, and
	// get them before it starts.
	f := newFixture(t, 3)
	f.m.Activate()
	_ = f.m.SelectBand(context.Background(), 0)
	f.m.Deactivate()

	if r, b := f.margins(); r != 0.0 || b != 0.0 {
		t.Errorf("the picture was left shrunk: right=%v bottom=%v", r, b)
	}
	cleared := false
	for _, c := range f.player.CallsNamed("osd-overlay") {
		if c.Args[1] == OverlayID && c.Args[2] == "none" {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the guide was left on screen")
	}
	if !f.tv.has("deactivate") || f.tv.quiet {
		t.Error("the TV's playback was not handed back with its banner on")
	}
}

func TestDeactivatingAGuideThatWasNeverActiveTouchesNothing(t *testing.T) {
	// The director deactivates every other mode at start-up. The guide shares
	// the TV's playback, so a careless deactivate would switch the TV off.
	f := newFixture(t, 3)
	f.m.Deactivate()
	if len(f.player.Calls()) != 0 || f.tv.has("deactivate") {
		t.Errorf("an inactive guide acted: player=%v tv=%v", f.player.Calls(), f.tv.calls)
	}
}

func TestAnInactiveGuideStaysOffScreen(t *testing.T) {
	f := newFixture(t, 3)
	if err := f.m.SelectBand(context.Background(), 0); err != screen.ErrInactive {
		t.Errorf("got %v, want ErrInactive", err)
	}
	f.m.HandlePlayerEvent(mpv.Event{Name: "end-file"})
	if len(f.player.Calls()) != 0 || f.tv.has("select 0") || f.tv.has("event end-file") {
		t.Error("an inactive guide touched the screen or the TV")
	}
}

func TestAnUnchangedGuideIsNotResent(t *testing.T) {
	// Redrawn once a second; sent only when something visible changed.
	f := newFixture(t, 3)
	f.m.Activate()
	_ = f.m.SelectBand(context.Background(), 0)
	n := len(f.draws())
	ctx := context.Background()
	_ = f.m.draw(ctx, false)
	_ = f.m.draw(ctx, false)
	if len(f.draws()) != n {
		t.Errorf("an unchanged guide was resent: %d draws, had %d", len(f.draws()), n)
	}
	f.clock.Advance(time.Minute) // the clock moves on
	_ = f.m.draw(ctx, false)
	if len(f.draws()) != n+1 {
		t.Error("the guide was not redrawn when the clock changed")
	}
}

func TestPagesTurnAndAChannelChangeJumpsToItsPage(t *testing.T) {
	f := newFixture(t, 7)
	f.m.Activate()
	ctx := context.Background()
	_ = f.m.SelectBand(ctx, 0) // channel 1, page 1
	page := func() int { f.m.mu.Lock(); defer f.m.mu.Unlock(); return f.m.page }

	f.clock.Advance(8 * time.Second)
	_ = f.m.draw(ctx, false)
	if page() != 1 {
		t.Errorf("page %d after the interval, want 1", page())
	}
	f.clock.Advance(8 * time.Second)
	_ = f.m.draw(ctx, false)
	if page() != 0 {
		t.Errorf("page %d, want it to wrap to 0", page())
	}
	_ = f.m.SelectBand(ctx, 6) // channel 7 is on the second page
	if page() != 1 {
		t.Errorf("changing to channel 7 left the guide on page %d", page())
	}
}

func TestListingsAreNotReadEverySecond(t *testing.T) {
	// The guide redraws every second. A down ErsatzTV must not be asked
	// every second too.
	f := newFixture(t, 3)
	f.tv.guideErr = fmt.Errorf("unreachable")
	f.m.Activate()
	ctx := context.Background()
	for range 30 {
		_ = f.m.draw(ctx, false)
		f.clock.Advance(time.Second)
	}
	if f.tv.guides != 1 {
		t.Errorf("listings read %d times in 30 seconds, want 1", f.tv.guides)
	}
	f.clock.Advance(time.Minute)
	_ = f.m.draw(ctx, false)
	if f.tv.guides != 2 {
		t.Errorf("listings not re-read after a minute: %d reads", f.tv.guides)
	}
}

func TestARestartedPlayerGetsTheLayoutBack(t *testing.T) {
	// A fresh mpv forgets the picture's size and every overlay.
	f := newFixture(t, 3)
	f.m.Activate()
	_ = f.m.SelectBand(context.Background(), 0)
	f.player.Reset()
	f.m.RestorePlayback(context.Background())
	if r, _ := f.margins(); r != 0.5 {
		t.Error("the corner layout was not restored")
	}
	if len(f.draws()) != 1 || !f.tv.has("restore") {
		t.Error("the guide or its picture was not restored")
	}
}
