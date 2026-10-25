package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/mpv"
)

// Raw readings for the mode knob's two halves.
const (
	modeKnobTV       = 300
	modeKnobAmbience = 3800
)

// ambienceHarness is a harness with ambience folders: ch1 holds one video, ch2
// holds two.
func ambienceHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"ch1-fireplace/fire.mp4", "ch2/1.mp4", "ch2/2.mp4"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, func(c *config.Config) { c.Ambience.Dir = dir })
	h.syncChannels(t)
	return h
}

// lastLoad is the most recent file mpv was asked to play, by base name.
func lastLoad(h *harness) string {
	loads := h.player.CallsNamed("loadfile")
	if len(loads) == 0 {
		return ""
	}
	return filepath.Base(loads[len(loads)-1].Args[1].(string))
}

func activeMode(h *harness) string { return h.app.screen.Active().ID() }

func TestTheModeKnobSwitchesToAmbience(t *testing.T) {
	h := ambienceHarness(t)
	h.app.router.PotReport(input.PotChannel, 400) // channel knob near the start
	waitFor(t, "a TV channel", func() bool { return h.app.media.Current() != nil })

	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	waitFor(t, "ambience", func() bool { return lastLoad(h) == "fire.mp4" })

	if activeMode(h) != config.ModeAmbience {
		t.Errorf("active mode %q", activeMode(h))
	}
	// The TV has let go: nothing it remembers can be mistaken for what is on
	// screen, and the snapshot says ambience.
	if h.app.media.Current() != nil {
		t.Error("the TV still claims a channel")
	}
	snap := h.app.Snapshot().Screen
	if snap.Mode != config.ModeAmbience || snap.Showing.Number != "1" || snap.Showing.Name != "Fireplace" {
		t.Errorf("snapshot %+v", snap)
	}
	waitFor(t, "the display", func() bool {
		for _, c := range h.nano.CommandsOfKind("display-text") {
			if c.Text == "Am.01" {
				return true
			}
		}
		return false
	})
}

func TestTheChannelKnobPicksAmbienceFoldersInAmbienceMode(t *testing.T) {
	h := ambienceHarness(t)
	h.app.router.PotReport(input.PotChannel, 400)
	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	waitFor(t, "ambience", func() bool { return lastLoad(h) == "fire.mp4" })

	// Two ambience channels: the knob is divided in two, and the far end is
	// channel 2 -- whatever the TV had.
	if got := h.app.router.ChannelCount(); got != 2 {
		t.Errorf("channel knob divided into %d, want 2 ambience channels", got)
	}
	h.app.router.PotReport(input.PotChannel, 3800)
	waitFor(t, "ambience channel 2", func() bool { return lastLoad(h) == "1.mp4" })
	if h.app.media.Current() != nil {
		t.Error("the channel knob changed the TV channel in ambience mode")
	}
}

func TestPlayerEventsDriveTheAmbiencePlaylist(t *testing.T) {
	// mpv's events reach the mode on screen: a video ending moves ambience
	// to the next, and does not make the TV reload a channel it is not
	// showing.
	h := ambienceHarness(t)
	h.app.router.PotReport(input.PotChannel, 3800)
	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	waitFor(t, "ambience channel 2", func() bool { return lastLoad(h) == "1.mp4" })

	h.app.onPlayerEvent(mpv.Event{Name: "playback-restart"})
	h.app.onPlayerEvent(mpv.Event{Name: "end-file", Reason: "eof"})
	waitFor(t, "the next video", func() bool { return lastLoad(h) == "2.mp4" })
}

func TestTVChannelChangesWaitWhileInAmbience(t *testing.T) {
	// ErsatzTV's list changing is about the TV. In ambience mode it must not
	// re-divide the knob or touch the screen.
	h := ambienceHarness(t)
	h.app.router.PotReport(input.PotChannel, 400)
	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	waitFor(t, "ambience", func() bool { return lastLoad(h) == "fire.mp4" })
	loads := len(h.player.CallsNamed("loadfile"))

	h.etv.SetChannels(ersatztv.Channel{Number: "9", Name: "New"})
	h.syncChannels(t)

	if got := h.app.router.ChannelCount(); got != 2 {
		t.Errorf("the TV's list re-divided the knob in ambience mode: %d bands", got)
	}
	if n := len(h.player.CallsNamed("loadfile")); n != loads {
		t.Errorf("a TV list change loaded something in ambience mode: %v", h.player.CallsNamed("loadfile"))
	}
}

func TestTurningBackToTVPicksUpWhereTheKnobPoints(t *testing.T) {
	h := ambienceHarness(t)
	h.app.router.PotReport(input.PotChannel, 3800)
	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	waitFor(t, "ambience", func() bool { return activeMode(h) == config.ModeAmbience })

	h.app.router.PotReport(input.PotMode, modeKnobTV)
	waitFor(t, "TV channel 4", func() bool {
		cur := h.app.media.Current()
		return cur != nil && cur.Number == "4"
	})
	if got := h.app.router.ChannelCount(); got != 4 {
		t.Errorf("channel knob divided into %d, want the 4 TV channels", got)
	}
	if !strings.Contains(h.player.CallsNamed("loadfile")[len(h.player.CallsNamed("loadfile"))-1].String(), "http") {
		t.Errorf("the last load was not a TV stream: %v", h.player.CallsNamed("loadfile"))
	}
}

func TestTheSameModeReportedAgainDoesNothing(t *testing.T) {
	// A reconnect makes the knob report its position afresh. If that is the
	// mode already showing, the picture must not be reloaded.
	h := ambienceHarness(t)
	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	waitFor(t, "ambience", func() bool { return activeMode(h) == config.ModeAmbience })
	waitFor(t, "a picture", func() bool { return lastLoad(h) != "" })
	loads := len(h.player.CallsNamed("loadfile"))

	h.app.router.Reset()
	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	if n := len(h.player.CallsNamed("loadfile")); n != loads {
		t.Errorf("re-reporting the same mode reloaded the picture: %d loads, had %d", n, loads)
	}
}

func TestTheSnapshotListsModesInKnobOrder(t *testing.T) {
	h := ambienceHarness(t)
	var ids []string
	for _, m := range h.app.Snapshot().Screen.Modes {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"tv", "guide", "ambience"}) {
		t.Errorf("modes %v", ids)
	}
	if n := len(h.app.Snapshot().Screen.AmbienceChannels); n != 2 {
		t.Errorf("%d ambience channels in the snapshot", n)
	}
}

func TestAnUnfittedModeKnobLeavesTheTVAlone(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, func(c *config.Config) {
		c.Ambience.Dir = dir
		c.Input.ModePotFitted = false
	})
	h.syncChannels(t)
	h.app.router.PotReport(input.PotMode, modeKnobAmbience)
	h.app.router.PotReport(input.PotChannel, 400)
	waitFor(t, "a TV channel", func() bool { return h.app.media.Current() != nil })
	if activeMode(h) != config.ModeTV {
		t.Errorf("a floating mode pin switched the television to %q", activeMode(h))
	}
}
