package app

import (
	"context"
	"errors"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/storage"
)

// This file holds every cross-subsystem policy decision in the device. It is
// deliberately the only place where "a knob moved" becomes "change the channel"
// and "a button was pressed" becomes "stop the alarm", so the behaviour of the
// physical hardware can be read end to end without chasing callbacks.

// --- physical input ----------------------------------------------------------

// OnChannelSelect handles movement of the channel potentiometer.
//
// The knob is an absolute-position control: its physical position, divided into
// as many bands as there are channels, *is* the current channel. A band of -1
// means the knob is somewhere with no channel behind it, and the television
// shows the standby image.
func (a *App) OnChannelSelect(ev input.ChannelSelect) {
	if a.media == nil {
		return
	}
	a.requestBand(ev.Band)
}

// OnVolumeChange handles movement of the alarm-volume potentiometer.
//
// Physical=true marks the knob as authoritative, which is what stops any
// software-set value from surviving past the next time someone touches it.
func (a *App) OnVolumeChange(ev input.VolumeChange) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := a.audio.SetAlarmVolume(ctx, ev.Percent, ev.Physical); err != nil {
			a.log.Warn("could not apply the alarm volume", "percent", ev.Percent, "error", err)
			return
		}
		// Recorded for diagnostics only. It is deliberately never replayed at
		// startup: the knob's physical position is the truth, and restoring a
		// stale value would fight it.
		if err := storage.SetInt(a.store, storage.KeyLastKnownVolume, ev.Percent); err != nil {
			a.log.Debug("could not record the last known volume", "error", err)
		}
		a.publish(state.NewEvent(state.EventVolumeChanged, map[string]any{
			"percent":            ev.Percent,
			"knob_authoritative": ev.Physical,
		}))
	}()
}

// OnReservedPot handles pots 2 and 3, which have no assigned function.
//
// TODO: assign functions to pots 2 and 3. The plumbing is complete — filtering,
// calibration and event delivery all work — so implementing one is a matter of
// adding a case here and, if it maps to discrete positions, a band mapper in the
// input router. Candidates considered: TV/media volume, display brightness,
// snooze duration, channel-overlay duration.
func (a *App) OnReservedPot(ev input.ReservedPotChange) {
	a.log.Debug("unassigned potentiometer moved", "index", ev.Index, "position", ev.Position)
}

// OnButton handles the two physical buttons.
func (a *App) OnButton(ev input.ButtonEvent) {
	switch ev.Name {
	case protocol.ButtonAlarmOff:
		a.handleAlarmOffButton(ev)
	case protocol.ButtonWiFi:
		a.handleWiFiButton(ev)
	default:
		a.log.Debug("press of an unrecognised button", "button", ev.Name, "action", ev.Action.String())
	}
}

// handleAlarmOffButton implements the big red button.
//
// Its one job is dismissing a ringing alarm. When nothing is ringing a press
// does nothing at all — deliberately, so the button never acquires a second
// meaning that could surprise someone slapping it half-asleep.
func (a *App) handleAlarmOffButton(ev input.ButtonEvent) {
	if ev.Action != input.ActionShortPress && ev.Action != input.ActionHold {
		return
	}
	if a.scheduler.Active() == nil {
		a.log.Debug("big red button pressed with no alarm active; ignoring")
		return
	}
	a.log.Info("big red button pressed; dismissing the active alarm")
	if err := a.scheduler.Dismiss(); err != nil && !errors.Is(err, alarm.ErrNoneActive) {
		a.log.Error("could not dismiss the alarm from the button", "error", err)
	}
}

// handleWiFiButton implements the Wi-Fi setup button.
//
// A five-second hold is the one and only way into Wi-Fi setup mode. A short
// press does nothing; the device never enters setup mode on its own just because
// a network is unreachable.
func (a *App) handleWiFiButton(ev input.ButtonEvent) {
	if ev.Action != input.ActionHold {
		if ev.Action == input.ActionShortPress {
			a.log.Info("Wi-Fi button pressed briefly; hold it for five seconds to enter setup mode")
		}
		return
	}

	a.log.Warn("Wi-Fi button held; entering setup mode",
		"held", ev.Held.Round(time.Millisecond), "ssid", a.cfg.WiFi.SetupSSID)

	// Tell the user something is happening before the network goes away.
	if a.nano != nil {
		if err := a.nano.ShowText("SETUP"); err != nil {
			a.log.Debug("could not update the display for setup mode", "error", err)
		}
		_ = a.nano.SetLED(protocol.LEDWiFi, true)
	}
	if a.overlay != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := a.overlay.ShowText(ctx, "WI-FI SETUP", 10*time.Second); err != nil {
			a.log.Debug("could not show the setup overlay", "error", err)
		}
		cancel()
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		if err := a.wifi.EnterSetupMode(ctx); err != nil {
			a.log.Error("could not enter Wi-Fi setup mode", "error", err)
			if a.nano != nil {
				_ = a.nano.ShowText("ERR")
				// Return to the clock shortly so the display is not stuck on an
				// error nobody can clear.
				time.AfterFunc(5*time.Second, func() {
					_ = a.nano.ShowClock()
					_ = a.nano.SetLED(protocol.LEDWiFi, false)
				})
			}
			return
		}
		a.log.Warn("Wi-Fi setup mode active",
			"ssid", a.cfg.WiFi.SetupSSID,
			"portal", "http://"+trimCIDR(a.cfg.WiFi.SetupAddress))
		a.publish(state.NewEvent(state.EventWiFiChanged, map[string]any{
			"mode": "setup", "ssid": a.cfg.WiFi.SetupSSID,
		}))
	}()
}

