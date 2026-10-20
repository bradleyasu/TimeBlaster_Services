// Package screen decides what the television is doing.
//
// The television has modes -- ErsatzTV channels, ambience videos, and whatever
// comes next -- and exactly one is active at a time. The mode knob picks the
// mode; the channel knob picks a channel within it. Every decision about what is
// on screen goes through one Director, applied one at a time, because loading
// takes long enough that two deciders acting concurrently will leave the wrong
// thing showing.
//
// A mode is anything that implements Mode. Adding one means writing that, and
// listing its ID in the mode knob's configuration; nothing here changes.
package screen

import (
	"context"
	"errors"
	"sync"

	"github.com/bradsheets/timeblaster/internal/mpv"
)

// Mode is one thing the television can show.
//
// All modes share one mpv. The contract that keeps them from fighting over it:
// a mode touches the player only while it is active, and Deactivate does not
// return until any load it had in flight has finished. Gate implements both
// halves; a mode that routes every load through its Gate gets them for free.
type Mode interface {
	// ID is the stable name used in configuration and the API: "tv".
	ID() string
	// Title is the name shown to people: "TV".
	Title() string
	// ChannelCount is how many positions the channel knob has in this mode.
	// It may change; the mode says so through its own observer.
	ChannelCount() int
	// Activate lets the mode use the player. It is a state change only: the
	// Director follows it with SelectBand, which is what puts something on
	// screen.
	Activate()
	// Deactivate stops the mode touching the player, waiting out any load
	// already in flight. It forgets what was on screen, so a later activation
	// loads afresh rather than assuming its old picture is still there.
	Deactivate()
	// SelectBand shows the channel in a zero-based channel-knob band. -1 means
	// the knob has no channel to offer, or has not reported yet.
	SelectBand(ctx context.Context, band int) error
	// RestorePlayback puts the mode's picture back after mpv restarts.
	RestorePlayback(ctx context.Context)
	// HandlePlayerEvent receives mpv's events while the mode is active.
	HandlePlayerEvent(ev mpv.Event)
	// Showing describes what is on screen, for the API.
	Showing() Showing
}

// Showing is what a mode has on screen.
type Showing struct {
	// Number is the channel's number within its mode; empty when none.
	Number string `json:"number,omitempty"`
	Name   string `json:"name,omitempty"`
}

// ErrInactive means a mode was asked to put something on screen while another
// mode owns it. Callers that act on behalf of a person should say which mode
// the television is in; background work should simply stop.
var ErrInactive = errors.New("screen: that mode is not active")

// Gate admits a mode's player work only while the mode is active.
//
// Holding the gate across a load is what makes Close airtight: a reload that
// passed the check a moment before the mode was switched away finishes before
// Close returns, so it can never land on top of the next mode's picture.
type Gate struct {
	mu   sync.Mutex
	open bool
}

// Open admits work.
func (g *Gate) Open() {
	g.mu.Lock()
	g.open = true
	g.mu.Unlock()
}

// Close refuses further work, after waiting for any in progress.
func (g *Gate) Close() {
	g.mu.Lock()
	g.open = false
	g.mu.Unlock()
}

// IsOpen reports whether work is admitted. It is a snapshot: anything that
// acts on the answer must do so inside Do.
func (g *Gate) IsOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// Do runs fn if the gate is open, holding it shut against Close until fn
// returns. It returns ErrInactive without running fn when closed.
func (g *Gate) Do(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return ErrInactive
	}
	return fn()
}
