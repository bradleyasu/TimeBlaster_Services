package mpv

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

// Overlay canvas dimensions. mpv scales ASS overlays from these coordinates to
// whatever the television is actually running, so a font size configured here
// looks the same on a 720p set and a 4K one.
const (
	OverlayResX = 1280
	OverlayResY = 720

	// OverlayID is the osd-overlay slot Timeblaster draws the channel banner in.
	// Using a fixed id means a new banner replaces the old one rather than
	// stacking, so spinning the channel knob never leaves ghosts on screen.
	OverlayID = 1
)

// ASS alignment codes (\an): the numeric keypad layout.
const (
	alignBottomLeft  = 1
	alignBottomRight = 3
	alignCenter      = 5
	alignTopLeft     = 7
	alignTopRight    = 9
)

// OverlayRenderer draws the channel-change banner over the video using mpv's ASS
// overlay support.
//
// Using mpv's own overlay rather than a second graphical stack matters on a Pi 5
// running Raspberry Pi OS Lite: there is no compositor to draw on top of DRM/KMS
// output, and starting one for a two-second banner would be absurd.
type OverlayRenderer struct {
	cfg   config.Overlay
	ctrl  Controller
	clock system.Clock
	log   *slog.Logger

	mu sync.Mutex
	// generation invalidates a pending hide when a new banner is shown, so the
	// second banner is not cleared early by the first one's timer.
	generation uint64
	visible    bool
	// awaiting marks a channel banner that is waiting for the picture to
	// arrive. Until it does, only the safety-net timer can clear it.
	awaiting bool
	// curNumber and curName are the channel the current banner is for, kept so
	// the animation loop and PlaybackStarted can redraw without the caller
	// having to hand them back.
	curNumber string
	curName   string
}

// NewOverlayRenderer builds a renderer.
func NewOverlayRenderer(cfg config.Overlay, ctrl Controller, clock system.Clock, log *slog.Logger) *OverlayRenderer {
	return &OverlayRenderer{cfg: cfg, ctrl: ctrl, clock: clock, log: log}
}

// ShowChannel displays the banner for a channel and holds it until the picture
// actually arrives.
//
// Hiding on a fixed timer looked wrong on real hardware: tuning takes over a
// second warm and much longer when ErsatzTV has to cold-start the channel, so
// the banner came and went while the screen still showed the previous content,
// and the viewer was left with no idea anything was happening. A television
// keeps the channel number up until the picture does, so this does too.
//
// PlaybackStarted ends the hold. MaxHold is only a safety net, for a stream
// that never starts at all.
//
// It never restarts playback and never blocks the caller: the hide is handled
// on a background goroutine so a channel change returns immediately.
func (o *OverlayRenderer) ShowChannel(ctx context.Context, number, name string) error {
	if !o.cfg.Enabled {
		return nil
	}

	// Claim the slot before drawing, so a Hide racing with this call cannot be
	// undone by the draw that follows it.
	o.mu.Lock()
	o.generation++
	gen := o.generation
	o.awaiting = true
	o.curNumber, o.curName = number, name
	tuning := o.cfg.Tuning
	o.mu.Unlock()

	var err error
	if tuning {
		err = o.showRaw(ctx, o.ASSTuning(number, name, 0))
	} else {
		err = o.show(ctx, formatOverlayText(o.cfg.TextFormat, number, name))
	}
	if err != nil {
		return err
	}

	if tuning {
		go o.animate(gen)
	}
	go o.hideAfter(gen, o.maxHold())
	return nil
}

