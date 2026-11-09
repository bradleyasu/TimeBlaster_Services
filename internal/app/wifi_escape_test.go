package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

func holdWiFi(h *harness) {
	h.app.OnButton(input.ButtonEvent{Name: protocol.ButtonWiFi, Action: input.ActionHold, Held: 5 * time.Second})
}

func TestHoldingTheWiFiButtonDuringSetupCancelsIt(t *testing.T) {
	// The physical way back: no waiting out the timeout, never a reason to open
	// the case for the Ethernet port.
	h := newHarness(t, nil)
	holdWiFi(h)
	waitFor(t, "setup mode", func() bool { return slices.Contains(h.wifi.Calls(), "enter_setup") })
	if !h.app.InSetupMode() {
		t.Fatal("the app does not know it is in setup mode")
	}

	holdWiFi(h)
	waitFor(t, "setup cancelled", func() bool { return slices.Contains(h.wifi.Calls(), "exit_setup") })
	if h.app.InSetupMode() {
		t.Error("still believed to be in setup mode after cancelling it")
	}
	if n := countOf(h.wifi.Calls(), "enter_setup"); n != 1 {
		t.Errorf("the second hold entered setup again (%d entries) instead of cancelling it", n)
	}
}

func TestSetupModeAlreadyActiveAtStartupIsAdopted(t *testing.T) {
	// The daemon restarted while the access point was up. Without adopting it,
	// the escape hatch would enter setup again instead of cancelling it.
	h := newHarness(t, nil)
	if err := h.wifi.EnterSetupMode(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.app.adoptSetupModeAtStartup(ctx) }()
	waitFor(t, "the startup check", func() bool { return h.clock.Waiters() > 0 })
	h.clock.Advance(remoteStartupDelay)
	waitFor(t, "adoption", h.app.InSetupMode)
}

func countOf(xs []string, s string) int {
	n := 0
	for _, x := range xs {
		if x == s {
			n++
		}
	}
	return n
}

