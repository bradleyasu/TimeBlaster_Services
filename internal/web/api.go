package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/audio"
	"github.com/bradsheets/timeblaster/internal/media"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

// maxBodyBytes bounds request bodies. Nothing the API accepts is large, and this
// device shares its memory with FFmpeg.
const maxBodyBytes = 64 << 10

// apiError is the error shape every failing endpoint returns, so the app has one
// code path for failures.
type apiError struct {
	Error string `json:"error"`
	// Detail carries a longer explanation where one helps.
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, msg string, detail ...string) {
	e := apiError{Error: msg}
	if len(detail) > 0 {
		e.Detail = detail[0]
	}
	writeJSON(w, status, e)
}

// decodeBody reads a bounded JSON body, rejecting unknown fields so a typo in a
// client is reported rather than silently ignored.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("malformed request body: %w", err)
	}
	return nil
}

// pathID extracts a numeric path parameter.
func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a valid id", raw)
	}
	return id, nil
}

// reqContext bounds a handler's work so a stuck subsystem cannot hold an HTTP
// worker indefinitely.
func reqContext(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// --- diagnostics -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := s.deps.Snapshots.Health()
	// A degraded device still answers 200: the endpoint is for humans and simple
	// monitors, and a non-200 for "the television is off" would be misleading.
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Snapshots.Snapshot())
}

// --- alarms ------------------------------------------------------------------

// alarmDTO is the wire representation of an alarm. It is kept separate from
// alarm.Alarm so that adding an internal field does not silently change the API,
// and so the computed next-occurrence can be included.
type alarmDTO struct {
	ID              int64      `json:"id"`
	Label           string     `json:"label"`
	Hour            int        `json:"hour"`
	Minute          int        `json:"minute"`
	Enabled         bool       `json:"enabled"`
	RepeatDays      int        `json:"repeat_days"`
	RepeatLabel     string     `json:"repeat_label"`
	OneShotDate     string     `json:"one_shot_date,omitempty"`
	SoundID         string     `json:"sound_id"`
	SnoozeMinutes   int        `json:"snooze_minutes"`
	AutoStopMinutes int        `json:"auto_stop_minutes"`
	NextOccurrence  *time.Time `json:"next_occurrence"`
}

func (s *Server) toDTO(a alarm.Alarm) alarmDTO {
	d := alarmDTO{
		ID: a.ID, Label: a.Label, Hour: a.Hour, Minute: a.Minute,
		Enabled: a.Enabled, RepeatDays: a.RepeatDays, RepeatLabel: a.RepeatString(),
		OneShotDate: a.OneShotDate, SoundID: a.SoundID,
		SnoozeMinutes: a.SnoozeMinutes, AutoStopMinutes: a.AutoStopMinutes,
	}
	if next, ok := a.NextOccurrence(time.Now(), s.deps.Alarms.Location()); ok {
		d.NextOccurrence = &next
	}
	return d
}

// alarmInput is the request body for creating or updating an alarm.
//
// Pointer fields distinguish "absent" from "zero" so a partial update does not
// silently disable an alarm or clear its repeat days.
type alarmInput struct {
	Label           *string `json:"label"`
	Hour            *int    `json:"hour"`
	Minute          *int    `json:"minute"`
	Enabled         *bool   `json:"enabled"`
	RepeatDays      *int    `json:"repeat_days"`
	OneShotDate     *string `json:"one_shot_date"`
	SoundID         *string `json:"sound_id"`
	SnoozeMinutes   *int    `json:"snooze_minutes"`
	AutoStopMinutes *int    `json:"auto_stop_minutes"`
}

