package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

// Service is the alarm audio interface the rest of the application uses.
type Service interface {
	// PlayAlarm starts the given sound on a loop on the alarm speaker.
	PlayAlarm(ctx context.Context, soundID string) error
	// StopAlarm stops whatever is playing.
	StopAlarm() error
	// TestAlarm plays a sound once for a bounded duration, for the companion
	// app's preview button. It refuses while a real alarm is ringing.
	TestAlarm(ctx context.Context, soundID string, d time.Duration) error
	// SetAlarmVolume sets the speaker volume, 0-100.
	SetAlarmVolume(ctx context.Context, pct int, physical bool) error
	// Volume reports the current volume and whether it came from the knob.
	Volume() (pct int, authoritative bool)
	// IsPlaying reports whether audio is currently coming out of the speaker.
	IsPlaying() bool
	// Sounds lists the available alarm sounds.
	Sounds() []Sound
	// Health describes the audio subsystem for the health endpoint.
	Health() Health
}

// Health describes the alarm audio path.
type Health struct {
	// DeviceAvailable is false when the USB speaker is unplugged.
	DeviceAvailable bool `json:"device_available"`
	// Device is the resolved ALSA device string.
	Device string `json:"device,omitempty"`
	// CardName is the human-readable card description.
	CardName string `json:"card_name,omitempty"`
	// MixerControl is the hardware volume control in use, empty when volume is
	// applied in software.
	MixerControl string `json:"mixer_control,omitempty"`
	// HardwareVolume is true when volume goes to the card's own mixer.
	HardwareVolume bool `json:"hardware_volume"`
	// SoundCount is how many playable sounds were found.
	SoundCount int `json:"sound_count"`
	// Playing reports current playback.
	Playing bool `json:"playing"`
	// VolumePercent is the current volume.
	VolumePercent int `json:"volume_percent"`
	// VolumeAuthoritative is true once the physical knob has reported in.
	VolumeAuthoritative bool `json:"volume_authoritative"`
	// LastError describes the most recent failure, empty when healthy.
	LastError string `json:"last_error,omitempty"`
}

// ErrBusy is returned when a test playback is requested during a real alarm.
var ErrBusy = errors.New("audio: an alarm is currently ringing")

// AlarmService is the production Service implementation. It is exported so the
// composition root can hold the concrete type and reach ResolveDevice and
// Library, which are lifecycle concerns rather than part of the API surface.
type AlarmService struct {
	cfg    config.Audio
	lib    *Library
	player Player
	probe  system.ALSAProbe
	runner system.CommandRunner
	clock  system.Clock
	log    *slog.Logger

	mu sync.Mutex
	// device state, re-resolved whenever the speaker is plugged back in
	card         system.ALSACard
	deviceString string
	mixerControl string
	deviceOK     bool
	lastProbe    time.Time
	lastErr      error

	// playback state
	handle    Handle
	playingID string
	isAlarm   bool

	// volume state. The physical potentiometer is authoritative: until it reports
	// in, volume is "provisional" and the startup value is used. Persisted volume
	// is deliberately never replayed over the knob.
	volume        int
	authoritative bool
}

// Deps are the collaborators an alarm service needs.
type Deps struct {
	Library *Library
	Player  Player
	Probe   system.ALSAProbe
	Runner  system.CommandRunner
	Clock   system.Clock
	Logger  *slog.Logger
}

// NewService builds the alarm audio service.
func NewService(cfg config.Audio, d Deps) *AlarmService {
	s := &AlarmService{
		cfg:    cfg,
		lib:    d.Library,
		player: d.Player,
		probe:  d.Probe,
		runner: d.Runner,
		clock:  d.Clock,
		log:    d.Logger,
		volume: clampPercent(cfg.StartupVolumePercent),
	}
	return s
}

