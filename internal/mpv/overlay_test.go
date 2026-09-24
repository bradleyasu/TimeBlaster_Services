package mpv

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

func overlayConfig() config.Overlay { return config.Default().Overlay }

func TestOverlayASSUsesRetroGreenInBGROrder(t *testing.T) {
	cfg := overlayConfig()
	cfg.Color = "#33FF33"
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())

	got := r.ASS("CH 3")

	// ASS stores colours as &HBBGGRR&, so #33FF33 must render as &H33FF33& only
	// because red and blue happen to match here; check a colour where they differ.
	if !strings.Contains(got, `\c&H33FF33&`) {
		t.Errorf("colour missing: %s", got)
	}
	if !strings.HasSuffix(got, "CH 3") {
		t.Errorf("text missing: %s", got)
	}
	for _, want := range []string{`\an7`, `\fs96`, `\b1`, `\bord3`, `\3c&H000000&`, `\pos(64,48)`} {
		if !strings.Contains(got, want) {
			t.Errorf("ASS is missing %s: %s", want, got)
		}
	}
}

func TestOverlayASSColourChannelOrder(t *testing.T) {
	cfg := overlayConfig()
	cfg.Color = "#FF0000" // pure red must become &H0000FF&, not &HFF0000&
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())
	if got := r.ASS("X"); !strings.Contains(got, `\c&H0000FF&`) {
		t.Errorf("red rendered in the wrong channel order: %s", got)
	}
}

func TestOverlayASSFallsBackOnAnInvalidColour(t *testing.T) {
	cfg := overlayConfig()
	cfg.Color = "chartreuse"
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())
	if got := r.ASS("X"); !strings.Contains(got, `\c&H33FF33&`) {
		t.Errorf("expected the green fallback: %s", got)
	}
}

func TestOverlayPositions(t *testing.T) {
	tests := map[string]struct {
		align string
		pos   string
	}{
		"top-left":     {`\an7`, `\pos(64,48)`},
		"top-right":    {`\an9`, `\pos(1216,48)`},
		"bottom-left":  {`\an1`, `\pos(64,672)`},
		"bottom-right": {`\an3`, `\pos(1216,672)`},
		"center":       {`\an5`, `\pos(640,360)`},
	}
	for position, want := range tests {
		t.Run(position, func(t *testing.T) {
			cfg := overlayConfig()
			cfg.Position = position
			r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())
			got := r.ASS("CH 1")
			if !strings.Contains(got, want.align) || !strings.Contains(got, want.pos) {
				t.Errorf("got %s, want %s and %s", got, want.align, want.pos)
			}
		})
	}
}

func TestOverlayEscapesChannelNames(t *testing.T) {
	// Channel names come from ErsatzTV, so braces in a name must not be able to
	// inject ASS markup.
	r := NewOverlayRenderer(overlayConfig(), NewFake(), system.NewFakeClock(time.Now()), testLogger())
	got := r.ASS(`Movies {\c&HFF0000&} \ 24-7`)
	if strings.Contains(got, `{\c&HFF0000&}`) {
		t.Errorf("markup was not escaped: %s", got)
	}
	if !strings.Contains(got, `\{`) || !strings.Contains(got, `\}`) {
		t.Errorf("braces not escaped: %s", got)
	}
}

func TestFormatOverlayText(t *testing.T) {
	for _, tc := range []struct {
		format, number, name, want string
	}{
		{"CH %s", "3", "Movies", "CH 3"},
		{"CH %s — %s", "3", "Movies", "CH 3 — Movies"},
		{"NO CHANNEL", "3", "Movies", "NO CHANNEL"},
		{"%s", "12.1", "Sci-Fi", "12.1"},
	} {
		if got := formatOverlayText(tc.format, tc.number, tc.name); got != tc.want {
			t.Errorf("formatOverlayText(%q) = %q want %q", tc.format, got, tc.want)
		}
	}
}