func (in alarmInput) applyTo(a alarm.Alarm) alarm.Alarm {
	if in.Label != nil {
		a.Label = *in.Label
	}
	if in.Hour != nil {
		a.Hour = *in.Hour
	}
	if in.Minute != nil {
		a.Minute = *in.Minute
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if in.RepeatDays != nil {
		a.RepeatDays = *in.RepeatDays
	}
	if in.OneShotDate != nil {
		a.OneShotDate = *in.OneShotDate
	}
	if in.SoundID != nil {
		a.SoundID = *in.SoundID
	}
	if in.SnoozeMinutes != nil {
		a.SnoozeMinutes = *in.SnoozeMinutes
	}
	if in.AutoStopMinutes != nil {
		a.AutoStopMinutes = *in.AutoStopMinutes
	}
	return a
}

func (s *Server) handleListAlarms(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Alarms.List()
	out := make([]alarmDTO, 0, len(list))
	for _, a := range list {
		out = append(out, s.toDTO(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"alarms": out})
}

func (s *Server) handleGetAlarm(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := s.deps.Alarms.Get(id)
	if err != nil {
		s.writeAlarmError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(a))
}

func (s *Server) handleCreateAlarm(w http.ResponseWriter, r *http.Request) {
	var in alarmInput
	if err := decodeBody(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Hour == nil || in.Minute == nil {
		writeError(w, http.StatusBadRequest, "hour and minute are required")
		return
	}

	a := in.applyTo(alarm.New(*in.Hour, *in.Minute))
	saved, err := s.deps.Alarms.Save(a)
	if err != nil {
		s.writeAlarmError(w, err)
		return
	}
	s.log.Info("alarm created via the companion app",
		"alarm_id", saved.ID, "time", saved.TimeString(), "repeat", saved.RepeatString())
	writeJSON(w, http.StatusCreated, s.toDTO(saved))
}

func (s *Server) handleUpdateAlarm(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.deps.Alarms.Get(id)
	if err != nil {
		s.writeAlarmError(w, err)
		return
	}

	var in alarmInput
	if err := decodeBody(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	saved, err := s.deps.Alarms.Save(in.applyTo(existing))
	if err != nil {
		s.writeAlarmError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(saved))
}

func (s *Server) handleDeleteAlarm(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.deps.Alarms.Delete(id); err != nil {
		s.writeAlarmError(w, err)
		return
	}
	s.log.Info("alarm deleted via the companion app", "alarm_id", id)
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleSetAlarmEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := s.deps.Alarms.SetEnabled(id, body.Enabled)
	if err != nil {
		s.writeAlarmError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(saved))
}

func (s *Server) handleTriggerAlarm(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.deps.Alarms.Trigger(id); err != nil {
		s.writeAlarmError(w, err)
		return
	}
	s.log.Info("alarm triggered manually via the companion app", "alarm_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"active": s.deps.Alarms.Active()})
}

func (s *Server) handleActiveAlarm(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"active": s.deps.Alarms.Active(),
		"next":   s.deps.Alarms.Next(),
	})
}

func (s *Server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Alarms.Dismiss(); err != nil {
		if errors.Is(err, alarm.ErrNoneActive) {
			writeError(w, http.StatusConflict, "no alarm is currently ringing")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not dismiss the alarm", err.Error())
		return
	}
	s.log.Info("alarm dismissed via the companion app")
	writeJSON(w, http.StatusOK, map[string]any{"active": nil})
}

func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Alarms.Snooze(); err != nil {
		switch {
		case errors.Is(err, alarm.ErrNoneActive):
			writeError(w, http.StatusConflict, "no alarm is currently ringing")
		case errors.Is(err, alarm.ErrNotRinging):
			writeError(w, http.StatusConflict, "the alarm is already snoozed")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	s.log.Info("alarm snoozed via the companion app")
	writeJSON(w, http.StatusOK, map[string]any{"active": s.deps.Alarms.Active()})
}

func (s *Server) writeAlarmError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, alarm.ErrNotFound):
		writeError(w, http.StatusNotFound, "alarm not found")
	case strings.HasPrefix(err.Error(), "alarm: "):
		// Validation failures carry a message written for a person.
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "alarm: "))
	default:
		s.log.Error("alarm operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "the alarm operation failed", err.Error())
	}
}

// --- sounds and volume -------------------------------------------------------

func (s *Server) handleListSounds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sounds": s.deps.Audio.Sounds()})
}

func (s *Server) handlePreviewSound(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "a sound id is required")
		return
	}
	ctx, cancel := reqContext(r, 15*time.Second)
	defer cancel()

	if err := s.deps.Audio.TestAlarm(ctx, id, 0); err != nil {
		switch {
		case errors.Is(err, audio.ErrBusy):
			writeError(w, http.StatusConflict, "an alarm is currently ringing")
		case errors.Is(err, audio.ErrSoundNotFound), errors.Is(err, audio.ErrNoSounds):
			writeError(w, http.StatusNotFound, "that sound is not available")
		default:
			writeError(w, http.StatusInternalServerError, "could not play the sound", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playing": id})
}

func (s *Server) handleStopPreview(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Audio.StopAlarm(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not stop playback", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playing": nil})
}

func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	pct, authoritative := s.deps.Audio.Volume()
	writeJSON(w, http.StatusOK, map[string]any{
		"percent": pct,
		// knob_authoritative tells the app whether the displayed value came from
		// the physical potentiometer, so it can present it read-only rather than
		// offering a slider that would immediately be overridden.
		"knob_authoritative": authoritative,
		"software_control":   s.deps.Config.Audio.AllowSoftwareVolume,
	})
}