// PlaybackStarted tells the overlay the picture has arrived, so the banner can
// now do its normal turn on screen and go.
//
// Called for every playback start, including the standby image, which is
// harmless: it only does anything when a channel banner is waiting.
func (o *OverlayRenderer) PlaybackStarted() {
	o.mu.Lock()
	if !o.awaiting {
		o.mu.Unlock()
		return
	}
	o.awaiting = false
	// Invalidate the safety-net timer and stop the animation, then schedule the
	// real one.
	o.generation++
	gen := o.generation
	number, name := o.curNumber, o.curName
	tuning := o.cfg.Tuning
	o.mu.Unlock()

	// Started here rather than inside the goroutine: the redraw below happens
	// first and costs an IPC round trip, which would otherwise delay the timer.
	timer := o.clock.NewTimer(o.cfg.Duration.Duration)
	go func() {
		// There is a picture to put a banner over now, so the full-screen
		// tuning card gives way to the ordinary corner banner for its turn.
		if tuning {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := o.show(ctx, formatOverlayText(o.cfg.TextFormat, number, name))
			cancel()
			if err != nil && o.log != nil {
				o.log.Debug("could not draw the channel banner", "error", err)
			}
		}
		o.hideOnTimer(gen, timer)
	}()
}

// Awaiting reports whether a banner is holding for the picture.
func (o *OverlayRenderer) Awaiting() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.awaiting
}

func (o *OverlayRenderer) maxHold() time.Duration {
	if d := o.cfg.MaxHold.Duration; d > 0 {
		return d
	}
	return 20 * time.Second
}

// tuningDots is the width of the animated indicator, in glyphs.
const tuningDots = 3