func TestOverlayShowAndHideAfterThePicture(t *testing.T) {
	fake := NewFake()
	clk := system.NewFakeClock(time.Now())
	cfg := overlayConfig()
	cfg.Duration = config.Dur(2 * time.Second)
	// This test is about the hold-and-hide contract, and its arithmetic below
	// counts registered timers. The tuning card animates on a timer of its own,
	// which would throw that count off; the card has its own tests.
	cfg.Tuning = false
	r := NewOverlayRenderer(cfg, fake, clk, testLogger())

	if err := r.ShowChannel(context.Background(), "3", "Movies 24/7"); err != nil {
		t.Fatalf("ShowChannel: %v", err)
	}
	if !r.Visible() {
		t.Error("overlay should be visible")
	}

	shows := fake.CallsNamed("osd-overlay")
	if len(shows) != 1 {
		t.Fatalf("calls: %+v", shows)
	}
	if shows[0].Args[1] != OverlayID || shows[0].Args[2] != "ass-events" {
		t.Errorf("osd-overlay args: %+v", shows[0].Args)
	}
	if data, _ := shows[0].Args[3].(string); !strings.HasSuffix(data, "CH 3") {
		t.Errorf("overlay text: %q", data)
	}
	// The overlay must not restart playback.
	if len(fake.CallsNamed("loadfile")) != 0 {
		t.Error("showing an overlay must not touch playback")
	}

	// The banner holds until the picture arrives, then runs its normal course.
	waitForWaiters(t, clk, 1)
	r.PlaybackStarted()
	waitForWaiters(t, clk, 2)
	clk.Advance(3 * time.Second)

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if len(fake.CallsNamed("osd-overlay")) >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	calls := fake.CallsNamed("osd-overlay")
	if len(calls) < 2 {
		t.Fatalf("overlay was never hidden: %+v", calls)
	}
	if calls[len(calls)-1].Args[2] != "none" {
		t.Errorf("hide call: %+v", calls[len(calls)-1].Args)
	}
}

func TestOverlayRapidChannelChangesDoNotClearTheLatestBanner(t *testing.T) {
	// Spinning the channel knob must not let the first banner's timer wipe the
	// banner belonging to the channel you landed on.
	fake := NewFake()
	clk := system.NewFakeClock(time.Now())
	cfg := overlayConfig()
	// This test is about the hold-and-hide contract, and its arithmetic below
	// counts registered timers. The tuning card animates on a timer of its own,
	// which would throw that count off; the card has its own tests.
	cfg.Tuning = false
	cfg.Duration = config.Dur(2 * time.Second)
	r := NewOverlayRenderer(cfg, fake, clk, testLogger())

	for _, ch := range []string{"1", "2", "3"} {
		if err := r.ShowChannel(context.Background(), ch, "x"); err != nil {
			t.Fatal(err)
		}
	}
	// Three banners, so three safety-net timers.
	waitForWaiters(t, clk, 3)

	// The picture arrives for the one the user landed on.
	r.PlaybackStarted()
	waitForWaiters(t, clk, 4)
	clk.Advance(30 * time.Second)
	time.Sleep(50 * time.Millisecond)

	// Exactly one hide: the superseded banners bail out rather than clearing
	// the one that is actually on screen.
	if n := hides(fake); n != 1 {
		t.Errorf("expected exactly one hide, got %d", n)
	}
}

