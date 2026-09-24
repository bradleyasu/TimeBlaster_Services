package app

import (
	"context"
	"fmt"
	"time"

	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/storage"
	"github.com/bradsheets/timeblaster/internal/web"
)

// Settings reads the user's preferences, falling back to the configuration file
// for anything not yet chosen in the companion app.
//
// The layering is deliberate: the config file is the installation's defaults and
// the database is the user's choices, so reinstalling does not discard
// preferences and editing the config file does not silently override them.
func (a *App) Settings() web.Settings {
	return web.Settings{
		Timezone:       storage.GetString(a.store, storage.KeyTimezone, a.cfg.General.Timezone),
		Clock24h:       storage.GetBool(a.store, storage.KeyClock24h, a.cfg.General.Clock24h),
		DisplayOn:      a.storedDisplayOn(),
		DefaultSoundID: storage.GetString(a.store, storage.KeyDefaultSoundID, a.cfg.Audio.DefaultSoundID),
		OverlayEnabled: storage.GetBool(a.store, storage.KeyChannelOverlayOn, a.cfg.Overlay.Enabled),
	}
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
