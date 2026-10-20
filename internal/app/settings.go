package app

import (
	"context"
	"fmt"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/storage"
	"github.com/bradsheets/timeblaster/internal/system"
	"github.com/bradsheets/timeblaster/internal/web"
)

// Settings reads the user's preferences, falling back to the configuration file
// for anything not yet chosen in the companion app.
//
// The layering is deliberate: the config file is the installation's defaults and
// the database is the user's choices, so reinstalling does not discard
// preferences and editing the config file does not silently override them.
func (a *App) Settings() web.Settings {
	s := web.Settings{
		Timezone:       storage.GetString(a.store, storage.KeyTimezone, a.cfg.General.Timezone),
		Clock24h:       storage.GetBool(a.store, storage.KeyClock24h, a.cfg.General.Clock24h),
		DisplayOn:      a.storedDisplayOn(),
		DefaultSoundID: storage.GetString(a.store, storage.KeyDefaultSoundID, a.cfg.Audio.DefaultSoundID),
		OverlayEnabled: storage.GetBool(a.store, storage.KeyChannelOverlayOn, a.cfg.Overlay.Enabled),
		TVAudioOutput:  a.storedTVAudioOutput(),
	}
	s.RemoteAccessEnabled, s.RemoteAccessAvailable = a.remoteAccessState()
	return s
}

// remoteStatusTimeout bounds the helper call made while building the settings
// document. It is on the path of every settings fetch, so a helper that has
// stopped answering must not hold the app's settings tab open.
const remoteStatusTimeout = 3 * time.Second

// remoteAccessState asks the helper what Raspberry Pi Connect is doing.
//
// The live state is authoritative rather than the stored preference: Connect can
// be turned on or off from a terminal, and a switch that shows what we last
// wrote instead of what is true would be worse than no switch. The stored value
// only decides what happens at startup.
func (a *App) remoteAccessState() (enabled, available bool) {
	if a.wifi == nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteStatusTimeout)
	defer cancel()

	st, err := a.wifi.RemoteStatus(ctx)
	if err != nil {
		a.log.Debug("could not read the remote access state", "error", err)
		return false, false
	}
	// Availability, not signed-in: Connect reports nothing about the account
	// while it is stopped, and stopped is the state this switch exists to get
	// out of. Gating on SignedIn hid the control exactly when it was needed.
	return st.Enabled, st.Available
}

// storedTVAudioOutput reads where the television's sound should go, falling
// back to the configured default if the stored value is not one we know.
func (a *App) storedTVAudioOutput() string {
	v := storage.GetString(a.store, storage.KeyTVAudioOutput, a.cfg.MPV.AudioOutput)
	if !config.ValidTVAudioOutput(v) {
		v = config.TVAudioHDMI
	}
	return v
}

// tvAudioDevice turns the chosen output into an mpv audio-device string.
//
// Always by card name, never by number: ALSA numbers cards in enumeration
// order, so a USB speaker plugged in before the HDMI drivers load becomes card
// 0 and mpv's "auto" follows it. That is how the television's sound ended up
// coming out of the alarm speaker.
func (a *App) tvAudioDevice(choice string) string {
	if choice == config.TVAudioSpeaker {
		if card := a.audio.CardID(); card != "" {
			return system.ALSADeviceForCard(card)
		}
		a.log.Warn("no alarm speaker to send the television's audio to; using HDMI")
	}
	return system.ALSADeviceForCard(system.ConnectedHDMICard(a.cfg.MPV.DRMRoot))
}

// applyTVAudioOutput points mpv at the chosen output.
//
// Setting the property is not enough on its own for a stream that is already
// playing: when an audio output fails to open -- an HDMI sink with no speakers
// behind it, say -- mpv gives up on audio for that playback session and leaves
// the track deselected. Pointing it at a working device afterwards changes
// nothing, and even selecting the track by hand produces no sound, because the
// output object was never created. Only reloading the file rebuilds the audio
// chain, which is why callers that change the setting reload afterwards.
//
// Re-applied whenever mpv reconnects, because a fresh process starts back on
// its own default. That path needs no reload of its own: the device is set
// before anything is loaded.
func (a *App) applyTVAudioOutput(ctx context.Context) {
	if a.tvPlayer == nil {
		return
	}
	choice := a.storedTVAudioOutput()
	device := a.tvAudioDevice(choice)
	if err := a.tvPlayer.SetProperty(ctx, "audio-device", device); err != nil {
		a.log.Debug("could not set the television's audio device",
			"output", choice, "device", device, "error", err)
		return
	}
	a.log.Info("television audio output set", "output", choice, "device", device)
}

// storedDisplayOn reads whether the display should be lit.
//
// Devices installed before the display became a switch stored a 0-100
// brightness level instead. That value is still honoured once, so upgrading
// leaves the display in the state the user left it in rather than silently
// reverting to the configured default.
func (a *App) storedDisplayOn() bool {
	if _, ok, _ := a.store.GetSetting(storage.KeyDisplayOn); ok {
		return storage.GetBool(a.store, storage.KeyDisplayOn, a.cfg.General.DisplayOn)
	}
	if v, ok, _ := a.store.GetSetting(storage.KeyLegacyDisplayBrightness); ok {
		return v != "0"
	}
	return a.cfg.General.DisplayOn
}