// animate advances the tuning card's dots until the picture arrives, the banner
// is superseded, or mpv stops accepting overlay commands.
func (o *OverlayRenderer) animate(gen uint64) {
	interval := o.tuningInterval()
	for phase := 1; ; phase++ {
		timer := o.clock.NewTimer(interval)
		<-timer.C()
		timer.Stop()

		o.mu.Lock()
		stale := o.generation != gen || !o.awaiting
		number, name := o.curNumber, o.curName
		o.mu.Unlock()
		if stale {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := o.showRaw(ctx, o.ASSTuning(number, name, phase))
		cancel()
		if err != nil {
			// mpv is gone or wedged. The safety-net hide still owns the slot,
			// so there is nothing to clean up here.
			if o.log != nil {
				o.log.Debug("could not animate the tuning card", "error", err)
			}
			return
		}
	}
}

// ASSTuning renders the full-screen card shown while a channel comes up.
//
// It is deliberately not the corner banner. A cold tune leaves the outgoing
// channel's last frame frozen on screen for several seconds — or black, if
// there was no frame to hold — so a small mark in a corner is easy to miss and
// easy to mistake for a stuck picture. The channel number is centred and large,
// with an animated row of dots underneath saying the appliance is working
// rather than wedged.
//
// The dot row is always tuningDots glyphs wide and animates by alpha alone.
// Appending real dots would change the line's width every frame and make the
// centred text jitter horizontally.
//
// Exported as a pure function for the same reason as ASS: the escaping and
// override-tag rules belong in a test, not in squinting at a television.
func (o *OverlayRenderer) ASSTuning(number, name string, phase int) string {
	var b strings.Builder

	// A filled rectangle covering the whole canvas, on its own event line so the
	// card's text draws over it. mpv leaves the outgoing channel's last frame on
	// screen until the new one decodes, and a frozen picture reads as a crash;
	// painting it out reads as having left the channel, which is what actually
	// happened.
	if bg := o.cfg.TuningBackground; bg != "" {
		b.WriteString("{\\an7\\pos(0,0)\\bord0\\shad0\\alpha&H00&")
		b.WriteString("\\c" + assColorOr(bg, "&H000000&"))
		b.WriteString("\\p1}")
		b.WriteString(fmt.Sprintf("m 0 0 l %d 0 l %d %d l 0 %d",
			OverlayResX, OverlayResX, OverlayResY, OverlayResY))
		b.WriteString("{\\p0}\n")
	}

	b.WriteString("{\\an5")
	b.WriteString(fmt.Sprintf("\\pos(%d,%d)", OverlayResX/2, OverlayResY/2))
	b.WriteString(fmt.Sprintf("\\fs%d", o.cfg.FontSize))
	b.WriteString("\\b1")
	b.WriteString("\\c" + assColor(o.cfg.Color))
	b.WriteString("\\3c&H000000&")
	b.WriteString(fmt.Sprintf("\\bord%d", o.cfg.Outline))
	b.WriteString("\\shad0")
	b.WriteString("}")
	b.WriteString(escapeASS(formatOverlayText(o.cfg.TextFormat, number, name)))

	b.WriteString("\\N")
	b.WriteString(fmt.Sprintf("{\\fs%d\\b0}", o.tuningFontSize()))
	b.WriteString(escapeASS(o.tuningText()))

	if phase < 0 {
		phase = 0
	}
	lit := phase % (tuningDots + 1)
	for i := 0; i < tuningDots; i++ {
		if i < lit {
			b.WriteString("{\\alpha&H00&}")
		} else {
			b.WriteString("{\\alpha&HFF&}")
		}
		b.WriteString(".")
	}
	return b.String()
}

func (o *OverlayRenderer) tuningText() string {
	if o.cfg.TuningText != "" {
		return o.cfg.TuningText
	}
	return "TUNING"
}

func (o *OverlayRenderer) tuningFontSize() int {
	if o.cfg.TuningFontSize > 0 {
		return o.cfg.TuningFontSize
	}
	if n := o.cfg.FontSize / 2; n > 0 {
		return n
	}
	return 1
}

func (o *OverlayRenderer) tuningInterval() time.Duration {
	if d := o.cfg.TuningInterval.Duration; d > 0 {
		return d
	}
	return 400 * time.Millisecond
}

// ShowText displays arbitrary text with the configured styling. It backs status
// messages such as entering Wi-Fi setup mode.
func (o *OverlayRenderer) ShowText(ctx context.Context, text string, d time.Duration) error {
	if !o.cfg.Enabled {
		return nil
	}
	if err := o.show(ctx, text); err != nil {
		return err
	}
	o.mu.Lock()
	o.generation++
	gen := o.generation
	o.awaiting = false
	o.mu.Unlock()

	if d > 0 {
		go o.hideAfter(gen, d)
	}
	return nil
}

func (o *OverlayRenderer) show(ctx context.Context, text string) error {
	return o.showRaw(ctx, o.ASS(text))
}

// showRaw puts already-rendered ASS into the overlay slot. The tuning card
// builds a multi-line event with per-glyph overrides, so it cannot go through
// ASS, which renders a single styled string.
func (o *OverlayRenderer) showRaw(ctx context.Context, data string) error {
	// osd-overlay: id, format, data, res_x, res_y, z, hidden, compute_bounds.
	_, err := o.ctrl.Command(ctx, "osd-overlay", OverlayID, "ass-events", data,
		OverlayResX, OverlayResY, 0, false, false)
	if err != nil {
		return fmt.Errorf("mpv: showing overlay: %w", err)
	}
	o.mu.Lock()
	o.visible = true
	o.mu.Unlock()
	return nil
}

// Hide removes the banner immediately.
func (o *OverlayRenderer) Hide(ctx context.Context) error {
	o.mu.Lock()
	o.generation++
	o.visible = false
	o.awaiting = false
	o.mu.Unlock()

	// An empty event list is how mpv clears an overlay slot.
	_, err := o.ctrl.Command(ctx, "osd-overlay", OverlayID, "none", "",
		OverlayResX, OverlayResY, 0, false, false)
	if err != nil {
		return fmt.Errorf("mpv: hiding overlay: %w", err)
	}
	return nil
}

// Visible reports whether the banner is currently up.
func (o *OverlayRenderer) Visible() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.visible
}

func (o *OverlayRenderer) hideAfter(gen uint64, d time.Duration) {
	o.hideOnTimer(gen, o.clock.NewTimer(d))
}

