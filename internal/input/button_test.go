package input

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)

func TestHoldTrackerShortPress(t *testing.T) {
	h := NewHoldTracker(5 * time.Second)
	if got := h.Edge(true, t0); got != ActionNone {
		t.Fatalf("press: %v", got)
	}
	if !h.Down() {
		t.Error("button should be down")
	}
	if got := h.Edge(false, t0.Add(200*time.Millisecond)); got != ActionShortPress {
		t.Fatalf("release: %v", got)
	}
	if h.Down() {
		t.Error("button should be up")
	}
}

func TestHoldTrackerFiresAtThresholdWhileStillHeld(t *testing.T) {
	h := NewHoldTracker(5 * time.Second)
	h.Edge(true, t0)

	if got := h.Tick(t0.Add(4999 * time.Millisecond)); got != ActionNone {
		t.Fatalf("tick before threshold: %v", got)
	}
	if got := h.Tick(t0.Add(5 * time.Second)); got != ActionHold {
		t.Fatalf("tick at threshold: %v", got)
	}
	// It must fire exactly once, however long the user keeps holding.
	for _, d := range []time.Duration{6 * time.Second, 10 * time.Second, time.Minute} {
		if got := h.Tick(t0.Add(d)); got != ActionNone {
			t.Errorf("tick at %v fired again: %v", d, got)
		}
	}
	// Releasing after a hold is not also a short press.
	if got := h.Edge(false, t0.Add(8*time.Second)); got != ActionNone {
		t.Errorf("release after hold: %v", got)
	}
}

func TestHoldTrackerHoldDetectedOnReleaseIfNotPolled(t *testing.T) {
	// If the poll loop was starved (a busy Pi during an FFmpeg transcode, say) the
	// hold must still be honoured when the release arrives.
	h := NewHoldTracker(5 * time.Second)
	h.Edge(true, t0)
	if got := h.Edge(false, t0.Add(7*time.Second)); got != ActionHold {
		t.Fatalf("got %v", got)
	}
}

func TestHoldTrackerIgnoresDuplicateEdges(t *testing.T) {
	h := NewHoldTracker(5 * time.Second)
	h.Edge(true, t0)
	// A replayed press after a serial reconnect must not restart the hold timer.
	h.Edge(true, t0.Add(4*time.Second))
	if got := h.Tick(t0.Add(5 * time.Second)); got != ActionHold {
		t.Fatalf("duplicate press restarted the hold timer: %v", got)
	}
	// A release with no matching press is ignored.
	h.Edge(false, t0.Add(6*time.Second))
	if got := h.Edge(false, t0.Add(7*time.Second)); got != ActionNone {
		t.Errorf("orphan release: %v", got)
	}
}

func TestHoldTrackerRejectsImplausiblyShortPresses(t *testing.T) {
	h := &HoldTracker{HoldDuration: 5 * time.Second, MinPressDuration: 30 * time.Millisecond}
	h.Edge(true, t0)
	if got := h.Edge(false, t0.Add(5*time.Millisecond)); got != ActionNone {
		t.Fatalf("a 5 ms blip should be rejected, got %v", got)
	}
	h.Edge(true, t0.Add(time.Second))
	if got := h.Edge(false, t0.Add(time.Second+50*time.Millisecond)); got != ActionShortPress {
		t.Fatalf("a 50 ms press should count, got %v", got)
	}
}

func TestHoldTrackerHeldFor(t *testing.T) {
	h := NewHoldTracker(5 * time.Second)
	if got := h.HeldFor(t0); got != 0 {
		t.Errorf("not held: %v", got)
	}
	h.Edge(true, t0)
	if got := h.HeldFor(t0.Add(3 * time.Second)); got != 3*time.Second {
		t.Errorf("held for: %v", got)
	}
}

func TestHoldTrackerReset(t *testing.T) {
	h := NewHoldTracker(5 * time.Second)
	h.Edge(true, t0)
	h.Reset()
	if h.Down() {
		t.Fatal("reset should clear the down state")
	}
	// After a reconnect, a stale hold must not fire.
	if got := h.Tick(t0.Add(time.Minute)); got != ActionNone {
		t.Errorf("stale hold fired: %v", got)
	}
}

func TestHoldTrackerZeroDurationDisablesHolds(t *testing.T) {
	h := &HoldTracker{HoldDuration: 0}
	h.Edge(true, t0)
	if got := h.Tick(t0.Add(time.Hour)); got != ActionNone {
		t.Errorf("got %v", got)
	}
	if got := h.Edge(false, t0.Add(time.Hour)); got != ActionShortPress {
		t.Errorf("got %v", got)
	}
}

func TestButtonActionString(t *testing.T) {
	for action, want := range map[ButtonAction]string{
		ActionNone: "none", ActionShortPress: "short-press", ActionHold: "hold",
	} {
		if got := action.String(); got != want {
			t.Errorf("%d.String() = %q want %q", action, got, want)
		}
	}
}
