// Package system holds the thin seams between Timeblaster's business logic and the
// operating system: the clock, process execution, and hardware device discovery.
//
// Nothing above this package calls time.Now, os/exec or reads /proc directly. That
// is what lets alarm scheduling, volume mapping and device selection be tested
// deterministically on a development machine.
package system

import "time"

// Timer abstracts a single-shot timer so that scheduling logic can be driven by a
// fake clock in tests.
type Timer interface {
	// C returns the channel on which the deadline is delivered.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether it did so before the
	// timer fired, matching time.Timer.Stop.
	Stop() bool
	// Reset reschedules the timer. The timer must be stopped or expired first, as
	// with time.Timer.Reset.
	Reset(d time.Duration) bool
}

// Clock is the only source of time in the application.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	NewTimer(d time.Duration) Timer
	// Sleep blocks for d. Used sparingly; prefer NewTimer with a context.
	Sleep(d time.Duration)
}

// RealClock is the production Clock backed by the standard library.
type RealClock struct{}

// Now reports the current local time.
func (RealClock) Now() time.Time { return time.Now() }

// Since reports the elapsed time since t.
func (RealClock) Since(t time.Time) time.Duration { return time.Since(t) }

// Sleep pauses the calling goroutine for d.
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// NewTimer creates a single-shot timer.
func (RealClock) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time        { return r.t.C }
func (r *realTimer) Stop() bool                 { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

// Compile-time assertion that the production clock satisfies the interface.
var _ Clock = RealClock{}
