package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

func TestNanoChannelText(t *testing.T) {
	// Four cells, and the firmware binds a '.' to the character before it
	// instead of spending a cell on it, so "Ch.02" fits exactly.
	for _, tc := range []struct{ number, want string }{
		{"1", "Ch.01"},
		{"2", "Ch.02"},
		{"12", "Ch.12"},
		{"99", "Ch.99"},
		{"0", "Ch.00"},
		{" 7 ", "Ch.07"},
		{"100", "100"},   // too wide for the banner; shown bare
		{"12.1", "12.1"}, // sub-channels are not integers
		{"", ""},
	} {
		if got := nanoChannelText(tc.number); got != tc.want {
			t.Errorf("nanoChannelText(%q) = %q, want %q", tc.number, got, tc.want)
		}
	}
}

// waitForDisplay blocks until the fake Nano has recorded n commands of kind, so
// a test never races the goroutine that reverts the banner.
func waitForDisplay(t *testing.T, nano *hardware.FakeNano, kind string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(nano.CommandsOfKind(kind)) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %q command(s), got %d",
		n, kind, len(nano.CommandsOfKind(kind)))
}

func waitForWaiters(t *testing.T, h *harness, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.clock.Waiters() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d clock waiter(s), got %d", n, h.clock.Waiters())
}

func TestChannelChangeFlashesTheChannelThenReturnsToTheClock(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.General.ChannelBanner = config.Dur(2 * time.Second)
	})
	h.nano.Reset()

	h.app.ChannelChanged(&ersatztv.Channel{ID: 2, Number: "2", Name: "Cartoons"})

	waitForDisplay(t, h.nano, "display-text", 1)
	if got := h.nano.CommandsOfKind("display-text")[0].Text; got != "Ch.02" {
		t.Errorf("display text: %q, want %q", got, "Ch.02")
	}
	if n := len(h.nano.CommandsOfKind("display-clock")); n != 0 {
		t.Fatalf("returned to the clock before the banner expired (%d times)", n)
	}

	// The banner is on a timer, so it must be up until that timer fires.
	waitForWaiters(t, h, 1)
	h.clock.Advance(2 * time.Second)
	waitForDisplay(t, h.nano, "display-clock", 1)
}

func TestAnExpiringChannelBannerDoesNotWipeALaterMessage(t *testing.T) {
	// The Wi-Fi button can be held while a channel banner is still on screen.
	// When the banner's timer fires it must not clear "SETUP".
	h := newHarness(t, func(c *config.Config) {
		c.General.ChannelBanner = config.Dur(2 * time.Second)
	})
	h.nano.Reset()

	h.app.ChannelChanged(&ersatztv.Channel{ID: 2, Number: "2", Name: "Cartoons"})
	waitForDisplay(t, h.nano, "display-text", 1)
	waitForWaiters(t, h, 1)

	// Something else claims the display before the banner expires.
	h.app.showNanoText("SETUP")
	waitForDisplay(t, h.nano, "display-text", 2)

	h.clock.Advance(5 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if n := len(h.nano.CommandsOfKind("display-clock")); n != 0 {
		t.Errorf("the expiring banner wiped a later message: %d clock reverts", n)
	}
	last, _ := h.nano.LastOfKind("display-text")
	if last.Text != "SETUP" {
		t.Errorf("display should still read SETUP, got %q", last.Text)
	}
}

func TestChannelBannerCanBeTurnedOff(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.General.ChannelBanner = config.Dur(0)
	})
	h.nano.Reset()

	h.app.ChannelChanged(&ersatztv.Channel{ID: 2, Number: "2", Name: "Cartoons"})
	time.Sleep(50 * time.Millisecond)

	if n := len(h.nano.CommandsOfKind("display-text")); n != 0 {
		t.Errorf("banner is disabled but %d display-text commands were sent", n)
	}
}

func ledState(t *testing.T, h *harness, name string) (on bool, seen bool) {
	t.Helper()
	for _, c := range h.nano.CommandsOfKind("led") {
		if c.Text == name {
			on, seen = c.Flag, true
		}
	}
	return on, seen
}

func TestEnteringSetupFromTheCompanionAppAnnouncesItOnTheDevice(t *testing.T) {
	// The companion app's button reaches the Wi-Fi manager directly, with no
	// access to the display. Entering setup that way used to leave the clock
	// showing the time and the Wi-Fi LED dark, while the Pi quietly dropped off
	// the network -- so the only feedback the user got was everything breaking.
	h := newHarness(t, nil)
	h.nano.Reset()

	if err := h.app.wifi.EnterSetupMode(context.Background()); err != nil {
		t.Fatalf("EnterSetupMode: %v", err)
	}

	last, ok := h.nano.LastOfKind("display-text")
	if !ok || last.Text != "SETUP" {
		t.Errorf("display should read SETUP, got %q (present=%v)", last.Text, ok)
	}
	if on, seen := ledState(t, h, protocol.LEDWiFi); !seen || !on {
		t.Errorf("the Wi-Fi LED should be lit (seen=%v on=%v)", seen, on)
	}
}

