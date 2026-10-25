package app

import (
	"context"
	"fmt"
	"time"

	"github.com/bradsheets/timeblaster/internal/ambience"
	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/guide"
	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/screen"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/storage"
)

// This file is the policy for the two knobs that decide what is on the
// television: the mode knob chooses a mode, and the channel knob chooses a
// channel within it. The screen.Director applies the result.

// initScreen builds the television modes, in the order the mode knob lists
// them, and the director that switches between them. It needs the player, so
// it runs at the end of initMedia.
func (a *App) initScreen() error {
	a.ambience = ambience.New(ambience.Options{
		Dir:                a.cfg.Ambience.Dir,
		RescanInterval:     a.cfg.Ambience.RescanInterval.Duration,
		StandbyImage:       a.cfg.MPV.NoChannelImage,
		BannerDuration:     a.cfg.Overlay.Duration.Duration,
		StallTimeout:       a.cfg.MPV.StallTimeout.Duration,
		StallCheckInterval: a.cfg.MPV.StallCheckInterval.Duration,
		StallStartTimeout:  a.cfg.MPV.StallStartTimeout.Duration,
		CommandTimeout:     a.cfg.MPV.CommandTimeout.Duration,
	}, ambience.Deps{
		Player: a.tvPlayer, Overlay: a.overlay, Clock: a.clock,
		Logger: a.log.With("component", "ambience"), Observer: a,
	})

	a.guide = guide.New(guide.Options{
		PageInterval:   a.cfg.Guide.PageInterval.Duration,
		CommandTimeout: a.cfg.MPV.CommandTimeout.Duration,
		// Read on every draw, so the guide follows the settings as they change.
		Clock24h: func() bool {
			return storage.GetBool(a.store, storage.KeyClock24h, a.cfg.General.Clock24h)
		},
		Location: func() *time.Location { return a.scheduler.Location() },
	}, guide.Deps{
		Player: a.tvPlayer, TV: a.media, Clock: a.clock,
		Logger: a.log.With("component", "guide"),
	})

	available := map[string]screen.Mode{
		config.ModeTV:       a.media,
		config.ModeGuide:    a.guide,
		config.ModeAmbience: a.ambience,
	}
	var modes []screen.Mode
	for _, id := range a.cfg.Input.ModeKnob {
		m, ok := available[id]
		if !ok {
			// Validation rejects unknown IDs, so this is a mode listed in
			// config.ScreenModes without being built here.
			return fmt.Errorf("app: the mode knob lists %q, which this daemon cannot build", id)
		}
		modes = append(modes, m)
	}
	a.screen = screen.NewDirector(modes, a.log.With("component", "screen"), a)
	return nil
}

// wantedMode is the mode the knob has asked for. It can be a step ahead of the
// director's active mode, which catches up when it applies the request.
func (a *App) wantedMode() screen.Mode {
	modes := a.screen.Modes()
	i := int(a.wantMode.Load())
	if i < 0 || i >= len(modes) {
		return modes[0]
	}
	return modes[i]
}

// OnModeSelect handles the mode knob.
//
// Like the channel knob it is an absolute-position control: where it points is
// the mode. Switching re-divides the channel knob for the new mode -- four TV
// channels, say, but two ambience ones -- and shows whichever channel the
// channel knob now lands on, so turning the mode knob never leaves the channel
// knob pointing at nothing.
func (a *App) OnModeSelect(ev input.ModeSelect) {
	if a.screen == nil {
		return
	}
	modes := a.screen.Modes()
	if ev.Band < 0 || ev.Band >= len(modes) {
		return
	}
	if int(a.wantMode.Swap(int32(ev.Band))) == ev.Band {
		// The same mode, reported again after a reconnect. Nothing to do.
		return
	}
	mode := modes[ev.Band]
	a.log.Info("mode knob turned", "mode", mode.ID())
	a.screen.Request(ev.Band, a.rebandChannelKnob(mode))
}

// rebandChannelKnob divides the channel knob for a mode and returns the band
// it now points at, or -1 when it has never reported a position.
func (a *App) rebandChannelKnob(m screen.Mode) int {
	if sel := a.router.SetChannelCount(m.ChannelCount()); sel != nil {
		return sel.Band
	}
	return -1
}

// requestBand asks for a channel-knob band in the wanted mode.
func (a *App) requestBand(band int) {
	if a.screen == nil {
		return
	}
	a.screen.Request(int(a.wantMode.Load()), band)
}

// modeChannelsChanged reacts to a mode's channel list changing. Only the
// wanted mode's list matters: the knob is divided for that mode, and another
// mode's list is read afresh when the mode knob next selects it.
//
// It returns whether the change was acted on.
func (a *App) modeChannelsChanged(id string) bool {
	if a.screen == nil {
		return false
	}
	m := a.wantedMode()
	if m.ID() != id {
		return false
	}
	band := a.rebandChannelKnob(m)
	if band < 0 && m.ChannelCount() > 0 {
		a.log.Info("channels are available but the channel knob has not reported; "+
			"showing the standby screen until it does", "mode", id, "channels", m.ChannelCount())
	}
	a.requestBand(band)
	return true
}

// runScreen applies the television's requests, one at a time and in order.
func (a *App) runScreen(ctx context.Context) error {
	if a.screen == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return a.screen.Run(ctx)
}

// ModeChanged announces a mode switch. The new mode's channel banner follows on
// its own, on the television and on the seven-segment display.
func (a *App) ModeChanged(from, to screen.Mode) {
	a.publish(state.NewEvent(state.EventScreenChanged, map[string]any{
		"mode": to.ID(), "title": to.Title(),
	}))
}

// AmbienceChannelsChanged follows ambience folders being added or removed.
func (a *App) AmbienceChannelsChanged(channels []ambience.Channel) {
	a.modeChannelsChanged(config.ModeAmbience)
	a.publish(state.NewEvent(state.EventScreenChanged, map[string]any{
		"ambience_channels": len(channels),
	}))
}

// AmbienceChannelChanged flashes the ambience channel on the display, as a
// channel change does for the TV.
func (a *App) AmbienceChannelChanged(ch *ambience.Channel) {
	if ch != nil {
		a.flashChannelText(nanoAmbienceText(ch.Number))
	}
	a.publish(state.NewEvent(state.EventScreenChanged, ch))
}

// nanoAmbienceText renders an ambience channel for the seven-segment display:
// "Am.02", the same shape as the TV's "Ch.02" so the mode reads at a glance.
func nanoAmbienceText(n int) string {
	if n >= 0 && n <= 99 {
		return fmt.Sprintf("Am.%02d", n)
	}
	return fmt.Sprint(n)
}

// screenState reports the television's mode for the API.
func (a *App) screenState() state.Screen {
	if a.screen == nil {
		return state.Screen{Modes: []state.ScreenMode{}}
	}
	active := a.screen.Active()
	st := state.Screen{
		Mode:    active.ID(),
		Title:   active.Title(),
		Showing: active.Showing(),
		Modes:   []state.ScreenMode{},
		// Reported even without a director, so the app can say where videos
		// would go.
		AmbienceDir: a.cfg.Ambience.Dir,
	}
	for _, m := range a.screen.Modes() {
		st.Modes = append(st.Modes, state.ScreenMode{ID: m.ID(), Title: m.Title()})
	}
	if a.ambience != nil {
		st.AmbienceChannels = a.ambience.Channels()
	}
	if st.AmbienceChannels == nil {
		st.AmbienceChannels = []ambience.Channel{}
	}
	return st
}