// ResolveDevice finds the alarm speaker and its mixer control.
//
// It is called at startup and again whenever playback fails or the recheck
// interval elapses, so unplugging and replugging the speaker recovers by itself
// rather than requiring a service restart.
func (s *AlarmService) ResolveDevice(ctx context.Context) error {
	card, err := s.probe.Find(s.cfg.AlarmDevice)
	if err != nil {
		s.mu.Lock()
		s.deviceOK = false
		s.lastErr = err
		s.lastProbe = s.clock.Now()
		s.mu.Unlock()
		s.log.Error("alarm audio device unavailable; alarms will still fire but may be silent",
			"selector", s.cfg.AlarmDevice, "error", err)
		return err
	}

	control := s.cfg.MixerControl
	switch {
	case strings.EqualFold(control, "none"):
		control = ""
	case control == "":
		c, err := s.probe.MixerControl(ctx, card)
		if err != nil {
			// A card with no usable mixer is common on cheap USB speakers; fall
			// back to software volume rather than treating it as a failure.
			s.log.Info("could not query the card's mixer controls; using software volume",
				"card", card.ID, "error", err)
		}
		control = c
	}

	s.mu.Lock()
	changed := !s.deviceOK || s.card.ID != card.ID || s.mixerControl != control
	s.card = card
	s.deviceString = card.MPVAudioDevice()
	s.mixerControl = control
	s.deviceOK = true
	s.lastErr = nil
	s.lastProbe = s.clock.Now()
	vol := s.volume
	s.mu.Unlock()

	if changed {
		s.log.Info("alarm audio device resolved",
			"card_id", card.ID, "card_index", card.Index, "description", card.Description,
			"device", card.MPVAudioDevice(), "mixer_control", orNone(control))
	}
	// Re-apply the current volume so a freshly plugged-in speaker matches the knob.
	if control != "" {
		if err := s.applyHardwareVolume(ctx, vol); err != nil {
			s.log.Warn("could not set the hardware volume on the alarm device", "error", err)
		}
	}
	return nil
}

// PlayAlarm starts the alarm sound on a loop.
func (s *AlarmService) PlayAlarm(ctx context.Context, soundID string) error {
	return s.play(ctx, soundID, true, 0)
}

// TestAlarm previews a sound for a bounded duration.
func (s *AlarmService) TestAlarm(ctx context.Context, soundID string, d time.Duration) error {
	s.mu.Lock()
	busy := s.handle != nil && s.isAlarm
	s.mu.Unlock()
	if busy {
		return ErrBusy
	}
	if d <= 0 {
		d = s.cfg.TestDuration.Duration
	}
	return s.play(ctx, soundID, false, d)
}

func (s *AlarmService) play(ctx context.Context, soundID string, isAlarm bool, limit time.Duration) error {
	sound, reason, err := s.lib.Resolve(soundID, s.cfg.DefaultSoundID)
	if err != nil {
		s.recordError(err)
		s.log.Error("no alarm sound could be played", "requested", soundID, "error", err)
		return err
	}
	if reason != "" {
		s.log.Warn("substituting an alarm sound", "reason", reason, "playing", sound.ID)
	}

	// Stop anything already playing, including a test preview.
	_ = s.StopAlarm()

	// Re-resolve the device if it was missing or the recheck interval has passed,
	// so a speaker replugged since boot is picked up.
	s.mu.Lock()
	needProbe := !s.deviceOK ||
		(s.cfg.DeviceRecheckInterval.Duration > 0 &&
			s.clock.Since(s.lastProbe) > s.cfg.DeviceRecheckInterval.Duration)
	device, vol := s.deviceString, s.volume
	s.mu.Unlock()

	if needProbe {
		if err := s.ResolveDevice(ctx); err == nil {
			s.mu.Lock()
			device = s.deviceString
			s.mu.Unlock()
		}
	}

	handle, err := s.player.Play(ctx, sound.Path, PlayOptions{
		Device:        device,
		Loop:          isAlarm,
		VolumePercent: vol,
		ExtraArgs:     s.cfg.ExtraPlayerArgs,
	})
	if err != nil {
		s.recordError(err)
		s.log.Error("alarm playback failed to start",
			"sound", sound.ID, "path", sound.Path, "device", device, "error", err)
		return fmt.Errorf("audio: playing %s: %w", sound.ID, err)
	}

	s.mu.Lock()
	s.handle = handle
	s.playingID = sound.ID
	s.isAlarm = isAlarm
	s.lastErr = nil
	s.mu.Unlock()

	kind := "test"
	if isAlarm {
		kind = "alarm"
	}
	s.log.Info("alarm audio started",
		"kind", kind, "sound", sound.ID, "device", orAuto(device), "volume", vol, "loop", isAlarm)

	go s.watch(handle, sound.ID, isAlarm, limit)
	return nil
}

// watch cleans up after playback ends and bounds a test preview.
func (s *AlarmService) watch(h Handle, soundID string, isAlarm bool, limit time.Duration) {
	var limitC <-chan time.Time
	if limit > 0 {
		t := s.clock.NewTimer(limit)
		defer t.Stop()
		limitC = t.C()
	}

	select {
	case <-h.Done():
		err := h.Err()
		s.mu.Lock()
		current := s.handle == h
		if current {
			s.handle, s.playingID, s.isAlarm = nil, "", false
		}
		if err != nil {
			s.lastErr = err
		}
		s.mu.Unlock()

		if err != nil {
			// A player that dies mid-alarm is a real problem: log it loudly. The
			// scheduler still holds the alarm active, so the big red button keeps
			// working and the caller can retry.
			s.log.Error("alarm player exited unexpectedly",
				"sound", soundID, "was_alarm", isAlarm, "error", err)
		} else if current {
			s.log.Info("alarm audio finished", "sound", soundID)
		}
	case <-limitC:
		s.log.Debug("test playback reached its time limit", "sound", soundID)
		_ = h.Stop()
	}
}