// hideOnTimer takes an already-running timer, so a caller with work to do first
// can start the clock before doing it. Drawing costs an IPC round trip, and a
// hide timer that only starts after the draw is one a fast channel change can
// race past.
func (o *OverlayRenderer) hideOnTimer(gen uint64, timer system.Timer) {
	defer timer.Stop()
	<-timer.C()

	o.mu.Lock()
	superseded := o.generation != gen
	o.mu.Unlock()
	if superseded {
		return // a newer banner owns the slot now
	}

	// A fresh context: the caller's request context is long gone by now.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.Hide(ctx); err != nil && o.log != nil {
		o.log.Debug("could not hide the channel overlay", "error", err)
	}
}

// ASS renders text as an ASS event line with the configured styling.
//
// Split out as an exported pure function because the escaping and colour-order
// rules are exactly the sort of thing that should be pinned by a test rather than
// verified by squinting at a television.
func (o *OverlayRenderer) ASS(text string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("{\\an%d", alignCode(o.cfg.Position)))
	b.WriteString(fmt.Sprintf("\\pos(%d,%d)", o.posX(), o.posY()))
	b.WriteString(fmt.Sprintf("\\fs%d", o.cfg.FontSize))
	b.WriteString("\\b1")                        // bold: readable across a room
	b.WriteString("\\c" + assColor(o.cfg.Color)) // primary fill
	b.WriteString("\\3c&H000000&")               // black border, so green stays legible over bright video
	b.WriteString(fmt.Sprintf("\\bord%d", o.cfg.Outline))
	b.WriteString("\\shad0")
	b.WriteString("}")
	b.WriteString(escapeASS(text))
	return b.String()
}

func (o *OverlayRenderer) posX() int {
	switch o.cfg.Position {
	case "top-right", "bottom-right":
		return OverlayResX - o.cfg.MarginX
	case "center":
		return OverlayResX / 2
	default:
		return o.cfg.MarginX
	}
}

func (o *OverlayRenderer) posY() int {
	switch o.cfg.Position {
	case "bottom-left", "bottom-right":
		return OverlayResY - o.cfg.MarginY
	case "center":
		return OverlayResY / 2
	default:
		return o.cfg.MarginY
	}
}

func alignCode(position string) int {
	switch position {
	case "top-right":
		return alignTopRight
	case "bottom-left":
		return alignBottomLeft
	case "bottom-right":
		return alignBottomRight
	case "center":
		return alignCenter
	default:
		return alignTopLeft
	}
}

// assColor converts #RRGGBB to ASS's &HBBGGRR& form. ASS stores colours in BGR
// order, which is the single most common way to end up with a blue banner when
// you asked for green.
func assColor(hex string) string {
	// Validation should have caught a bad colour; fall back to retro green
	// rather than drawing nothing.
	return assColorOr(hex, "&H33FF33&")
}

// assColorOr renders hex as an ASS &HBBGGRR& colour, using fallback if it will
// not parse. The background takes black rather than the banner's green: a
// colour typo should not turn the tuning card into a full-screen green wall.
func assColorOr(hex, fallback string) string {
	r, g, b, err := config.ParseHexColor(hex)
	if err != nil {
		return fallback
	}
	return fmt.Sprintf("&H%02X%02X%02X&", b, g, r)
}

// escapeASS neutralises the characters that would otherwise be interpreted as ASS
// markup. Channel names come from ErsatzTV, so they are not under our control.
func escapeASS(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		"{", `\{`,
		"}", `\}`,
		"\n", `\N`,
		"\r", "",
	)
	return r.Replace(s)
}

// formatOverlayText applies the configured format string. The format receives the
// channel number first and the channel name second, so "CH %s" and
// "CH %s — %s" both work.
func formatOverlayText(format, number, name string) string {
	switch strings.Count(format, "%s") {
	case 0:
		return format
	case 1:
		return fmt.Sprintf(format, number)
	default:
		return fmt.Sprintf(format, number, name)
	}
}
