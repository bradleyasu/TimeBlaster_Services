package app

import (
	"context"
	"time"

	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

// announcingWiFi wraps the Wi-Fi manager so entering and leaving setup mode
// always show on the device itself, whichever path asked for it.
//
// The announcement used to live in the button-hold handler alone, so starting
// setup from the companion app left the seven-segment display showing the time
// and the Wi-Fi LED dark while the Pi silently dropped off the network -- the
// user's only feedback was that everything stopped working. Wrapping the
// manager puts the announcement at the one point both paths share, where they
// cannot drift apart again.
type announcingWiFi struct {
	wifi.Manager
	app *App
}

func (w announcingWiFi) EnterSetupMode(ctx context.Context) error {
	// Announce first. The network disappears during the call below, and saying
	// so beforehand is the entire point.
	w.app.announceWiFiSetup()
	if err := w.Manager.EnterSetupMode(ctx); err != nil {
		w.app.announceWiFiFailed()
		return err
	}
	go w.app.watchSetupMode()
	return nil
}

func (w announcingWiFi) ExitSetupMode(ctx context.Context) error {
	err := w.Manager.ExitSetupMode(ctx)
	w.app.announceWiFiNormal()
	return err
}

// announceWiFiSetup shows SETUP on the display, lights the Wi-Fi LED and puts a
// notice on the television.
func (a *App) announceWiFiSetup() {
	a.inSetup.Store(true)
	if a.nano != nil {
		a.showNanoText("SETUP")
		if err := a.nano.SetLED(protocol.LEDWiFi, true); err != nil {
			a.log.Debug("could not light the Wi-Fi LED", "error", err)
		}
	}
	if a.overlay != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := a.overlay.ShowText(ctx, "WI-FI SETUP", 10*time.Second); err != nil {
			a.log.Debug("could not show the setup overlay", "error", err)
		}
		cancel()
	}
}

// announceWiFiFailed shows a brief error rather than leaving SETUP up for a
// setup mode that never started.
func (a *App) announceWiFiFailed() {
	if a.nano == nil {
		return
	}
	a.flashNanoText("ERR", 5*time.Second)
	time.AfterFunc(5*time.Second, func() {
		if err := a.nano.SetLED(protocol.LEDWiFi, false); err != nil {
			a.log.Debug("could not put the Wi-Fi LED out", "error", err)
		}
	})
}

// announceWiFiConnected says, on the display, that setup worked -- while the
// phone that did the setup is still finding its way back to the same network
// and its page can tell nobody anything -- then returns to the clock.
func (a *App) announceWiFiConnected() {
	a.inSetup.Store(false)
	if a.nano == nil {
		return
	}
	if err := a.nano.SetLED(protocol.LEDWiFi, false); err != nil {
		a.log.Debug("could not put the Wi-Fi LED out", "error", err)
	}
	// Long enough for the word to scroll past twice.
	a.flashNanoText("Connected", 8*time.Second)
}

// announceWiFiNormal returns the display and the LED to their resting state.
func (a *App) announceWiFiNormal() {
	a.inSetup.Store(false)
	if a.nano == nil {
		return
	}
	a.showNanoClock()
	if err := a.nano.SetLED(protocol.LEDWiFi, false); err != nil {
		a.log.Debug("could not put the Wi-Fi LED out", "error", err)
	}
}

// InSetupMode reports whether the Timeblaster is in Wi-Fi setup mode, which is
// what the companion app's server needs to send a visitor to the setup page.
func (a *App) InSetupMode() bool { return a.inSetup.Load() }

// adoptSetupModeAtStartup picks up a setup mode already under way when the
// daemon starts -- it restarted while the helper had the access point up --
// so the display, the escape hatch and the setup-page hand-off all know.
func (a *App) adoptSetupModeAtStartup(ctx context.Context) error {
	timer := a.clock.NewTimer(remoteStartupDelay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C():
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	st, err := a.wifi.Status(sctx)
	cancel()
	if err == nil && st.Mode == wifi.ModeSetup {
		a.log.Info("Wi-Fi setup mode was already active at startup")
		a.announceWiFiSetup()
		go a.watchSetupMode()
	}
	<-ctx.Done()
	return ctx.Err()
}

// watchSetupMode polls the helper until setup mode ends, then clears the
// announcement.
//
// Setup almost always ends on the helper's side: the user picks a network in
// the captive portal, or the timeout expires. Neither tells the daemon, so
// without this the display would sit on SETUP and the LED stay lit long after
// the Pi was back on the network.
//
// The poll runs only while setup is believed to be active, so an idle
// Timeblaster still polls nothing.
func (a *App) watchSetupMode() {
	// Often enough that CONNECTED is on the display within moments of the
	// join: it is the only feedback anyone has while their phone is between
	// networks. The helper answers over a local socket, so this costs little,
	// and only runs during setup.
	const interval = 2 * time.Second
	// Outlast the helper's own timeout, then give up rather than poll forever.
	deadline := a.clock.Now().Add(a.cfg.WiFi.SetupTimeout.Duration + 2*time.Minute)

	for a.clock.Now().Before(deadline) {
		timer := a.clock.NewTimer(interval)
		<-timer.C()
		timer.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st, err := a.wifi.Status(ctx)
		cancel()
		if err != nil {
			a.log.Debug("could not read Wi-Fi status while watching setup mode", "error", err)
			continue
		}
		if st.Mode != wifi.ModeSetup {
			// Cancelled from the button: that path has already said so.
			if !a.inSetup.Load() {
				return
			}
			if st.Connected {
				a.log.Info("Wi-Fi setup mode ended; connected", "ssid", st.SSID)
				a.announceWiFiConnected()
				return
			}
			a.log.Info("Wi-Fi setup mode ended; returning the display to the clock",
				"mode", st.Mode, "ssid", st.SSID)
			a.announceWiFiNormal()
			return
		}
	}
	a.log.Warn("stopped watching Wi-Fi setup mode; clearing the display anyway")
	a.announceWiFiNormal()
}