func (s *Server) handleSetVolume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Percent int `json:"percent"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Percent < 0 || body.Percent > 100 {
		writeError(w, http.StatusBadRequest, "percent must be 0-100")
		return
	}
	ctx, cancel := reqContext(r, 10*time.Second)
	defer cancel()

	// physical=false: the knob remains authoritative and will override this on
	// its next movement. The service rejects the call outright unless software
	// volume has been explicitly enabled in the configuration.
	if err := s.deps.Audio.SetAlarmVolume(ctx, body.Percent, false); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	pct, authoritative := s.deps.Audio.Volume()
	writeJSON(w, http.StatusOK, map[string]any{"percent": pct, "knob_authoritative": authoritative})
}

// --- television --------------------------------------------------------------

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"channels": s.deps.Media.Channels(),
		"current":  s.deps.Media.Current(),
		"status":   s.deps.Media.Status(),
	})
}

func (s *Server) handleRefreshChannels(w http.ResponseWriter, r *http.Request) {
	s.deps.Media.RefreshSoon()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "refresh requested"})
}

func (s *Server) handleSelectChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Number string `json:"number"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Number == "" {
		writeError(w, http.StatusBadRequest, "a channel number is required")
		return
	}
	ctx, cancel := reqContext(r, 20*time.Second)
	defer cancel()

	if err := s.deps.Media.SelectNumber(ctx, body.Number); err != nil {
		if errors.Is(err, media.ErrNoChannels) {
			writeError(w, http.StatusNotFound, "no such channel")
			return
		}
		writeError(w, http.StatusBadGateway, "could not start that channel", err.Error())
		return
	}
	// The channel knob is an absolute-position control, so a software selection
	// only holds until the knob is next moved. The app is told so it can say as
	// much rather than appearing to have failed.
	writeJSON(w, http.StatusOK, map[string]any{
		"current":            s.deps.Media.Current(),
		"overridden_by_knob": true,
		"note":               "The channel knob is authoritative; moving it will change the channel again.",
	})
}

func (s *Server) handleClearChannel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r, 20*time.Second)
	defer cancel()
	if err := s.deps.Media.ShowNoChannel(ctx); err != nil {
		writeError(w, http.StatusBadGateway, "could not clear the channel", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": nil})
}

// --- settings ----------------------------------------------------------------

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Settings.Settings())
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	current := s.deps.Settings.Settings()

	// Start from the current values so a partial update leaves the rest alone.
	in := current
	if err := decodeBody(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.DisplayBrightness < 0 || in.DisplayBrightness > 100 {
		writeError(w, http.StatusBadRequest, "display_brightness must be 0-100")
		return
	}
	if in.Timezone != "" {
		if _, err := time.LoadLocation(in.Timezone); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a known timezone", in.Timezone))
			return
		}
	}

	ctx, cancel := reqContext(r, 10*time.Second)
	defer cancel()

	saved, err := s.deps.Settings.UpdateSettings(ctx, in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the settings", err.Error())
		return
	}
	s.log.Info("settings updated via the companion app",
		"timezone", saved.Timezone, "clock_24h", saved.Clock24h,
		"brightness", saved.DisplayBrightness, "default_sound", saved.DefaultSoundID)
	writeJSON(w, http.StatusOK, saved)
}

// --- networking --------------------------------------------------------------

func (s *Server) handleWiFiStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r, 10*time.Second)
	defer cancel()

	st, err := s.deps.WiFi.Status(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "the network helper is unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleWiFiScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r, 30*time.Second)
	defer cancel()

	nets, err := s.deps.WiFi.Scan(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not scan for networks", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": nets})
}

func (s *Server) handleEnterSetup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r, 60*time.Second)
	defer cancel()

	s.log.Warn("Wi-Fi setup mode requested from the companion app")
	if err := s.deps.WiFi.EnterSetupMode(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not start setup mode", err.Error())
		return
	}
	st, _ := s.deps.WiFi.Status(ctx)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleExitSetup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r, 60*time.Second)
	defer cancel()

	if err := s.deps.WiFi.ExitSetupMode(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not leave setup mode", err.Error())
		return
	}
	st, _ := s.deps.WiFi.Status(ctx)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleWiFiConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SSID       string `json:"ssid"`
		Passphrase string `json:"passphrase"`
		Hidden     bool   `json:"hidden"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := wifi.ValidateCredentials(body.SSID, body.Passphrase); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := reqContext(r, 2*time.Minute)
	defer cancel()

	// The SSID is logged; the passphrase never is.
	s.log.Info("network change requested from the companion app", "ssid", body.SSID)

	result, err := s.deps.WiFi.Connect(ctx, body.SSID, body.Passphrase, body.Hidden)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
