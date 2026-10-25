package app

import (
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/input"
)

func displayed(h *harness, text string) bool {
	for _, c := range h.nano.CommandsOfKind("display-text") {
		if c.Text == text {
			return true
		}
	}
	return false
}

func TestTheAudioKnobSwitchesTheTVsSound(t *testing.T) {
	// Exactly what the app's switch does: the setting, mpv's device, and a
	// reload so the new device is actually used.
	h := newHarness(t, nil)
	h.app.router.PotReport(input.PotAudio, 300) // HDMI end: already the default
	h.player.Reset()

	h.app.router.PotReport(input.PotAudio, 3800)
	waitFor(t, "the switch", func() bool { return h.app.Settings().TVAudioOutput == config.TVAudioSpeaker })
	waitFor(t, "the reload", func() bool { return len(h.player.CallsNamed("loadfile")) > 0 })
	if last, _ := lastSetProperty(h.player, "audio-device"); !strings.Contains(last, "CARD="+h.app.audio.CardID()) {
		t.Errorf("mpv was not pointed at the speaker: %q", last)
	}
	waitFor(t, "USb on the display", func() bool { return displayed(h, "USb") })

	h.app.router.PotReport(input.PotAudio, 300)
	waitFor(t, "back to HDMI", func() bool { return h.app.Settings().TVAudioOutput == config.TVAudioHDMI })
	waitFor(t, "HdMi on the display", func() bool { return displayed(h, "HdMi") })
}

func TestTheAudioKnobReportingItsPositionAgainChangesNothing(t *testing.T) {
	// After every reconnect the knob reports where it is. If that is where
	// the sound already goes, the picture must not be reloaded for it.
	h := newHarness(t, nil)
	h.app.router.PotReport(input.PotAudio, 3800)
	waitFor(t, "the switch", func() bool { return h.app.Settings().TVAudioOutput == config.TVAudioSpeaker })
	waitFor(t, "the reload", func() bool { return len(h.player.CallsNamed("loadfile")) > 0 })
	h.player.Reset()
	h.nano.Reset()

	h.app.router.Reset()
	h.app.router.PotReport(input.PotAudio, 3800)
	runAsync(h)
	if n := len(h.player.CallsNamed("loadfile")); n != 0 {
		t.Errorf("re-reporting the same position reloaded the picture %d times", n)
	}
	if displayed(h, "USb") {
		t.Error("re-reporting the same position flashed the display")
	}
}

func TestTheAppsSwitchHoldsUntilTheKnobMoves(t *testing.T) {
	// The knob is authoritative, like the others: the app's choice stands
	// until the knob is next turned.
	h := newHarness(t, nil)
	h.app.router.PotReport(input.PotAudio, 300) // knob on HDMI

	cur := h.app.Settings()
	cur.TVAudioOutput = config.TVAudioSpeaker
	if _, err := h.app.UpdateSettings(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	runAsync(h)
	if got := h.app.Settings().TVAudioOutput; got != config.TVAudioSpeaker {
		t.Fatalf("the app's choice was overridden without the knob moving: %q", got)
	}

	h.app.router.PotReport(input.PotAudio, 3800) // to USB: no change needed
	h.app.router.PotReport(input.PotAudio, 300)  // and back to HDMI: the knob wins
	waitFor(t, "the knob", func() bool { return h.app.Settings().TVAudioOutput == config.TVAudioHDMI })
}

// runAsync gives work the app hands to a goroutine -- the knob's switch runs
// off the input path -- time to finish, for checks that something did not
// happen.
func runAsync(h *harness) {
	_ = h
	time.Sleep(50 * time.Millisecond)
}

func TestTheAudioSwitchIsNotHiddenByTheChannelReloading(t *testing.T) {
	// Switching the output reloads the channel so the new device is used.
	// The reload is the same channel: flashing its number again buried the
	// HdMi the viewer had just turned the knob to see.
	h := newHarness(t, nil)
	h.syncChannels(t)
	h.app.router.PotReport(input.PotAudio, 300)
	h.app.router.PotReport(input.PotChannel, 400)
	waitFor(t, "channel 1", func() bool { return displayed(h, "Ch.01") })
	h.nano.Reset()
	loads := len(h.player.CallsNamed("loadfile"))

	h.app.router.PotReport(input.PotAudio, 3800)
	waitFor(t, "USb", func() bool { return displayed(h, "USb") })
	waitFor(t, "the reload", func() bool { return len(h.player.CallsNamed("loadfile")) > loads })
	runAsync(h)
	if displayed(h, "Ch.01") {
		t.Error("the reload of the same channel flashed its number over USb")
	}

	// A real channel change still announces itself.
	h.app.router.PotReport(input.PotChannel, 3800)
	waitFor(t, "channel 4", func() bool { return displayed(h, "Ch.04") })
}
