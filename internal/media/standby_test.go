package media

import (
	"context"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/mpv"
)

func TestStandbyRetryDelayBacksOffToAMinute(t *testing.T) {
	want := []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := standbyRetryDelay(i + 1); got != w {
			t.Errorf("attempt %d: %s, want %s", i+1, got, w)
		}
	}
}

// standbyFails reports the standby image ending with an error, as it does
// over and over with no display attached.
func standbyFails(f *fixture) {
	f.svc.HandlePlayerEvent(mpv.Event{Name: "end-file", Reason: "error", Error: "vo init failed"})
}

func TestAStandbyImageThatKeepsFailingIsNotReloadedInATightLoop(t *testing.T) {
	// The television off: every load fails at once. Restoring immediately each
	// time was 88 reloads a second.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	loads := func() int { return len(f.player.CallsNamed("loadfile")) }
	base := loads()

	standbyFails(f) // the first is restored at once
	waitUntil(t, "the first restore", func() bool { return loads() == base+1 })

	standbyFails(f) // the second waits
	time.Sleep(20 * time.Millisecond)
	if loads() != base+1 {
		t.Fatal("a repeated failure was restored without waiting")
	}
	f.clock.Advance(2 * time.Second)
	waitUntil(t, "the second restore", func() bool { return loads() == base+2 })

	// And it keeps growing: a burst of failures is not a burst of reloads.
	for range 20 {
		standbyFails(f)
	}
	time.Sleep(20 * time.Millisecond)
	if n := loads() - base; n > 2 {
		t.Errorf("%d reloads for 22 failures with no time passing", n)
	}
}

func TestAPictureResetsTheStandbyBackoff(t *testing.T) {
	// When the television comes back, the next problem gets a prompt retry,
	// not the minute the last outage built up to.
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.ShowNoChannel(ctx); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		standbyFails(f)
	}
	f.svc.HandlePlayerEvent(mpv.Event{Name: "playback-restart"})
	f.svc.mu.RLock()
	n := f.svc.standbyFailures
	f.svc.mu.RUnlock()
	if n != 0 {
		t.Errorf("a picture left %d failures counted", n)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
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
