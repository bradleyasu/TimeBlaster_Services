package system

import (
	"testing"
	"time"
)

func TestFakeClockFiresTimersInOrder(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)

	early := c.NewTimer(1 * time.Minute)
	late := c.NewTimer(5 * time.Minute)

	if c.Waiters() != 2 {
		t.Fatalf("waiters: %d", c.Waiters())
	}
	select {
	case <-early.C():
		t.Fatal("timer fired before the clock advanced")
	default:
	}

	c.Advance(90 * time.Second)
	select {
	case got := <-early.C():
		if !got.Equal(start.Add(90 * time.Second)) {
			t.Errorf("fired at %v", got)
		}
	default:
		t.Fatal("early timer did not fire")
	}
	select {
	case <-late.C():
		t.Fatal("late timer fired too early")
	default:
	}

	c.Advance(10 * time.Minute)
	select {
	case <-late.C():
	default:
		t.Fatal("late timer did not fire")
	}
}

func TestFakeClockZeroDurationFiresImmediately(t *testing.T) {
	c := NewFakeClock(time.Now())
	select {
	case <-c.NewTimer(0).C():
	default:
		t.Fatal("zero-duration timer should fire immediately")
	}
}

func TestFakeClockStopPreventsFiring(t *testing.T) {
	c := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tm := c.NewTimer(time.Minute)
	if !tm.Stop() {
		t.Fatal("Stop should report true for a pending timer")
	}
	c.Advance(time.Hour)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if tm.Stop() {
		t.Error("second Stop should report false")
	}
}

func TestFakeClockResetReschedules(t *testing.T) {
	c := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tm := c.NewTimer(time.Minute)
	tm.Reset(10 * time.Minute)

	c.Advance(2 * time.Minute)
	select {
	case <-tm.C():
		t.Fatal("timer fired at the original deadline after Reset")
	default:
	}
	c.Advance(9 * time.Minute)
	select {
	case <-tm.C():
	default:
		t.Fatal("timer did not fire at the new deadline")
	}
}

func TestFakeClockSetNowBackwards(t *testing.T) {
	// A backwards NTP step must not fire pending timers.
	start := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	tm := c.NewTimer(time.Hour)
	c.SetNow(start.Add(-2 * time.Hour))
	select {
	case <-tm.C():
		t.Fatal("timer fired on a backwards step")
	default:
	}
	if !c.Now().Equal(start.Add(-2 * time.Hour)) {
		t.Errorf("now: %v", c.Now())
	}
}