// --- alarm lifecycle ---------------------------------------------------------

// AlarmStarted begins alarm audio and tells the Nano.
//
// Audio and hardware are handled independently: a failure in either must not
// prevent the other, because an alarm that lights the LED but makes no sound is
// still better than nothing, and vice versa.
func (a *App) AlarmStarted(active alarm.Active) {
	a.log.Info("alarm ringing", "alarm_id", active.AlarmID, "label", active.Label, "sound", active.SoundID)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := a.audio.PlayAlarm(ctx, active.SoundID); err != nil {
			a.log.Error("alarm audio failed to start; the alarm is still active "+
				"and can be dismissed with the big red button",
				"alarm_id", active.AlarmID, "sound", active.SoundID, "error", err)
		}
	}()

	if a.nano != nil {
		if err := a.nano.SetAlarmActive(true); err != nil {
			a.log.Debug("could not tell the Nano the alarm is active", "error", err)
		}
		_ = a.nano.SetLED(protocol.LEDAlarm, true)
	}
	a.publish(state.NewEvent(state.EventAlarmStarted, active))
}

// AlarmStopped silences the speaker and returns the hardware to normal.
func (a *App) AlarmStopped(active alarm.Active, reason alarm.StopReason) {
	a.log.Info("alarm ended", "alarm_id", active.AlarmID, "reason", string(reason))

	if err := a.audio.StopAlarm(); err != nil {
		a.log.Warn("could not stop alarm audio", "alarm_id", active.AlarmID, "error", err)
	}
	if a.nano != nil {
		if err := a.nano.SetAlarmActive(false); err != nil {
			a.log.Debug("could not tell the Nano the alarm ended", "error", err)
		}
		_ = a.nano.SetLED(protocol.LEDAlarm, false)
		_ = a.nano.ShowClock()
	}
	a.publish(state.NewEvent(state.EventAlarmStopped, map[string]any{
		"alarm_id": active.AlarmID, "reason": string(reason),
	}))
}

// AlarmSnoozed quiets the speaker but leaves the alarm active.
func (a *App) AlarmSnoozed(active alarm.Active) {
	if err := a.audio.StopAlarm(); err != nil {
		a.log.Warn("could not stop alarm audio for the snooze", "error", err)
	}
	// The Nano is still told the alarm is active: the display and LED should keep
	// showing that something is pending, not return to a normal clock face.
	if a.nano != nil {
		_ = a.nano.SetAlarmActive(true)
		_ = a.nano.SetLED(protocol.LEDAlarm, false)
	}
	a.publish(state.NewEvent(state.EventAlarmSnoozed, active))
}

// ScheduleChanged pushes the next-alarm time to connected apps.
func (a *App) ScheduleChanged(next *alarm.Upcoming) {
	a.publish(state.NewEvent(state.EventScheduleChanged, next))
}

// --- hardware link -----------------------------------------------------------

// LinkUp resynchronises derived state when the Nano appears.
func (a *App) LinkUp(device string) {
	a.log.Info("Nano link established", "device", device)

	// Everything the input layer derived from the old session is stale: knobs may
	// have been turned and buttons pressed while the cable was out. Forget it all
	// and let the Nano's first reports re-establish the truth.
	a.router.Reset()
	if a.media != nil {
		a.router.SetChannelCount(a.media.ChannelCount())
	}
	a.publish(state.NewEvent(state.EventHardwareChanged, map[string]any{
		"connected": true, "device": device,
	}))
}

// LinkDown records the Nano disappearing.
func (a *App) LinkDown(err error) {
	a.log.Warn("Nano link lost; physical controls are unavailable until it returns", "error", err)
	a.router.Reset()
	a.publish(state.NewEvent(state.EventHardwareChanged, map[string]any{
		"connected": false,
	}))
}

// --- media -------------------------------------------------------------------

// ChannelListChanged re-bands the channel knob for the new channel count.
//
// This is what makes the channel count dynamic: adding a channel in ErsatzTV
// re-divides the knob's travel immediately, with no restart and nothing
// hardcoded.
func (a *App) ChannelListChanged(channels []ersatztv.Channel) {
	// Re-band the knob for the new count and take its verdict. A nil verdict
	// means the knob has never reported a position, so it cannot choose and the
	// standby screen is the answer until it does.
	//
	// Exactly one request follows, which is the point: this used to both ask
	// the knob and separately decide for itself, and the two raced.
	band := -1
	if sel := a.router.SetChannelCount(len(channels)); sel != nil {
		band = sel.Band
	} else if len(channels) > 0 {
		a.log.Info("channels are available but the channel knob has not reported; "+
			"showing the standby screen until it does", "channels", len(channels))
	}
	a.requestBand(band)

	a.publish(state.NewEvent(state.EventChannelsChanged, channels))
}

// ChannelChanged records and announces the selected channel.
func (a *App) ChannelChanged(ch *ersatztv.Channel) {
	if ch != nil {
		if err := a.store.SetSetting(storage.KeyLastChannelNumber, ch.Number); err != nil {
			a.log.Debug("could not record the last channel", "error", err)
		}
	}
	a.publish(state.NewEvent(state.EventChannelChanged, ch))
}

// publish broadcasts an event to connected companion apps.
//
// It tolerates a nil hub because the subsystems are wired before the web server
// exists, and loading the alarm set during construction legitimately produces a
// schedule-changed event before anything is listening.
func (a *App) publish(ev state.Event) {
	if a.hub == nil {
		return
	}
	a.hub.Publish(ev)
}

func trimCIDR(addr string) string {
	for i, c := range addr {
		if c == '/' {
			return addr[:i]
		}
	}
	return addr
}
