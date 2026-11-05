package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/protocol"
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