func TestOverlayDisabledIsANoOp(t *testing.T) {
	fake := NewFake()
	cfg := overlayConfig()
	cfg.Enabled = false
	r := NewOverlayRenderer(cfg, fake, system.NewFakeClock(time.Now()), testLogger())

	if err := r.ShowChannel(context.Background(), "3", "Movies"); err != nil {
		t.Fatalf("ShowChannel: %v", err)
	}
	if err := r.ShowText(context.Background(), "SETUP", time.Second); err != nil {
		t.Fatalf("ShowText: %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("a disabled overlay must issue no commands: %+v", fake.Calls())
	}
	if r.Visible() {
		t.Error("a disabled overlay is never visible")
	}
}

func TestOverlayReportsCommandFailures(t *testing.T) {
	fake := NewFake()
	fake.NotAlive = true
	r := NewOverlayRenderer(overlayConfig(), fake, system.NewFakeClock(time.Now()), testLogger())

	if err := r.ShowChannel(context.Background(), "3", "Movies"); err == nil {
		t.Error("expected an error when mpv is down")
	}
	if r.Visible() {
		t.Error("the overlay must not be marked visible after a failure")
	}
}

func TestOverlayHideIsImmediate(t *testing.T) {
	fake := NewFake()
	r := NewOverlayRenderer(overlayConfig(), fake, system.NewFakeClock(time.Now()), testLogger())
	if err := r.ShowChannel(context.Background(), "5", "News"); err != nil {
		t.Fatal(err)
	}
	if err := r.Hide(context.Background()); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	if r.Visible() {
		t.Error("overlay should be hidden")
	}
}

func TestOverlayShowTextWithoutDurationStaysUp(t *testing.T) {
	fake := NewFake()
	clk := system.NewFakeClock(time.Now())
	r := NewOverlayRenderer(overlayConfig(), fake, clk, testLogger())

	if err := r.ShowText(context.Background(), "SETUP MODE", 0); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)

	for _, c := range fake.CallsNamed("osd-overlay") {
		if c.Args[2] == "none" {
			t.Fatal("a persistent overlay was hidden")
		}
	}
	if !r.Visible() {
		t.Error("overlay should still be visible")
	}
}

func TestFakeControllerBehaviour(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if err := f.SetProperty(ctx, "volume", 55); err != nil {
		t.Fatal(err)
	}
	var vol int
	if err := f.GetProperty(ctx, "volume", &vol); err != nil {
		t.Fatalf("GetProperty: %v", err)
	}
	if vol != 55 {
		t.Errorf("volume: %d", vol)
	}
	if err := f.GetProperty(ctx, "missing", &vol); err == nil {
		t.Error("unknown property should fail")
	}

	f.Reset()
	if len(f.Calls()) != 0 {
		t.Error("Reset did not clear calls")
	}
	if _, ok := f.LastCall(); ok {
		t.Error("LastCall on an empty fake")
	}
}

