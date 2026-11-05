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
