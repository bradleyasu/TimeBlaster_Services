package input

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/system"
)

// Potentiometer indices. These are the physical knobs, left to right.
const (
	// PotChannel selects the ErsatzTV channel (absolute position).
	PotChannel = 0
	// PotAlarmVolume sets the USB alarm speaker volume. The knob is authoritative.
	PotAlarmVolume = 1
	// PotReservedA is unassigned. TODO: assign a function to pot 2.
	PotReservedA = 2
	// PotReservedB is unassigned. TODO: assign a function to pot 3.
	PotReservedB = 3

	// PotCount is how many potentiometers the Router tracks.
	PotCount = 4
)

// Events emitted by the Router. They are deliberately about *meaning* rather than
// hardware: the Router is the last place that knows about ADC counts.

// ChannelSelect asks for a channel band to be selected.
type ChannelSelect struct {
	// Band is the zero-based channel index, or -1 when no channels exist.
	Band int
	// Position is the filtered knob position, 0.0..1.0, for diagnostics and UI.
	Position float64
}

// VolumeChange asks for the alarm speaker volume to be set.
type VolumeChange struct {
	Percent int
	// Physical is true when the change came from the knob rather than software.
	Physical bool
}

// ReservedPotChange reports movement of a pot with no assigned function yet.
// TODO: remove once pots 2 and 3 are assigned.
type ReservedPotChange struct {
	Index    int
	Position float64
}

// ButtonEvent reports a completed button gesture.
type ButtonEvent struct {
	Name   string
	Action ButtonAction
	// Held is how long the button was down when the action was determined.
	Held time.Duration
}

// Handler receives routed input events. Implementations must not block for long:
// the Router calls them from its own goroutine, and a slow handler delays other
// input. Anything slow (starting an access point, loading a stream) should be
// dispatched asynchronously by the handler.
type Handler interface {
	OnChannelSelect(ChannelSelect)
	OnVolumeChange(VolumeChange)
	OnButton(ButtonEvent)
	OnReservedPot(ReservedPotChange)
}

// Config tunes the Router's signal processing. It mirrors the [input] section of
// the configuration file.
type Config struct {
	Calibration       Calibration
	FilterAlpha       float64
	FilterSnap        float64
	ChannelHysteresis float64
	VolumeDeadband    int
	HoldDuration      time.Duration
	MinPressDuration  time.Duration
	// PollInterval is how often held buttons are checked against the hold
	// threshold. The ticker only runs while a button is actually down.
	PollInterval time.Duration
}

// DefaultConfig returns tuning values that work with 10 kΩ linear pots on an
// ESP32-S3 ADC.
func DefaultConfig() Config {
	return Config{
		Calibration:       DefaultCalibration(),
		FilterAlpha:       0.35,
		FilterSnap:        0.08,
		ChannelHysteresis: 0.25,
		VolumeDeadband:    2,
		HoldDuration:      5 * time.Second,
		MinPressDuration:  30 * time.Millisecond,
		PollInterval:      100 * time.Millisecond,
	}
}

// Router converts raw Nano reports into application events.
//
// It owns no hardware and performs no I/O; hardware.Link feeds it and it calls the
// Handler. That makes the whole "knob moved, therefore change channel" path
// testable by calling PotReport and ButtonEdge directly.
type Router struct {
	cfg     Config
	clock   system.Clock
	log     *slog.Logger
	handler Handler

	mu      sync.Mutex
	filters [PotCount]*Filter
	rawLast [PotCount]int
	bands   *HysteresisBands
	volume  *Deadband
	buttons map[string]*HoldTracker

	// pollWake is signalled when a button goes down so the poll loop can start
	// ticking; it is a buffered channel so signalling never blocks.
	pollWake chan struct{}
}

// NewRouter builds a Router. handler may be nil, in which case events are dropped
// (useful when the daemon is starting up and subsystems are not yet wired).
func NewRouter(cfg Config, clock system.Clock, log *slog.Logger, handler Handler) *Router {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	r := &Router{
		cfg:      cfg,
		clock:    clock,
		log:      log,
		handler:  handler,
		bands:    NewHysteresisBands(0, cfg.ChannelHysteresis),
		volume:   NewDeadband(cfg.VolumeDeadband),
		buttons:  map[string]*HoldTracker{},
		pollWake: make(chan struct{}, 1),
	}
	for i := range r.filters {
		r.filters[i] = NewFilter(cfg.FilterAlpha, cfg.FilterSnap)
	}
	return r
}

// SetHandler installs the event handler.
func (r *Router) SetHandler(h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handler = h
}

// SetChannelCount updates how many channels the channel knob maps across. It is
// called whenever the ErsatzTV channel list is refreshed, and it re-evaluates the
// knob's current position immediately so that adding a channel takes effect
// without the user having to touch anything.
//
// It returns the resulting selection rather than calling the handler, so the
// caller makes exactly one decision about what should be on screen. Calling the
// handler here as well gave two sources racing to own the television, and the
// slower one won: a knob that had already chosen channel 1 was overwritten by
// the standby image three milliseconds later.
//
// A nil return means the knob has never reported a position, so it cannot
// choose anything and the caller decides.
func (r *Router) SetChannelCount(n int) *ChannelSelect {
	r.mu.Lock()
	r.bands.SetCount(n, 0)
	primed := r.filters[PotChannel].Primed()
	pos := r.filters[PotChannel].Value()
	var ev *ChannelSelect
	// With no channels there is nothing to be primed for: the answer is "none"
	// either way, and saying so is what clears the screen.
	if primed || n == 0 {
		band, _ := r.bands.Update(pos)
		ev = &ChannelSelect{Band: band, Position: pos}
	}
	r.mu.Unlock()

	r.log.Info("channel count updated", "channels", n, "knob_reported", primed)
	return ev
}