func TestChannelBannerHoldsUntilThePictureArrives(t *testing.T) {
	// The behaviour this fixes, seen on the Pi: the banner expired on a fixed
	// timer while the screen still showed the previous content, so "CH 1" came
	// and went seconds before channel 1 appeared.
	fake := NewFake()
	clk := system.NewFakeClock(time.Now())
	cfg := overlayConfig()
	cfg.Duration = config.Dur(2 * time.Second)
	cfg.MaxHold = config.Dur(20 * time.Second)
	// This test is about the hold-and-hide contract, and its arithmetic below
	// counts registered timers. The tuning card animates on a timer of its own,
	// which would throw that count off; the card has its own tests.
	cfg.Tuning = false
	r := NewOverlayRenderer(cfg, fake, clk, testLogger())

	if err := r.ShowChannel(context.Background(), "1", "ErsatzTV"); err != nil {
		t.Fatal(err)
	}
	if !r.Awaiting() {
		t.Fatal("the banner should be holding for the picture")
	}

	// Well past the display duration, with no picture yet: it must still be up.
	waitForWaiters(t, clk, 1)
	clk.Advance(10 * time.Second)
	time.Sleep(30 * time.Millisecond)
	if hides(fake) != 0 {
		t.Fatalf("the banner was hidden before the picture arrived: %+v", fake.CallsNamed("osd-overlay"))
	}

	// The picture arrives; now the normal duration applies.
	r.PlaybackStarted()
	if r.Awaiting() {
		t.Error("the hold should be over")
	}
	// Two timers now: the superseded safety net and the real one. Waiting for
	// only one would return before the real one had registered.
	waitForWaiters(t, clk, 2)
	clk.Advance(3 * time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hides(fake) == 0 {
		time.Sleep(time.Millisecond)
	}
	if hides(fake) != 1 {
		t.Errorf("expected exactly one hide once the picture arrived, got %d", hides(fake))
	}
}

func TestChannelBannerGivesUpIfThePictureNeverArrives(t *testing.T) {
	// A stream that never starts must not pin the banner on screen forever.
	fake := NewFake()
	clk := system.NewFakeClock(time.Now())
	cfg := overlayConfig()
	cfg.MaxHold = config.Dur(20 * time.Second)
	r := NewOverlayRenderer(cfg, fake, clk, testLogger())

	if err := r.ShowChannel(context.Background(), "3", "Sci-Fi"); err != nil {
		t.Fatal(err)
	}
	waitForWaiters(t, clk, 1)
	clk.Advance(25 * time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hides(fake) == 0 {
		time.Sleep(time.Millisecond)
	}
	if hides(fake) != 1 {
		t.Errorf("the banner should have given up after max_hold, hides=%d", hides(fake))
	}
}

func TestPlaybackStartedIsHarmlessWithNoBanner(t *testing.T) {
	// It is called for every playback start, the standby image included.
	fake := NewFake()
	r := NewOverlayRenderer(overlayConfig(), fake, system.NewFakeClock(time.Now()), testLogger())
	r.PlaybackStarted()
	r.PlaybackStarted()
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("issued %d commands with no banner pending", n)
	}
}

func hides(f *Fake) int {
	n := 0
	for _, c := range f.CallsNamed("osd-overlay") {
		if c.Args[2] == "none" {
			n++
		}
	}
	return n
}

func waitForWaiters(t *testing.T, clk *system.FakeClock, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if clk.Waiters() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d pending timers, have %d", want, clk.Waiters())
}

func TestASSTuningDotsAnimateWithoutChangingWidth(t *testing.T) {
	r := NewOverlayRenderer(overlayConfig(), NewFake(), system.NewFakeClock(time.Now()), testLogger())

	seen := map[string]bool{}
	for phase := 0; phase < 8; phase++ {
		got := r.ASSTuning("2", "Music TV", phase)
		// The glyph count never changes, whatever the phase. Appending real
		// dots as it animated would change the line's width every frame and
		// make the centred text jitter from side to side.
		if n := strings.Count(got, "."); n != tuningDots {
			t.Errorf("phase %d drew %d dots, want %d: %q", phase, n, tuningDots, got)
		}
		seen[got] = true
	}
	if len(seen) != tuningDots+1 {
		t.Errorf("got %d distinct frames, want %d before it repeats", len(seen), tuningDots+1)
	}
}

func TestASSTuningIsCentredAndNamesTheChannel(t *testing.T) {
	cfg := overlayConfig()
	cfg.Position = "top-left" // the card ignores this: it owns the whole screen
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())

	got := r.ASSTuning("2", "Music TV", 0)
	for _, want := range []string{`\an5`, `\pos(640,360)`, "CH 2", "TUNING"} {
		if !strings.Contains(got, want) {
			t.Errorf("tuning card is missing %s: %s", want, got)
		}
	}
}

func TestASSTuningEscapesTheChannelName(t *testing.T) {
	// Channel names come from ErsatzTV, so the card must escape them just as
	// the banner does.
	cfg := overlayConfig()
	cfg.TextFormat = "CH %s - %s"
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())

	got := r.ASSTuning("2", `Music {\c&HFF0000&}`, 0)
	if strings.Contains(got, `{\c&HFF0000&}`) {
		t.Errorf("markup was not escaped: %s", got)
	}
}

