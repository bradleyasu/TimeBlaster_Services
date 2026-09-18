package input

import "time"

// ButtonAction is the result of interpreting a button's press/release pattern.
type ButtonAction int

const (
	// ActionNone means nothing worth acting on happened.
	ActionNone ButtonAction = iota
	// ActionShortPress is a press released before the hold threshold.
	ActionShortPress
	// ActionHold is the hold threshold being reached while the button is still
	// down. It fires once per press, at the moment the threshold is crossed, so
	// the user gets feedback without having to let go first.
	ActionHold
)

// String makes log lines and test failures readable.
func (a ButtonAction) String() string {
	switch a {
	case ActionShortPress:
		return "short-press"
	case ActionHold:
		return "hold"
	default:
		return "none"
	}
}

// HoldTracker interprets debounced press/release edges from one button.
//
// It is a pure state machine: it never sleeps, allocates or starts goroutines, and
// every transition is driven by a caller-supplied timestamp. The caller polls Tick
// while the button is down (see Router, which only runs its poll ticker while at
// least one button is held, so an idle Timeblaster does no work at all).
//
// The 5-second Wi-Fi hold lives here rather than in firmware, which keeps the
// duration configurable and the behaviour testable without a Nano attached.
type HoldTracker struct {
	// HoldDuration is how long the button must stay down to count as a hold.
	HoldDuration time.Duration
	// MinPressDuration rejects implausibly short edges that survived the
	// firmware's debounce, for example from a noisy cable. Zero disables the check.
	MinPressDuration time.Duration

	down      bool
	pressedAt time.Time
	holdFired bool
}

// NewHoldTracker returns a tracker with the given hold threshold.
func NewHoldTracker(hold time.Duration) *HoldTracker {
	return &HoldTracker{HoldDuration: hold}
}

// Down reports whether the button is currently held.
func (h *HoldTracker) Down() bool { return h.down }

// HeldFor reports how long the button has been down at time now, or zero when it
// is not down.
func (h *HoldTracker) HeldFor(now time.Time) time.Duration {
	if !h.down {
		return 0
	}
	return now.Sub(h.pressedAt)
}

// Edge feeds a press (down=true) or release (down=false) at time now and returns
// the action it implies.
//
// A repeated edge in the same direction is ignored, which matters because a serial
// reconnect can replay or drop an edge; the tracker must not end up believing a
// button is held forever.
func (h *HoldTracker) Edge(down bool, now time.Time) ButtonAction {
	if down {
		if h.down {
			return ActionNone // duplicate press; keep the original timestamp
		}
		h.down, h.pressedAt, h.holdFired = true, now, false
		return ActionNone
	}

	if !h.down {
		return ActionNone // release without a matching press
	}
	held := now.Sub(h.pressedAt)
	h.down = false
	switch {
	case h.holdFired:
		// The hold already fired while the button was down; releasing it is not a
		// second event. This is what stops a 5-second Wi-Fi hold from also
		// registering as a short press.
		return ActionNone
	case h.MinPressDuration > 0 && held < h.MinPressDuration:
		return ActionNone
	case h.HoldDuration > 0 && held >= h.HoldDuration:
		// The caller was not polling (or polled coarsely) and we only learned of
		// the hold on release. Still honour it.
		return ActionHold
	default:
		return ActionShortPress
	}
}

// Tick checks whether a currently-held button has crossed the hold threshold. It
// returns ActionHold exactly once per press.
func (h *HoldTracker) Tick(now time.Time) ButtonAction {
	if !h.down || h.holdFired || h.HoldDuration <= 0 {
		return ActionNone
	}
	if now.Sub(h.pressedAt) >= h.HoldDuration {
		h.holdFired = true
		return ActionHold
	}
	return ActionNone
}

// Reset clears all state. Used when the Nano reconnects, because we cannot know
// whether a button was released while the link was down.
func (h *HoldTracker) Reset() {
	h.down, h.holdFired = false, false
	h.pressedAt = time.Time{}
}