// helperEndsSetup plays the helper finishing setup by itself -- a successful
// join -- which the daemon only learns by watching.
func helperEndsSetup(t *testing.T, h *harness) {
	t.Helper()
	if err := h.wifi.ExitSetupMode(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTheDisplaySaysConnectedWhenSetupSucceeds(t *testing.T) {
	// The phone that did the setup is between networks and its page can tell
	// nobody anything. The display can.
	h := newHarness(t, nil)
	holdWiFi(h)
	waitFor(t, "setup mode", h.app.InSetupMode)
	h.nano.Reset()

	helperEndsSetup(t, h)
	waitFor(t, "CONNECTED", func() bool {
		h.clock.Advance(time.Second)
		return displayed(h, "Connected")
	})
	if h.app.InSetupMode() {
		t.Error("still believed to be in setup mode")
	}
}

func TestCancellingSetupDoesNotClaimItConnected(t *testing.T) {
	// The button's way back says bAcK. Following it with Connected would
	// suggest the setup had worked.
	h := newHarness(t, nil)
	holdWiFi(h)
	waitFor(t, "setup mode", h.app.InSetupMode)
	holdWiFi(h) // cancel
	waitFor(t, "cancelled", func() bool { return !h.app.InSetupMode() })
	for range 10 {
		h.clock.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	if displayed(h, "Connected") {
		t.Error("a cancelled setup claimed to have connected")
	}
	if !displayed(h, "bAcK") {
		t.Error("cancelling did not say bAcK")
	}
}

func TestTheWiFiIndicatorFollowsTheConnection(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.app.runWiFiIndicator(ctx) }()
	waitFor(t, "online", h.app.wifiOnline.Load)

	// Setup mode is off the home network: the symbol says so at once.
	holdWiFi(h)
	waitFor(t, "offline in setup", func() bool { return !h.app.wifiOnline.Load() })

	// And Connected brings it back at once, without waiting for a check.
	helperEndsSetup(t, h)
	waitFor(t, "back online", func() bool {
		h.clock.Advance(time.Second)
		return h.app.wifiOnline.Load()
	})
}

func tapWiFi(h *harness) {
	h.app.OnButton(input.ButtonEvent{Name: protocol.ButtonWiFi, Action: input.ActionShortPress})
}

func restarted(h *harness) bool { return slices.Contains(h.wifi.Calls(), "restart_services") }

// passRestartDelay lets the rEStArt message be read, on the fake clock.
func passRestartDelay(t *testing.T, h *harness) {
	t.Helper()
	waitFor(t, "the restart", func() bool {
		h.clock.Advance(250 * time.Millisecond)
		return restarted(h)
	})
}

func TestDoubleTappingTheWiFiButtonRestartsTheServices(t *testing.T) {
	h := newHarness(t, nil)
	tapWiFi(h)
	h.clock.Advance(300 * time.Millisecond)
	tapWiFi(h)
	waitFor(t, "rEStArt on the display", func() bool { return displayed(h, "rEStArt") })
	passRestartDelay(t, h)
}

func TestASingleTapOrTwoSlowTapsDoNothing(t *testing.T) {
	h := newHarness(t, nil)
	tapWiFi(h)
	h.clock.Advance(2 * time.Second)
	tapWiFi(h) // too late to pair with the first
	for range 10 {
		h.clock.Advance(250 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	if restarted(h) {
		t.Error("two taps two seconds apart restarted the services")
	}
}

func TestADoubleTapIsRefusedWhenARestartWouldDoHarm(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, h *harness){
		// While it rings, a tap snoozes instead; once snoozed, the alarm is
		// still waiting and a restart would lose it.
		"an alarm is snoozed": func(t *testing.T, h *harness) {
			ringAnAlarm(t, h)
			if err := h.app.scheduler.Snooze(); err != nil {
				t.Fatal(err)
			}
		},
		"Wi-Fi setup is active": func(t *testing.T, h *harness) {
			holdWiFi(h)
			waitFor(t, "setup", h.app.InSetupMode)
		},
		"a firmware update is running": func(t *testing.T, h *harness) {
			h.app.fw.mu.Lock()
			h.app.fw.updating = true
			h.app.fw.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			setup(t, h)
			tapWiFi(h)
			tapWiFi(h)
			waitFor(t, "no on the display", func() bool { return displayed(h, "no") })
			for range 10 {
				h.clock.Advance(250 * time.Millisecond)
				time.Sleep(2 * time.Millisecond)
			}
			if restarted(h) {
				t.Errorf("restarted while %s", name)
			}
		})
	}
}

func TestTheHelperGoingDownMidRestartIsNotAnError(t *testing.T) {
	// The helper queues the restart, then systemd stops it -- sometimes before
	// its reply gets out. That dropped connection is the restart working.
	h := newHarness(t, nil)
	h.wifi.RestartErr = fmt.Errorf("%w: reading response: EOF", wifi.ErrHelperUnavailable)
	tapWiFi(h)
	tapWiFi(h)
	passRestartDelay(t, h)
	time.Sleep(20 * time.Millisecond)
	if displayed(h, "Err") {
		t.Error("a restart that went through was shown as an error")
	}
}

func TestARestartTheHelperRefusesIsShown(t *testing.T) {
	h := newHarness(t, nil)
	h.wifi.RestartErr = errors.New("wifi: restarting the services: Access denied")
	tapWiFi(h)
	tapWiFi(h)
	passRestartDelay(t, h)
	waitFor(t, "Err on the display", func() bool { return displayed(h, "Err") })
}

func alarmState(h *harness) string {
	if a := h.app.scheduler.Active(); a != nil {
		return string(a.State)
	}
	return ""
}

func TestTappingTheWiFiButtonSnoozesARingingAlarm(t *testing.T) {
	h := newHarness(t, nil)
	ringAnAlarm(t, h)
	tapWiFi(h)
	waitFor(t, "snoozed", func() bool { return alarmState(h) == "snoozed" })
	waitFor(t, "SnooZE on the display", func() bool { return displayed(h, "SnooZE") })
}

func TestASleepySecondTapAfterSnoozingDoesNotRestart(t *testing.T) {
	// Tap to snooze, tap again a moment later: that must not count as a
	// double tap, and nothing may restart while the alarm waits to ring again.
	h := newHarness(t, nil)
	ringAnAlarm(t, h)
	tapWiFi(h)
	waitFor(t, "snoozed", func() bool { return alarmState(h) == "snoozed" })
	h.clock.Advance(200 * time.Millisecond)
	tapWiFi(h)
	tapWiFi(h) // and a deliberate double tap, while snoozed
	for range 10 {
		h.clock.Advance(250 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	if restarted(h) {
		t.Error("restarted with a snoozed alarm waiting")
	}
	if alarmState(h) != "snoozed" {
		t.Errorf("the snoozed alarm is now %q", alarmState(h))
	}
}

func TestTappingTheWiFiButtonWithNoAlarmStillDoesNothing(t *testing.T) {
	h := newHarness(t, nil)
	tapWiFi(h)
	time.Sleep(20 * time.Millisecond)
	if displayed(h, "SnooZE") || restarted(h) {
		t.Error("a lone tap with no alarm did something")
	}
}