// ChannelCount reports the number of bands the channel knob is divided into.
func (r *Router) ChannelCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bands.Count
}

// Position returns the filtered position of a potentiometer, and whether it has
// ever been reported.
func (r *Router) Position(index int) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= PotCount {
		return 0, false
	}
	return r.filters[index].Value(), r.filters[index].Primed()
}

// RawValue returns the most recent raw ADC reading for a potentiometer.
func (r *Router) RawValue(index int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= PotCount {
		return 0
	}
	return r.rawLast[index]
}

// PotReport feeds a raw potentiometer reading from the Nano.
func (r *Router) PotReport(index, raw int) {
	if index < 0 || index >= PotCount {
		r.log.Debug("ignoring report for unknown potentiometer", "index", index)
		return
	}

	r.mu.Lock()
	r.rawLast[index] = raw
	norm := r.cfg.Calibration.Normalize(raw)
	pos := r.filters[index].Update(norm)
	handler := r.handler

	var (
		chEv  *ChannelSelect
		volEv *VolumeChange
		resEv *ReservedPotChange
	)
	switch index {
	case PotChannel:
		if band, changed := r.bands.Update(pos); changed {
			chEv = &ChannelSelect{Band: band, Position: pos}
		}
	case PotAlarmVolume:
		if pct, changed := r.volume.Update(pos); changed {
			volEv = &VolumeChange{Percent: pct, Physical: true}
		}
	default:
		// TODO: pots 2 and 3 have no assigned function. They are still filtered and
		// surfaced so a future feature can be added without touching this path.
		resEv = &ReservedPotChange{Index: index, Position: pos}
	}
	r.mu.Unlock()

	// Raw readings are firehose-level detail, so they are debug-only.
	r.log.Debug("pot report", "index", index, "raw", raw, "position", pos)

	if handler == nil {
		return
	}
	switch {
	case chEv != nil:
		r.log.Info("channel knob selected band", "band", chEv.Band, "position", roundTo(chEv.Position, 3))
		handler.OnChannelSelect(*chEv)
	case volEv != nil:
		r.log.Info("alarm volume knob moved", "percent", volEv.Percent)
		handler.OnVolumeChange(*volEv)
	case resEv != nil:
		handler.OnReservedPot(*resEv)
	}
}

// ButtonEdge feeds a debounced button edge from the Nano.
func (r *Router) ButtonEdge(name string, down bool) {
	now := r.clock.Now()

	r.mu.Lock()
	tr, ok := r.buttons[name]
	if !ok {
		tr = &HoldTracker{HoldDuration: r.cfg.HoldDuration, MinPressDuration: r.cfg.MinPressDuration}
		r.buttons[name] = tr
	}
	action := tr.Edge(down, now)
	held := tr.HeldFor(now)
	handler := r.handler
	anyDown := r.anyDownLocked()
	r.mu.Unlock()

	r.log.Info("button edge", "button", name, "state", edgeName(down))

	if anyDown {
		r.wakePoll()
	}
	if action != ActionNone && handler != nil {
		r.log.Info("button action", "button", name, "action", action.String(), "held", held.Round(time.Millisecond))
		handler.OnButton(ButtonEvent{Name: name, Action: action, Held: held})
	}
}

// Run drives hold detection. It sleeps entirely while no button is held, so an
// idle Timeblaster costs nothing, and only ticks at PollInterval while a button is
// actually down.
func (r *Router) Run(ctx context.Context) error {
	for {
		// Idle: wait for a press or for shutdown.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.pollWake:
		}

		// Active: poll until every button is released.
		for r.anyDown() {
			t := r.clock.NewTimer(r.cfg.PollInterval)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C():
				r.pollHolds()
			}
		}
	}
}

// Reset clears all derived input state. Called when the Nano reconnects: the knobs
// may have been turned and the buttons pressed while the link was down, so every
// filter is unprimed and every hold forgotten, and the next report from the Nano
// re-establishes the truth.
func (r *Router) Reset() {
	r.mu.Lock()
	for i := range r.filters {
		r.filters[i].Reset()
		r.rawLast[i] = 0
	}
	r.bands.Reset()
	r.volume.Reset()
	for _, tr := range r.buttons {
		tr.Reset()
	}
	r.mu.Unlock()
	r.log.Info("input state reset after hardware reconnect")
}

func (r *Router) pollHolds() {
	now := r.clock.Now()

	r.mu.Lock()
	var fired []ButtonEvent
	for name, tr := range r.buttons {
		if tr.Tick(now) == ActionHold {
			fired = append(fired, ButtonEvent{Name: name, Action: ActionHold, Held: tr.HeldFor(now)})
		}
	}
	handler := r.handler
	r.mu.Unlock()

	for _, ev := range fired {
		r.log.Info("button hold threshold reached", "button", ev.Name, "held", ev.Held.Round(time.Millisecond))
		if handler != nil {
			handler.OnButton(ev)
		}
	}
}

func (r *Router) anyDown() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.anyDownLocked()
}

func (r *Router) anyDownLocked() bool {
	for _, tr := range r.buttons {
		if tr.Down() {
			return true
		}
	}
	return false
}

func (r *Router) wakePoll() {
	select {
	case r.pollWake <- struct{}{}:
	default:
	}
}

func edgeName(down bool) string {
	if down {
		return "down"
	}
	return "up"
}

func roundTo(v float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int(v*p+0.5)) / p
}