func TestLeavingSetupClearsTheAnnouncement(t *testing.T) {
	// Nothing used to put the display back: setup ends on the helper's side, so
	// SETUP stayed up and the LED stayed lit after the Pi was back online.
	h := newHarness(t, nil)
	if err := h.app.wifi.EnterSetupMode(context.Background()); err != nil {
		t.Fatalf("EnterSetupMode: %v", err)
	}
	h.nano.Reset()

	if err := h.app.wifi.ExitSetupMode(context.Background()); err != nil {
		t.Fatalf("ExitSetupMode: %v", err)
	}

	if n := len(h.nano.CommandsOfKind("display-clock")); n == 0 {
		t.Error("the display was not returned to the clock")
	}
	if on, seen := ledState(t, h, protocol.LEDWiFi); !seen || on {
		t.Errorf("the Wi-Fi LED should be out (seen=%v on=%v)", seen, on)
	}
}

func TestSetupModeEndingOnTheHelperSideClearsTheDisplay(t *testing.T) {
	// The usual way setup ends: the user picks a network in the captive portal,
	// or the timeout expires. Neither goes through the daemon, so it has to
	// notice for itself or the display sits on SETUP indefinitely.
	h := newHarness(t, nil)
	h.wifi.SetStatus(wifi.Status{Mode: wifi.ModeSetup})
	if err := h.app.wifi.EnterSetupMode(context.Background()); err != nil {
		t.Fatalf("EnterSetupMode: %v", err)
	}
	h.nano.Reset()

	// The helper finishes setup without telling anyone.
	h.wifi.SetStatus(wifi.Status{Mode: wifi.ModeNormal, Connected: true, SSID: "the_wifi"})

	// Nudge the clock in a loop rather than once: the watcher re-registers its
	// timer after every poll, and a single Advance can land in the gap between
	// the two and be lost.
	waitForWaiters(t, h, 1) // the watcher's poll timer
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(h.nano.CommandsOfKind("display-clock")) == 0 {
		h.clock.Advance(10 * time.Second)
		time.Sleep(20 * time.Millisecond)
	}
	waitForDisplay(t, h.nano, "display-clock", 1)
	if on, seen := ledState(t, h, protocol.LEDWiFi); !seen || on {
		t.Errorf("the Wi-Fi LED should be out (seen=%v on=%v)", seen, on)
	}
}

func TestChangingTheClockFormatReachesTheNano(t *testing.T) {
	// The setting used to change only the companion app: the seven-segment
	// display formats the time itself from a unix timestamp, so nothing about
	// the preference ever reached it and the hardware stayed in whatever mode
	// it had booted in.
	h := newHarness(t, nil)
	h.nano.Reset()

	cur := h.app.Settings()
	cur.Clock24h = !cur.Clock24h
	if _, err := h.app.UpdateSettings(context.Background(), cur); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	cmds := h.nano.CommandsOfKind("clock-24h")
	if len(cmds) != 1 {
		t.Fatalf("expected one clock-24h command, got %d", len(cmds))
	}
	if cmds[0].Flag != cur.Clock24h {
		t.Errorf("sent clock_24h=%v, want %v", cmds[0].Flag, cur.Clock24h)
	}

	// Saving the same value again is not a change and must not re-send.
	h.nano.Reset()
	if _, err := h.app.UpdateSettings(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if n := len(h.nano.CommandsOfKind("clock-24h")); n != 0 {
		t.Errorf("an unchanged setting re-sent %d commands", n)
	}
}

func TestTurningTheKnobShowsTheVolumeThenReturnsToTheClock(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.General.VolumeBanner = config.Dur(1500 * time.Millisecond)
	})
	h.nano.Reset()

	h.app.OnVolumeChange(input.VolumeChange{Percent: 44, Physical: true})

	waitForDisplay(t, h.nano, "display-text", 1)
	if got := h.nano.CommandsOfKind("display-text")[0].Text; got != "V 44" {
		t.Errorf("display text: %q, want %q", got, "V 44")
	}
	if n := len(h.nano.CommandsOfKind("display-clock")); n != 0 {
		t.Fatal("returned to the clock before the banner expired")
	}

	waitForWaiters(t, h, 1)
	h.clock.Advance(1500 * time.Millisecond)
	waitForDisplay(t, h.nano, "display-clock", 1)
}