// StopAlarm stops playback.
func (s *AlarmService) StopAlarm() error {
	s.mu.Lock()
	h, id := s.handle, s.playingID
	s.handle, s.playingID, s.isAlarm = nil, "", false
	s.mu.Unlock()

	if h == nil {
		return nil
	}
	if err := h.Stop(); err != nil {
		s.log.Warn("could not stop the alarm player cleanly", "sound", id, "error", err)
		return err
	}
	s.log.Info("alarm audio stopped", "sound", id)
	return nil
}

// IsPlaying reports whether audio is currently playing.
func (s *AlarmService) IsPlaying() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handle != nil
}

// SetAlarmVolume sets the speaker volume.
//
// The conflict-resolution policy between the knob and the companion app lives
// here: a physical change always wins and marks the value authoritative; a
// software change is rejected unless audio.allow_software_volume is set, and even
// then it is overridden by the next movement of the knob.
func (s *AlarmService) SetAlarmVolume(ctx context.Context, pct int, physical bool) error {
	pct = clampPercent(pct)

	s.mu.Lock()
	if !physical && !s.cfg.AllowSoftwareVolume {
		s.mu.Unlock()
		return errors.New("audio: software volume control is disabled; the volume knob is authoritative")
	}
	if physical {
		s.authoritative = true
	}
	unchanged := s.volume == pct
	s.volume = pct
	control, card, handle := s.mixerControl, s.card, s.handle
	s.mu.Unlock()

	if unchanged {
		return nil
	}

	source := "app"
	if physical {
		source = "knob"
	}
	s.log.Info("alarm volume set", "percent", pct, "source", source,
		"path", volumePath(control))

	if control != "" {
		if err := s.applyHardwareVolumeTo(ctx, card, control, pct); err != nil {
			s.recordError(err)
			return err
		}
		return nil
	}
	// No hardware mixer: adjust whatever is currently playing, and the next
	// playback picks the value up from its --volume argument.
	if handle != nil {
		if err := handle.SetVolume(pct); err != nil {
			s.recordError(err)
			return fmt.Errorf("audio: setting software volume: %w", err)
		}
	}
	return nil
}

func (s *AlarmService) applyHardwareVolume(ctx context.Context, pct int) error {
	s.mu.Lock()
	card, control := s.card, s.mixerControl
	s.mu.Unlock()
	if control == "" {
		return nil
	}
	return s.applyHardwareVolumeTo(ctx, card, control, pct)
}

func (s *AlarmService) applyHardwareVolumeTo(ctx context.Context, card system.ALSACard, control string, pct int) error {
	if s.runner == nil {
		return errors.New("audio: no command runner configured")
	}
	args := system.AmixerSetArgs(card.Index, control, pct)
	if _, err := s.runner.Run(ctx, "amixer", args...); err != nil {
		return fmt.Errorf("audio: setting hardware volume on card %d: %w", card.Index, err)
	}
	return nil
}

// Volume reports the current volume.
func (s *AlarmService) Volume() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volume, s.authoritative
}

// Sounds lists the available alarm sounds.
func (s *AlarmService) Sounds() []Sound { return s.lib.Sounds() }

// Library exposes the sound library for refresh and lookup.
func (s *AlarmService) Library() *Library { return s.lib }

// Health describes the audio subsystem.
func (s *AlarmService) Health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := Health{
		DeviceAvailable:     s.deviceOK,
		Device:              s.deviceString,
		CardName:            s.card.Description,
		MixerControl:        s.mixerControl,
		HardwareVolume:      s.mixerControl != "",
		SoundCount:          len(s.lib.Sounds()),
		Playing:             s.handle != nil,
		VolumePercent:       s.volume,
		VolumeAuthoritative: s.authoritative,
	}
	if s.lastErr != nil {
		h.LastError = s.lastErr.Error()
	}
	return h
}

func (s *AlarmService) recordError(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

func orNone(s string) string {
	if s == "" {
		return "(none: using software volume)"
	}
	return s
}

func orAuto(s string) string {
	if s == "" {
		return "(system default)"
	}
	return s
}

func volumePath(control string) string {
	if control == "" {
		return "software"
	}
	return "hardware:" + control
}

var _ Service = (*AlarmService)(nil)
