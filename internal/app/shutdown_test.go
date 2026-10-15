package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/protocol"
)

// lastShutdownAnswer returns the most recent answer sent to the Nano.
func lastShutdownAnswer(h *harness) (accepted bool, reason string, ok bool) {
	cmds := h.nano.CommandsOfKind("shutdown-answer")
	if len(cmds) == 0 {
		return false, "", false
	}
	c := cmds[len(cmds)-1]
	return c.Flag, c.Text, true
}

func poweredOff(h *harness) bool {
	return slices.Contains(h.wifi.Calls(), "power_off")
}

// passAnswerDelay lets the goroutine that waits before powering off reach its
// timer, then moves the fake clock past it.
func passAnswerDelay(t *testing.T, h *harness) {
	t.Helper()
	waitFor(t, "the answer delay", func() bool { return h.clock.Waiters() > 0 })
	h.clock.Advance(shutdownAnswerDelay + time.Millisecond)
}

func ringAnAlarm(t *testing.T, h *harness) {
	t.Helper()
	a := alarm.New(6, 30)
	a.RepeatDays = alarm.EveryDay
	a.SoundID = "alarm1"
	saved, err := h.app.scheduler.Save(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.scheduler.Trigger(saved.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the alarm to ring", h.app.scheduler.IsRinging)
}

func TestShutdownRequestAnswersThenPowersOff(t *testing.T) {
	h := newHarness(t, nil)
	h.nano.Reset()

	h.app.ShutdownRequested()

	accepted, _, ok := lastShutdownAnswer(h)
	if !ok || !accepted {
		t.Fatalf("the Nano was not told the shutdown is going ahead (sent=%v, accepted=%v)", ok, accepted)
	}
	// The answer goes first, so it reaches the Nano before services stop.
	if poweredOff(h) {
		t.Fatal("powered off before the answer delay; the ACCEPTED could be lost")
	}

	passAnswerDelay(t, h)
	waitFor(t, "power off", func() bool { return poweredOff(h) })
}

func TestShutdownIsRefusedWhileAnAlarmRings(t *testing.T) {
	// The Nano refuses to count down during a ringing alarm, but it learns of the
	// alarm a moment late over the same link. This is the check that does not
	// depend on that.
	h := newHarness(t, nil)
	ringAnAlarm(t, h)
	h.nano.Reset()

	h.app.ShutdownRequested()

	accepted, reason, ok := lastShutdownAnswer(h)
	if !ok || accepted {
		t.Fatalf("a shutdown during a ringing alarm was not refused (sent=%v, accepted=%v)", ok, accepted)
	}
	if reason == "" {
		t.Error("the refusal carries no reason")
	}
	time.Sleep(50 * time.Millisecond)
	if poweredOff(h) {
		t.Fatal("powered off while an alarm was ringing")
	}
}

func TestShutdownTellsTheNanoWhenPowerOffFails(t *testing.T) {
	// The Nano shows the animation once accepted. If the power-off then fails it
	// would wait for a silence that never comes, so it has to be told.
	h := newHarness(t, nil)
	h.wifi.PowerOffErr = errors.New("systemctl: access denied")
	h.nano.Reset()

	h.app.ShutdownRequested()
	passAnswerDelay(t, h)

	waitFor(t, "the refusal after a failed power off", func() bool {
		accepted, _, ok := lastShutdownAnswer(h)
		return ok && !accepted
	})
	if n := len(h.nano.CommandsOfKind("shutdown-answer")); n != 2 {
		t.Errorf("want an acceptance then a refusal, got %d answers", n)
	}
}

func TestHoldingTheBigRedButtonNeverPowersOff(t *testing.T) {
	// The property the whole design rests on. The Pi only sees press and release
	// messages, and if a release were lost it would believe the button was still
	// held. So nothing the Pi derives from button messages may ever power the
	// Timeblaster off: only the Nano's explicit request does.
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.app.router.Run(ctx) }()

	// Pressed and, as far as the Pi can tell, never released.
	h.app.router.ButtonEdge(protocol.ButtonAlarmOff, true)
	for range 60 {
		h.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if poweredOff(h) {
		t.Fatal("a minute of the big red button held powered the Timeblaster off without a request from the Nano")
	}
	if n := len(h.nano.CommandsOfKind("shutdown-answer")); n != 0 {
		t.Errorf("answered a shutdown nobody requested (%d answers)", n)
	}
}

func TestWiFiHoldStillEntersSetupAndNeverPowersOff(t *testing.T) {
	// The Wi-Fi button's five-second hold must be untouched by the big red
	// button's shutdown: it still enters setup mode, and holding it for far longer
	// than ten seconds does nothing else.
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.app.router.Run(ctx) }()

	h.app.router.ButtonEdge(protocol.ButtonWiFi, true)
	for range 15 {
		h.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	h.app.router.ButtonEdge(protocol.ButtonWiFi, false)

	waitFor(t, "setup mode", func() bool { return slices.Contains(h.wifi.Calls(), "enter_setup") })
	if poweredOff(h) {
		t.Fatal("holding the Wi-Fi button powered the Timeblaster off")
	}
}