func TestAVolumeChangeFromTheAppDoesNotTouchTheDisplay(t *testing.T) {
	// Nobody is standing in front of the device when the change came from a
	// phone, so flashing the clock at them serves no one.
	h := newHarness(t, nil)
	h.nano.Reset()

	h.app.OnVolumeChange(input.VolumeChange{Percent: 44, Physical: false})
	time.Sleep(50 * time.Millisecond)

	if n := len(h.nano.CommandsOfKind("display-text")); n != 0 {
		t.Errorf("a software volume change wrote to the display %d time(s)", n)
	}
}

func TestNanoVolumeTextAlwaysFitsFourCells(t *testing.T) {
	// Four cells is all there is. "V" plus a right-aligned number covers every
	// value from 0 to 100 exactly, which is why there are no special cases.
	for _, tc := range []struct {
		percent int
		want    string
	}{
		{0, "V  0"},
		{5, "V  5"},
		{44, "V 44"},
		{100, "V100"},
		{-10, "V  0"}, // clamped rather than rendered as "V -10"
		{150, "V100"},
	} {
		got := nanoVolumeText(tc.percent)
		if got != tc.want {
			t.Errorf("nanoVolumeText(%d) = %q, want %q", tc.percent, got, tc.want)
		}
		if len(got) != 4 {
			t.Errorf("nanoVolumeText(%d) = %q, which is %d cells, not 4", tc.percent, got, len(got))
		}
	}
}

func TestTVAudioOutputSetting(t *testing.T) {
	// mpv's "auto" follows ALSA's default, which is card 0, and card numbers
	// are assigned in enumeration order -- so plugging in a USB speaker made it
	// card 0 and the television's sound started coming out of it. The device is
	// therefore always named, never numbered.
	h := newHarness(t, nil)
	ctx := context.Background()

	if got := h.app.Settings().TVAudioOutput; got != config.TVAudioHDMI {
		t.Errorf("default output = %q, want %q", got, config.TVAudioHDMI)
	}

	cur := h.app.Settings()
	cur.TVAudioOutput = config.TVAudioSpeaker
	saved, err := h.app.UpdateSettings(ctx, cur)
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if saved.TVAudioOutput != config.TVAudioSpeaker {
		t.Errorf("saved %q", saved.TVAudioOutput)
	}

	// mpv must be pointed at the SPEAKER's card specifically. Checking only for
	// "CARD=" was useless: falling through to HDMI also produces a CARD= string,
	// so the assertion held whether the speaker branch ran or not.
	speakerCard := h.app.audio.CardID()
	if speakerCard == "" {
		t.Fatal("the harness has no resolved speaker, so this proves nothing")
	}
	last, ok := lastSetProperty(h.player, "audio-device")
	if !ok {
		t.Fatal("mpv was never told which audio device to use")
	}
	if !strings.Contains(last, "CARD="+speakerCard) {
		t.Errorf("expected the speaker card %q, got %q", speakerCard, last)
	}
	if strings.Contains(last, "vc4hdmi") {
		t.Errorf("selecting the speaker still routed to HDMI: %q", last)
	}

	// And back to HDMI.
	cur.TVAudioOutput = config.TVAudioHDMI
	if _, err := h.app.UpdateSettings(ctx, cur); err != nil {
		t.Fatal(err)
	}
	last, _ = lastSetProperty(h.player, "audio-device")
	if !strings.Contains(last, "vc4hdmi") {
		t.Errorf("expected an HDMI card, got %q", last)
	}

	// An unknown value is refused rather than silently ignored.
	cur.TVAudioOutput = "bluetooth"
	if _, err := h.app.UpdateSettings(ctx, cur); err == nil {
		t.Error("an unknown output should be rejected")
	}
}

func TestChangingTVAudioOutputReloadsPlayback(t *testing.T) {
	// Setting the property alone does nothing to a stream that is already
	// playing. When an audio output fails to open -- an HDMI sink with no
	// speakers behind it -- mpv gives up on audio for that session and leaves
	// the track deselected; pointing it somewhere working afterwards changes
	// nothing, and even selecting the track by hand stays silent, because the
	// output object was never created. Only a reload rebuilds the audio chain.
	h := newHarness(t, nil)
	ctx := context.Background()
	h.player.Reset()

	cur := h.app.Settings()
	cur.TVAudioOutput = config.TVAudioSpeaker
	if _, err := h.app.UpdateSettings(ctx, cur); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	if n := len(h.player.CallsNamed("loadfile")); n == 0 {
		t.Error("playback was not reloaded, so the new device will not be used")
	}
}

// lastSetProperty returns the most recent value mpv was given for a property.
func lastSetProperty(p *mpv.Fake, name string) (string, bool) {
	calls := p.CallsNamed("set_property")
	for i := len(calls) - 1; i >= 0; i-- {
		if len(calls[i].Args) >= 3 {
			if n, _ := calls[i].Args[1].(string); n == name {
				v, _ := calls[i].Args[2].(string)
				return v, true
			}
		}
	}
	return "", false
}