// UpdateSettings persists preferences and applies them to the running
// subsystems.
//
// Each change is written first and applied second, so a crash between the two
// leaves the device with the setting the user asked for rather than the one it
// happened to be using.
func (a *App) UpdateSettings(ctx context.Context, s web.Settings) (web.Settings, error) {
	current := a.Settings()

	if s.Timezone != current.Timezone {
		loc, err := time.LoadLocation(s.Timezone)
		if err != nil {
			return web.Settings{}, fmt.Errorf("app: %q is not a known timezone: %w", s.Timezone, err)
		}
		if err := a.store.SetSetting(storage.KeyTimezone, s.Timezone); err != nil {
			return web.Settings{}, err
		}
		a.scheduler.SetLocation(loc)
		// The Nano keeps local time for display continuity, so a timezone change
		// must reach it immediately rather than waiting for the next periodic
		// sync — otherwise the 7-segment display is wrong for up to a minute.
		if a.nano != nil {
			if err := a.nano.SetTime(a.clock.Now()); err != nil {
				a.log.Debug("could not resync the Nano after a timezone change", "error", err)
			}
		}
		a.log.Info("timezone changed", "timezone", s.Timezone)
	}

	if s.Clock24h != current.Clock24h {
		if err := storage.SetBool(a.store, storage.KeyClock24h, s.Clock24h); err != nil {
			return web.Settings{}, err
		}
		// The 7-segment display formats the time itself, so the preference has
		// to reach it. Without this the setting changed the companion app and
		// left the hardware clock in whatever mode it booted in.
		if a.nano != nil {
			if err := a.nano.SetClock24h(s.Clock24h); err != nil {
				a.log.Debug("could not tell the Nano the clock format changed", "error", err)
			}
		}
		a.log.Info("clock format changed", "clock_24h", s.Clock24h)
	}

	if s.DisplayOn != current.DisplayOn {
		if err := storage.SetBool(a.store, storage.KeyDisplayOn, s.DisplayOn); err != nil {
			return web.Settings{}, err
		}
		if a.nano != nil {
			if err := a.nano.SetDisplayOn(s.DisplayOn); err != nil {
				a.log.Debug("could not change the display state", "error", err)
			}
		}
		a.log.Info("display switched", "on", s.DisplayOn)
	}

	if s.DefaultSoundID != current.DefaultSoundID {
		// Validate before storing: an alarm pointing at a missing sound would
		// fall back at ring time, which is far too late to tell the user.
		if s.DefaultSoundID != "" {
			sound, err := a.library.Get(s.DefaultSoundID)
			if err != nil {
				return web.Settings{}, fmt.Errorf("app: sound %q is not available", s.DefaultSoundID)
			}
			if err := a.library.Refresh(); err == nil {
				_ = sound // re-read after the refresh below
			}
		}
		if err := a.store.SetSetting(storage.KeyDefaultSoundID, s.DefaultSoundID); err != nil {
			return web.Settings{}, err
		}
		a.scheduler.SetDefaultSound(s.DefaultSoundID)
	}

	// An omitted value means "leave it as it is". There is nothing an empty
	// string could usefully mean for a two-valued choice, and an API client
	// that sends only the fields it cares about should not be rejected.
	if s.TVAudioOutput != "" && s.TVAudioOutput != current.TVAudioOutput {
		if !config.ValidTVAudioOutput(s.TVAudioOutput) {
			return web.Settings{}, fmt.Errorf("app: tv_audio_output must be %q or %q, got %q",
				config.TVAudioHDMI, config.TVAudioSpeaker, s.TVAudioOutput)
		}
		if err := a.store.SetSetting(storage.KeyTVAudioOutput, s.TVAudioOutput); err != nil {
			return web.Settings{}, err
		}
		a.applyTVAudioOutput(ctx)
		// Reload whatever is on screen so the new device is actually used. See
		// applyTVAudioOutput: a stream that is already playing keeps its dead
		// audio output until the file is loaded again.
		if a.screen != nil {
			a.screen.RestorePlayback()
		}
		a.log.Info("television audio output changed", "output", s.TVAudioOutput)
	}

	if s.RemoteAccessEnabled != current.RemoteAccessEnabled {
		if a.wifi == nil {
			return web.Settings{}, fmt.Errorf("app: remote access cannot be changed without the privileged helper")
		}
		if _, err := a.wifi.SetRemoteAccess(ctx, s.RemoteAccessEnabled); err != nil {
			return web.Settings{}, err
		}
		// Recorded so startup can restore it. The helper is still the authority
		// on what is actually running; this only says what we asked for.
		if err := storage.SetBool(a.store, storage.KeyRemoteAccessOn, s.RemoteAccessEnabled); err != nil {
			return web.Settings{}, err
		}
		a.log.Info("remote access changed", "enabled", s.RemoteAccessEnabled)
	}

	if s.OverlayEnabled != current.OverlayEnabled {
		if err := storage.SetBool(a.store, storage.KeyChannelOverlayOn, s.OverlayEnabled); err != nil {
			return web.Settings{}, err
		}
		// The overlay renderer reads its configuration at construction, so this
		// takes effect on the next restart.
		// TODO: make the overlay renderer's configuration live-updatable.
		a.log.Info("channel overlay preference changed; it applies after the next restart",
			"enabled", s.OverlayEnabled)
	}

	saved := a.Settings()
	a.publish(state.NewEvent(state.EventSettingsChanged, saved))
	return saved, nil
}