func TestTuningCardGoesUpImmediatelyAndAnimates(t *testing.T) {
	// A cold channel change leaves the outgoing frame frozen on screen for
	// several seconds, so the card has to appear at once and then visibly keep
	// moving, or a nine-second wait reads as a wedged appliance.
	fake := NewFake()
	clk := system.NewFakeClock(time.Now())
	cfg := overlayConfig()
	cfg.TuningInterval = config.Dur(400 * time.Millisecond)
	r := NewOverlayRenderer(cfg, fake, clk, testLogger())

	if err := r.ShowChannel(context.Background(), "2", "Music TV"); err != nil {
		t.Fatal(err)
	}
	first, ok := fake.LastCall()
	if !ok {
		t.Fatal("nothing was drawn")
	}
	if data, _ := first.Args[3].(string); !strings.Contains(data, "TUNING") {
		t.Fatalf("the first draw should be the tuning card: %q", data)
	}

	// Two timers: the safety-net hold, and the animation.
	waitForWaiters(t, clk, 2)
	before := len(fake.CallsNamed("osd-overlay"))
	clk.Advance(cfg.TuningInterval.Duration)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(fake.CallsNamed("osd-overlay")) == before {
		time.Sleep(time.Millisecond)
	}
	if len(fake.CallsNamed("osd-overlay")) == before {
		t.Error("the tuning card never animated")
	}
}

func TestTuningCardGivesWayToTheBannerWhenThePictureArrives(t *testing.T) {
	fake := NewFake()
	clk := system.NewFakeClock(time.Now())
	cfg := overlayConfig()
	cfg.Duration = config.Dur(2 * time.Second)
	r := NewOverlayRenderer(cfg, fake, clk, testLogger())

	if err := r.ShowChannel(context.Background(), "2", "Music TV"); err != nil {
		t.Fatal(err)
	}
	r.PlaybackStarted()

	// The full-screen card is for the black; once there is a picture the
	// ordinary corner banner takes over for its normal turn.
	var data string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, ok := fake.LastCall(); ok {
			data, _ = c.Args[3].(string)
			// The banner's corner position: the card uses \pos(0,0) for its
			// background and \pos(640,360) for its text, never this.
			if strings.Contains(data, `\pos(64,48)`) {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(data, `\pos(64,48)`) || strings.Contains(data, "TUNING") {
		t.Errorf("the card should have given way to the corner banner, got %q", data)
	}
}

func TestASSTuningPaintsOverTheFrozenFrame(t *testing.T) {
	// mpv leaves the outgoing channel's last frame on screen until the new one
	// decodes. Letting it show through the card makes a channel change look
	// like the picture has crashed, so the card paints the canvas out first.
	cfg := overlayConfig()
	cfg.TuningBackground = "#000000"
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())

	lines := strings.Split(r.ASSTuning("2", "Music TV", 0), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected a background event then a text event, got %d: %q", len(lines), lines)
	}
	for _, want := range []string{
		`\p1`,                               // drawing mode
		`\c&H000000&`,                       // black fill
		`\alpha&H00&`,                       // fully opaque, or the frame shows through
		`\bord0`,                            // no outline around a full-screen box
		"m 0 0 l 1280 0 l 1280 720 l 0 720", // the whole canvas
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("background event is missing %s: %s", want, lines[0])
		}
	}
	// Order matters: an event drawn after the background sits on top of it.
	if !strings.Contains(lines[1], "TUNING") {
		t.Errorf("the card text must be the second event, got %q", lines[1])
	}
}

func TestASSTuningBackgroundCanBeTurnedOff(t *testing.T) {
	cfg := overlayConfig()
	cfg.TuningBackground = ""
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())

	got := r.ASSTuning("2", "Music TV", 0)
	if strings.Contains(got, `\p1`) {
		t.Errorf("no background colour was set, but one was drawn: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("expected a single event with no background: %q", got)
	}
}

func TestASSTuningBackgroundFallsBackToBlackNotGreen(t *testing.T) {
	// assColor's fallback is the banner's green, which would turn a colour typo
	// into a full-screen green wall.
	cfg := overlayConfig()
	cfg.TuningBackground = "chartreuse"
	r := NewOverlayRenderer(cfg, NewFake(), system.NewFakeClock(time.Now()), testLogger())

	bg := strings.Split(r.ASSTuning("2", "x", 0), "\n")[0]
	if !strings.Contains(bg, `\c&H000000&`) {
		t.Errorf("expected a black fallback, got %s", bg)
	}
}
