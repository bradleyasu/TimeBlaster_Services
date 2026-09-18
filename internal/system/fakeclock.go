package system

import (
	"sort"
	"sync"
	"time"
)

// FakeClock is a deterministic Clock for tests. Time only advances when Advance or
// SetNow is called, so scheduling tests never sleep and never flake.
//
// It lives in the non-test file set because several packages' tests need it.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeTimer
}

// NewFakeClock returns a FakeClock positioned at now.
func NewFakeClock(now time.Time) *FakeClock { return &FakeClock{now: now} }

// Now reports the fake current time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since reports the fake elapsed time since t.
func (f *FakeClock) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Sleep advances the clock rather than blocking, which keeps tests instant.
func (f *FakeClock) Sleep(d time.Duration) { f.Advance(d) }

// NewTimer registers a timer that fires when the fake clock passes the deadline.
func (f *FakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{
		clock:    f,
		deadline: f.now.Add(d),
		ch:       make(chan time.Time, 1),
		active:   true,
	}
	// A non-positive duration fires immediately, matching time.NewTimer.
	if d <= 0 {
		t.active = false
		t.ch <- f.now
		return t
	}
	f.waiters = append(f.waiters, t)
	return t
}

// Advance moves the clock forward by d, firing every timer whose deadline passes.
func (f *FakeClock) Advance(d time.Duration) { f.SetNow(f.Now().Add(d)) }

// SetNow jumps the clock to t, firing any timers in between. Jumping backwards is
// allowed so that DST and NTP-step behaviour can be exercised.
func (f *FakeClock) SetNow(t time.Time) {
	f.mu.Lock()
	f.now = t
	var fired []*fakeTimer
	remaining := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.deadline.After(t) {
			fired = append(fired, w)
		} else {
			remaining = append(remaining, w)
		}
	}
	f.waiters = remaining
	f.mu.Unlock()

	sort.Slice(fired, func(i, j int) bool { return fired[i].deadline.Before(fired[j].deadline) })
	for _, w := range fired {
		w.fire(t)
	}
}

// Waiters reports how many timers are currently pending. Tests use it to
// synchronise with code that is about to block on a timer.
func (f *FakeClock) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

type fakeTimer struct {
	clock    *FakeClock
	mu       sync.Mutex
	deadline time.Time
	ch       chan time.Time
	active   bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) fire(now time.Time) {
	t.mu.Lock()
	t.active = false
	t.mu.Unlock()
	select {
	case t.ch <- now:
	default: // a pending tick is already buffered; drop this one like time.Timer does
	}
}

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	was := t.active
	t.active = false
	t.mu.Unlock()

	t.clock.mu.Lock()
	remaining := t.clock.waiters[:0]
	for _, w := range t.clock.waiters {
		if w != t {
			remaining = append(remaining, w)
		}
	}
	t.clock.waiters = remaining
	t.clock.mu.Unlock()
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	was := t.Stop()
	t.clock.mu.Lock()
	t.mu.Lock()
	t.deadline = t.clock.now.Add(d)
	t.active = true
	t.mu.Unlock()
	t.clock.waiters = append(t.clock.waiters, t)
	t.clock.mu.Unlock()
	return was
}

var _ Clock = (*FakeClock)(nil)
