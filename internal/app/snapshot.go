package app

import (
	"context"
	"time"

	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/media"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/storage"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

// Snapshot builds the full live picture served at /api/state and pushed to each
// companion app on connection.
//
// It reads from the subsystems every time rather than maintaining a cached copy.
// The device has one user and a dozen fields; a snapshot that can be stale is a
// worse problem than the cost of assembling it.
func (a *App) Snapshot() state.Snapshot {
	now := a.clock.Now()
	loc := a.scheduler.Location()
	_, offset := now.In(loc).Zone()

	alarms := a.scheduler.List()
	enabled := 0
	for _, al := range alarms {
		if al.Enabled {
			enabled++
		}
	}

	snap := state.Snapshot{
		Version:  a.version,
		Hostname: a.cfg.General.Hostname,
		Clock: state.Clock{
			Now:              now.In(loc),
			Timezone:         loc.String(),
			UTCOffsetSeconds: offset,
			Clock24h:         storage.GetBool(a.store, storage.KeyClock24h, a.cfg.General.Clock24h),
			Uptime:           a.tracker.Uptime(now),
		},
		Alarms: state.AlarmState{
			Active:       a.scheduler.Active(),
			Next:         a.scheduler.Next(),
			Count:        len(alarms),
			EnabledCount: enabled,
		},
		Audio:    a.audio.Health(),
		Hardware: a.hardwareStatus().Status(),
		Inputs:   a.inputSnapshot(),
	}

	if a.media != nil {
		snap.Media = a.media.Status()
		snap.Channels = a.media.Channels()
	} else {
		snap.Media = media.Status{LastError: "the television subsystem is not configured"}
		snap.Channels = []ersatztv.Channel{}
	}
	if snap.Channels == nil {
		snap.Channels = []ersatztv.Channel{}
	}

	snap.WiFi = a.wifiStatus()
	return snap
}

// inputSnapshot reports the knobs' live positions, which makes the companion app
// useful for diagnosing a suspect potentiometer without a screwdriver.
func (a *App) inputSnapshot() state.Inputs {
	pos := func(i int) float64 {
		v, primed := a.router.Position(i)
		if !primed {
			return -1
		}
		return v
	}
	return state.Inputs{
		ChannelKnob:   pos(input.PotChannel),
		VolumeKnob:    pos(input.PotAlarmVolume),
		ReservedKnobs: []float64{pos(input.PotReservedA), pos(input.PotReservedB)},
	}
}

// wifiStatus asks the helper, tolerating its absence.
func (a *App) wifiStatus() wifi.Status {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	st, err := a.wifi.Status(ctx)
	if err != nil {
		return wifi.Status{
			Mode:      wifi.ModeUnknown,
			Interface: a.cfg.WiFi.Interface,
			Hostname:  a.cfg.General.Hostname + ".local",
			LastError: err.Error(),
		}
	}
	if st.Hostname == "" {
		st.Hostname = a.cfg.General.Hostname + ".local"
	}
	return st
}

// Health builds the compact component report served at /api/health.
//
// The classification rule encodes the project's priority order: only an impaired
// alarm path makes the device unhealthy. A dead television, a missing Nano or an
// unreachable ErsatzTV are degraded, because none of them stops the Timeblaster
// waking you up.
func (a *App) Health() state.Health {
	now := a.clock.Now()
	audioHealth := a.audio.Health()
	hw := a.hardwareStatus().Status()
	wifiStatus := a.wifiStatus()

	mediaStatus := media.Status{}
	if a.media != nil {
		mediaStatus = a.media.Status()
	}

	components := map[string]string{}

	// The alarm subsystem is healthy when scheduling works. Audio problems are
	// reported separately because an alarm that rings silently is still an alarm
	// that fired, and the distinction matters when diagnosing.
	failures := a.sup.failures()
	alarmHealthy := true
	if _, failed := failures["alarm-scheduler"]; failed {
		components["alarm"] = state.StatusDown
		alarmHealthy = false
	} else {
		components["alarm"] = state.StatusOK
	}

	switch {
	case audioHealth.DeviceAvailable && audioHealth.LastError == "":
		components["alarm_audio"] = state.StatusOK
	case audioHealth.DeviceAvailable:
		components["alarm_audio"] = state.StatusDegraded
	default:
		components["alarm_audio"] = state.StatusDown
	}
	if audioHealth.SoundCount == 0 {
		components["alarm_sounds"] = state.StatusDown
	} else {
		components["alarm_sounds"] = state.StatusOK
	}

	if hw.Connected {
		components["nano"] = state.StatusOK
	} else {
		components["nano"] = state.StatusDown
	}

	if a.media == nil {
		components["ersatztv"] = state.StatusUnknown
		components["tv_player"] = state.StatusUnknown
	} else {
		if mediaStatus.ErsatzTVReachable {
			components["ersatztv"] = state.StatusOK
		} else {
			components["ersatztv"] = state.StatusDown
		}
		if mediaStatus.PlayerAlive {
			components["tv_player"] = state.StatusOK
		} else {
			components["tv_player"] = state.StatusDown
		}
	}

	switch {
	case wifiStatus.Mode == wifi.ModeSetup:
		components["network"] = state.StatusDegraded
	case wifiStatus.Connected:
		components["network"] = state.StatusOK
	case wifiStatus.Mode == wifi.ModeUnknown:
		components["network"] = state.StatusUnknown
	default:
		components["network"] = state.StatusDown
	}

	if a.wifi.Available() {
		components["wifi_helper"] = state.StatusOK
	} else {
		components["wifi_helper"] = state.StatusDown
	}

	if _, failed := failures["web"]; failed {
		components["web"] = state.StatusDown
	} else {
		components["web"] = state.StatusOK
	}

	active := a.scheduler.Active()
	return state.Health{
		Status:     state.Overall(components, alarmHealthy),
		Version:    a.version,
		Uptime:     a.tracker.Uptime(now).Round(time.Second).String(),
		Components: components,
		Details: state.HealthDetails{
			NanoConnected:       hw.Connected,
			ErsatzTVReachable:   mediaStatus.ErsatzTVReachable,
			PlayerAlive:         mediaStatus.PlayerAlive,
			AlarmDeviceReady:    audioHealth.DeviceAvailable,
			AlarmSubsystemReady: alarmHealthy,
			WiFiHelperReachable: a.wifi.Available(),
			WiFiMode:            string(wifiStatus.Mode),
			CurrentChannel:      mediaStatus.CurrentChannel,
			ChannelCount:        mediaStatus.ChannelCount,
			AlarmCount:          len(a.scheduler.List()),
			AlarmRinging:        active != nil && active.State == "ringing",
		},
	}
}
