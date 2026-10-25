package screen

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/mpv"
)

// applyTimeout bounds one request, so a stream that will not start cannot wedge
// the queue and leave later requests unapplied.
const applyTimeout = 30 * time.Second

// Observer is told when the mode changes.
type Observer interface {
	ModeChanged(from, to Mode)
}

// Request asks for a mode and a channel-knob band within it.
type Request struct {
	Mode int
	Band int
}

// Director owns the television. It applies requests one at a time, in order,
// and routes the player's events to whichever mode is active.
type Director struct {
	modes []Mode
	log   *slog.Logger
	obs   Observer

	// reqs holds at most one pending request; a newer one replaces it, because
	// a knob being swept only ever means "show me where it ended up".
	reqs    chan Request
	restore chan struct{}

	mu     sync.RWMutex
	active int
}

// NewDirector builds a director over modes, in mode-knob order. The first is
// active from the start; every other is explicitly deactivated, so no mode can
// begin life believing it owns the screen.
func NewDirector(modes []Mode, log *slog.Logger, obs Observer) *Director {
	if len(modes) == 0 {
		panic("screen: a director needs at least one mode")
	}
	// Everything else first, the first mode last: modes may share machinery
	// -- the guide borrows the TV's playback -- and the one that ends up
	// active must be the last to touch it.
	for _, m := range modes[1:] {
		m.Deactivate()
	}
	modes[0].Activate()
	return &Director{
		modes:   modes,
		log:     log,
		obs:     obs,
		reqs:    make(chan Request, 1),
		restore: make(chan struct{}, 1),
	}
}

// Modes returns the modes in knob order.
func (d *Director) Modes() []Mode { return append([]Mode(nil), d.modes...) }

// Index returns the position of the mode with the given ID, or -1.
func (d *Director) Index(id string) int {
	for i, m := range d.modes {
		if m.ID() == id {
			return i
		}
	}
	return -1
}

// Active returns the mode that owns the screen.
func (d *Director) Active() Mode {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.modes[d.active]
}

// Request asks for a mode and band to be shown. It never blocks.
func (d *Director) Request(mode, band int) {
	select {
	case <-d.reqs:
	default:
	}
	select {
	case d.reqs <- Request{Mode: mode, Band: band}:
	default:
		// Another caller got there in between. Theirs is at least as fresh as
		// this one, so leaving it alone is correct.
	}
}

// RestorePlayback asks the active mode to put its picture back, after mpv has
// restarted or its audio output changed. It is queued behind any pending
// request rather than run on the caller's goroutine, so it cannot interleave
// with a mode switch.
func (d *Director) RestorePlayback() {
	select {
	case d.restore <- struct{}{}:
	default:
	}
}

// HandlePlayerEvent routes an mpv event to the active mode only. A mode that
// is not on screen has nothing to learn from the player.
func (d *Director) HandlePlayerEvent(ev mpv.Event) {
	d.Active().HandlePlayerEvent(ev)
}

// Run applies requests until the context is cancelled.
func (d *Director) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-d.reqs:
			d.apply(ctx, r)
		case <-d.restore:
			reqCtx, cancel := context.WithTimeout(ctx, applyTimeout)
			d.Active().RestorePlayback(reqCtx)
			cancel()
		}
	}
}

func (d *Director) apply(ctx context.Context, r Request) {
	if r.Mode < 0 || r.Mode >= len(d.modes) {
		d.log.Warn("ignoring a request for a mode that does not exist", "mode", r.Mode)
		return
	}

	d.mu.RLock()
	from := d.modes[d.active]
	d.mu.RUnlock()
	to := d.modes[r.Mode]

	if to != from {
		// Old first, and only then the new one: the old mode must be done
		// with the player before the new one starts using it.
		from.Deactivate()
		to.Activate()
		d.mu.Lock()
		d.active = r.Mode
		d.mu.Unlock()
		d.log.Info("television mode changed", "from", from.ID(), "to", to.ID())
		if d.obs != nil {
			d.obs.ModeChanged(from, to)
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	err := to.SelectBand(reqCtx, r.Band)
	switch {
	case err == nil, errors.Is(err, ErrInactive):
	case errors.Is(err, mpv.ErrNotConnected):
		// Normal at boot: the first request lands a second or two before mpv
		// has the display, and the mode's picture is restored the moment it
		// connects. A warning here would be a red line in every boot.
		d.log.Debug("player not ready yet; the picture is restored when it connects",
			"mode", to.ID(), "band", r.Band)
	default:
		d.log.Warn("could not put the requested channel on screen",
			"mode", to.ID(), "band", r.Band, "error", err)
	}
}
