package alarm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/system"
)

// Store is the persistence the scheduler needs. It is deliberately narrow so the
// scheduler can be tested against an in-memory implementation.
type Store interface {
	Alarms() ([]Alarm, error)
	SaveAlarm(a Alarm) (Alarm, error)
	DeleteAlarm(id int64) error
}

// State is the lifecycle state of a ringing alarm.
type State string

const (
	// StateRinging means audio should be playing.
	StateRinging State = "ringing"
	// StateSnoozed means the alarm is quiet but will ring again.
	StateSnoozed State = "snoozed"
)

// StopReason explains why an active alarm ended, which is useful in logs when
// someone asks "why did it stop by itself?".
type StopReason string

const (
	// ReasonDismissed is the big red button, or the equivalent API call.
	ReasonDismissed StopReason = "dismissed"
	// ReasonAutoStop is the auto-stop timeout expiring.
	ReasonAutoStop StopReason = "auto-stop"
	// ReasonSuperseded is another alarm taking over.
	ReasonSuperseded StopReason = "superseded"
	// ReasonShutdown is the daemon stopping.
	ReasonShutdown StopReason = "shutdown"
	// ReasonDeleted is the alarm being deleted or disabled while ringing.
	ReasonDeleted StopReason = "deleted"
)

// Active describes the currently ringing or snoozed alarm.
type Active struct {
	AlarmID     int64     `json:"alarm_id"`
	Label       string    `json:"label"`
	SoundID     string    `json:"sound_id"`
	State       State     `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	ScheduledAt time.Time `json:"scheduled_at"`
	SnoozeCount int       `json:"snooze_count"`
	// SnoozeUntil is set while State is StateSnoozed.
	SnoozeUntil time.Time `json:"snooze_until,omitzero"`
	// AutoStopAt is when an undismissed alarm gives up.
	AutoStopAt time.Time `json:"auto_stop_at,omitzero"`
}

// Sink receives alarm lifecycle notifications. The scheduler calls it from its own
// goroutine; implementations must not block for long and must not call back into
// the scheduler synchronously.
//
// Keeping audio behind this interface rather than calling an AudioService directly
// is what lets the scheduler be tested with no audio subsystem at all, and what
// guarantees an audio failure cannot stall scheduling.
type Sink interface {
	// AlarmStarted is called when an alarm begins ringing, including after a snooze.
	AlarmStarted(Active)
	// AlarmStopped is called when an alarm stops for any reason.
	AlarmStopped(Active, StopReason)
	// AlarmSnoozed is called when a ringing alarm is snoozed.
	AlarmSnoozed(Active)
	// ScheduleChanged is called when the next-due time changes, so the UI can
	// update without polling.
	ScheduleChanged(next *Upcoming)
}

// Upcoming describes the next alarm that will ring.
type Upcoming struct {
	AlarmID int64     `json:"alarm_id"`
	Label   string    `json:"label"`
	At      time.Time `json:"at"`
}

// Config tunes the scheduler.
type Config struct {
	// Location is the timezone alarms are interpreted in.
	Location *time.Location
	// CatchUpWindow is how late an alarm may be and still ring. It covers a
	// reboot, a daemon restart, or a Pi that was briefly overloaded. Beyond it,
	// the occurrence is skipped so the Timeblaster does not start shouting at
	// lunchtime because it was unplugged overnight.
	CatchUpWindow time.Duration
	// Tick is the maximum time the scheduler will sleep before re-evaluating.
	// Re-evaluating periodically rather than trusting one long timer is what makes
	// the scheduler immune to NTP steps, timezone changes and DST transitions.
	Tick time.Duration
	// DefaultSound is used when an alarm names no sound, or names a missing one.
	DefaultSound string
}

// DefaultConfig returns the standard scheduler tuning.
func DefaultConfig() Config {
	return Config{
		Location:      time.Local,
		CatchUpWindow: 5 * time.Minute,
		Tick:          30 * time.Second,
	}
}

// Errors returned by scheduler operations.
var (
	ErrNotFound   = errors.New("alarm: not found")
	ErrNoneActive = errors.New("alarm: no alarm is active")
	ErrNotRinging = errors.New("alarm: the active alarm is not ringing")
)

// Scheduler owns alarm timing. It holds the alarm set in memory (the database is
// the durable copy) and re-evaluates on every tick, on every change, and whenever
// it is explicitly kicked.
type Scheduler struct {
	cfg   Config
	store Store
	clock system.Clock
	log   *slog.Logger
	sink  Sink

	mu     sync.RWMutex
	alarms map[int64]Alarm
	active *Active
	next   *Upcoming

	kick chan struct{}
}

// NewScheduler builds a scheduler. Load must be called (or Run started) before it
// knows about any alarms.
func NewScheduler(cfg Config, store Store, clock system.Clock, log *slog.Logger, sink Sink) *Scheduler {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.CatchUpWindow <= 0 {
		cfg.CatchUpWindow = 5 * time.Minute
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 30 * time.Second
	}
	return &Scheduler{
		cfg:    cfg,
		store:  store,
		clock:  clock,
		log:    log,
		sink:   sink,
		alarms: map[int64]Alarm{},
		kick:   make(chan struct{}, 1),
	}
}

// SetSink installs the lifecycle sink after construction, which the composition
// root needs because the audio service and the scheduler are built together.
func (s *Scheduler) SetSink(sink Sink) {
	s.mu.Lock()
	s.sink = sink
	s.mu.Unlock()
}

// SetLocation changes the timezone alarms are interpreted in and re-evaluates.
func (s *Scheduler) SetLocation(loc *time.Location) {
	if loc == nil {
		return
	}
	s.mu.Lock()
	s.cfg.Location = loc
	s.mu.Unlock()
	s.log.Info("alarm timezone changed", "timezone", loc.String())
	s.recomputeNext(s.clock.Now())
	s.Kick()
}

// Location reports the scheduler's timezone.
func (s *Scheduler) Location() *time.Location {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Location
}

// SetDefaultSound updates the fallback sound used by alarms that name none.
func (s *Scheduler) SetDefaultSound(id string) {
	s.mu.Lock()
	s.cfg.DefaultSound = id
	s.mu.Unlock()
}

// Load reads the alarm set from the store. It is safe to call at any time.
func (s *Scheduler) Load() error {
	list, err := s.store.Alarms()
	if err != nil {
		return fmt.Errorf("alarm: loading alarms: %w", err)
	}
	s.mu.Lock()
	s.alarms = make(map[int64]Alarm, len(list))
	for _, a := range list {
		s.alarms[a.ID] = a
	}
	s.mu.Unlock()

	s.log.Info("alarms loaded", "count", len(list))
	for _, a := range list {
		s.log.Debug("alarm definition", "alarm", a.String(), "sound", a.SoundID)
	}
	// Recompute synchronously so callers (the API, the companion app) can read
	// the next-due alarm the instant the set changes, rather than after the
	// scheduler's next evaluation tick.
	s.recomputeNext(s.clock.Now())
	s.Kick()
	return nil
}

// List returns the current alarm set, sorted for display.
func (s *Scheduler) List() []Alarm {
	s.mu.RLock()
	out := make([]Alarm, 0, len(s.alarms))
	for _, a := range s.alarms {
		out = append(out, a)
	}
	s.mu.RUnlock()
	SortAlarms(out)
	return out
}

// Get returns one alarm.
func (s *Scheduler) Get(id int64) (Alarm, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.alarms[id]
	if !ok {
		return Alarm{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	return a, nil
}

// Save creates or updates an alarm, persisting it before it becomes schedulable.
func (s *Scheduler) Save(a Alarm) (Alarm, error) {
	a = a.Normalize()
	if err := a.Validate(); err != nil {
		return Alarm{}, err
	}
	now := s.clock.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now

	saved, err := s.store.SaveAlarm(a)
	if err != nil {
		return Alarm{}, fmt.Errorf("alarm: saving: %w", err)
	}

	s.mu.Lock()
	_, existed := s.alarms[saved.ID]
	s.alarms[saved.ID] = saved
	active := s.active
	s.mu.Unlock()

	verb := "created"
	if existed {
		verb = "updated"
	}
	s.log.Info("alarm "+verb, "alarm", saved.String(), "label", saved.Label, "sound", saved.SoundID)

	// Disabling a ringing alarm must silence it, not leave it shouting.
	if active != nil && active.AlarmID == saved.ID && !saved.Enabled {
		s.stopActive(ReasonDeleted)
	}
	s.recomputeNext(s.clock.Now())
	s.Kick()
	return saved, nil
}

// Delete removes an alarm, stopping it first if it is currently ringing.
func (s *Scheduler) Delete(id int64) error {
	s.mu.RLock()
	_, ok := s.alarms[id]
	active := s.active
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: id %d", ErrNotFound, id)
	}

	if active != nil && active.AlarmID == id {
		s.stopActive(ReasonDeleted)
	}
	if err := s.store.DeleteAlarm(id); err != nil {
		return fmt.Errorf("alarm: deleting: %w", err)
	}

	s.mu.Lock()
	delete(s.alarms, id)
	s.mu.Unlock()

	s.log.Info("alarm deleted", "alarm_id", id)
	s.recomputeNext(s.clock.Now())
	s.Kick()
	return nil
}

// SetEnabled toggles an alarm.
func (s *Scheduler) SetEnabled(id int64, enabled bool) (Alarm, error) {
	a, err := s.Get(id)
	if err != nil {
		return Alarm{}, err
	}
	a.Enabled = enabled
	return s.Save(a)
}

// Active returns a copy of the currently ringing or snoozed alarm, or nil.
func (s *Scheduler) Active() *Active {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil
	}
	cp := *s.active
	return &cp
}

// IsRinging reports whether audio should currently be playing.
func (s *Scheduler) IsRinging() bool {
	a := s.Active()
	return a != nil && a.State == StateRinging
}

// Next returns the next scheduled alarm, or nil when nothing is scheduled.
func (s *Scheduler) Next() *Upcoming {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.next == nil {
		return nil
	}
	cp := *s.next
	return &cp
}

// Dismiss stops the active alarm. This is what the big red button calls.
//
// It is intentionally forgiving: dismissing when nothing is ringing returns
// ErrNoneActive rather than an internal error, because the button is pressed
// speculatively all the time.
func (s *Scheduler) Dismiss() error {
	if s.Active() == nil {
		return ErrNoneActive
	}
	s.stopActive(ReasonDismissed)
	return nil
}

// Snooze quiets a ringing alarm until its snooze interval elapses.
func (s *Scheduler) Snooze() error {
	s.mu.Lock()
	if s.active == nil {
		s.mu.Unlock()
		return ErrNoneActive
	}
	if s.active.State != StateRinging {
		s.mu.Unlock()
		return ErrNotRinging
	}
	a, ok := s.alarms[s.active.AlarmID]
	mins := DefaultSnoozeMinutes
	if ok && a.SnoozeMinutes > 0 {
		mins = a.SnoozeMinutes
	}
	if ok && a.SnoozeMinutes == 0 {
		// A snooze interval of zero means the user disabled snoozing.
		s.mu.Unlock()
		return fmt.Errorf("alarm: snooze is disabled for alarm %d", a.ID)
	}

	now := s.clock.Now()
	s.active.State = StateSnoozed
	s.active.SnoozeCount++
	s.active.SnoozeUntil = now.Add(time.Duration(mins) * time.Minute)
	snapshot := *s.active
	sink := s.sink
	s.mu.Unlock()

	s.log.Info("alarm snoozed",
		"alarm_id", snapshot.AlarmID, "minutes", mins,
		"until", snapshot.SnoozeUntil.Format(time.RFC3339), "snooze_count", snapshot.SnoozeCount)
	if sink != nil {
		sink.AlarmSnoozed(snapshot)
	}
	s.Kick()
	return nil
}

// Trigger starts an alarm immediately, bypassing the schedule. It backs the
// companion app's "test alarm" button and manual firing from tbctl.
func (s *Scheduler) Trigger(id int64) error {
	a, err := s.Get(id)
	if err != nil {
		return err
	}
	s.start(a, s.clock.Now(), false)
	return nil
}

// Kick asks the scheduler to re-evaluate now rather than waiting for the next
// tick. It never blocks.
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run drives the scheduler until the context is cancelled.
//
// The loop sleeps until the sooner of the next due instant and the next tick.
// Waking up at least once per tick is what makes the scheduler correct across
// clock steps: it never assumes that the duration it slept for matches the wall
// clock that elapsed.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("alarm scheduler started",
		"timezone", s.cfg.Location.String(),
		"catch_up_window", s.cfg.CatchUpWindow,
		"tick", s.cfg.Tick)

	for {
		s.evaluate()

		wait := s.sleepDuration()
		timer := s.clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			if s.Active() != nil {
				s.stopActive(ReasonShutdown)
			}
			s.log.Info("alarm scheduler stopped")
			return ctx.Err()
		case <-timer.C():
		case <-s.kick:
			timer.Stop()
		}
	}
}

// sleepDuration computes how long to wait before the next evaluation.
func (s *Scheduler) sleepDuration() time.Duration {
	now := s.clock.Now()
	wait := s.cfg.Tick

	s.mu.RLock()
	next := s.next
	active := s.active
	s.mu.RUnlock()

	consider := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if d := t.Sub(now); d > 0 && d < wait {
			wait = d
		}
	}
	if next != nil {
		consider(next.At)
	}
	if active != nil {
		consider(active.SnoozeUntil)
		consider(active.AutoStopAt)
	}
	// Never busy-loop, even if a deadline is in the past by a hair.
	if wait < 10*time.Millisecond {
		wait = 10 * time.Millisecond
	}
	return wait
}

// evaluate performs one pass: expire the active alarm if needed, fire anything
// due, and recompute the next occurrence.
func (s *Scheduler) evaluate() {
	now := s.clock.Now()

	s.mu.RLock()
	active := s.active
	loc := s.cfg.Location
	catchUp := s.cfg.CatchUpWindow
	alarms := make([]Alarm, 0, len(s.alarms))
	for _, a := range s.alarms {
		alarms = append(alarms, a)
	}
	s.mu.RUnlock()

	// 1. Handle the currently active alarm.
	if active != nil {
		switch active.State {
		case StateSnoozed:
			if !active.SnoozeUntil.IsZero() && !now.Before(active.SnoozeUntil) {
				if a, err := s.Get(active.AlarmID); err == nil {
					s.log.Info("snooze expired, alarm ringing again", "alarm_id", a.ID)
					s.start(a, now, true)
				} else {
					// The alarm was deleted while snoozed.
					s.stopActive(ReasonDeleted)
				}
			}
		case StateRinging:
			if !active.AutoStopAt.IsZero() && !now.Before(active.AutoStopAt) {
				s.log.Warn("alarm auto-stopped after ringing undismissed",
					"alarm_id", active.AlarmID, "rang_for", now.Sub(active.StartedAt).Round(time.Second))
				s.stopActive(ReasonAutoStop)
			}
		}
	}

	// 2. Fire anything due. Only one alarm can ring at a time; if two are due, the
	// earlier scheduled instant wins and the other is skipped (its LastFired is
	// still advanced so it does not fire late).
	SortAlarms(alarms)
	for _, a := range alarms {
		when, due := a.Due(now, loc, catchUp)
		if !due {
			continue
		}
		if cur := s.Active(); cur != nil {
			if cur.AlarmID == a.ID {
				continue // already ringing this one
			}
			s.log.Info("another alarm is already active; skipping",
				"skipped_alarm_id", a.ID, "active_alarm_id", cur.AlarmID)
			s.markFired(a, when)
			continue
		}
		late := now.Sub(when)
		if late > time.Second {
			s.log.Warn("firing a missed alarm within the catch-up window",
				"alarm_id", a.ID, "scheduled", when.Format(time.RFC3339), "late", late.Round(time.Second))
		}
		s.start(a, when, false)
	}

	// 3. Recompute the next occurrence for the UI and for sleep sizing.
	s.recomputeNext(now)
}

// start begins ringing. resume is true when this is a snooze re-ring, which must
// not reset the snooze count or re-stamp LastFired.
func (s *Scheduler) start(a Alarm, scheduled time.Time, resume bool) {
	now := s.clock.Now()

	s.mu.Lock()
	sound := a.SoundID
	if sound == "" {
		sound = s.cfg.DefaultSound
	}
	snoozeCount := 0
	if resume && s.active != nil {
		snoozeCount = s.active.SnoozeCount
	}
	autoStop := time.Time{}
	if a.AutoStopMinutes > 0 {
		autoStop = now.Add(time.Duration(a.AutoStopMinutes) * time.Minute)
	}
	s.active = &Active{
		AlarmID:     a.ID,
		Label:       a.Label,
		SoundID:     sound,
		State:       StateRinging,
		StartedAt:   now,
		ScheduledAt: scheduled,
		SnoozeCount: snoozeCount,
		AutoStopAt:  autoStop,
	}
	snapshot := *s.active
	sink := s.sink
	s.mu.Unlock()

	s.log.Info("alarm triggered",
		"alarm_id", a.ID, "label", a.Label, "sound", sound,
		"scheduled", scheduled.Format(time.RFC3339), "resumed_from_snooze", resume,
		"auto_stop_at", formatTime(autoStop))

	if !resume {
		s.markFired(a, scheduled)
	}
	if sink != nil {
		sink.AlarmStarted(snapshot)
	}
}

// markFired records that an occurrence has been handled, disabling one-shot
// alarms. A persistence failure here is logged but must never stop the alarm from
// ringing, so the in-memory copy is updated regardless.
func (s *Scheduler) markFired(a Alarm, when time.Time) {
	a.LastFired = when
	if !a.Repeats() {
		a.Enabled = false
	}
	a.UpdatedAt = s.clock.Now()

	saved, err := s.store.SaveAlarm(a)
	if err != nil {
		s.log.Error("could not persist alarm fire state; continuing in memory",
			"alarm_id", a.ID, "error", err)
		saved = a
	}
	s.mu.Lock()
	s.alarms[saved.ID] = saved
	s.mu.Unlock()
}

// stopActive clears the active alarm and notifies the sink.
func (s *Scheduler) stopActive(reason StopReason) {
	s.mu.Lock()
	if s.active == nil {
		s.mu.Unlock()
		return
	}
	snapshot := *s.active
	s.active = nil
	sink := s.sink
	s.mu.Unlock()

	s.log.Info("alarm stopped",
		"alarm_id", snapshot.AlarmID, "reason", string(reason),
		"rang_for", s.clock.Since(snapshot.StartedAt).Round(time.Second),
		"snooze_count", snapshot.SnoozeCount)
	if sink != nil {
		sink.AlarmStopped(snapshot, reason)
	}
	s.Kick()
}

// recomputeNext updates the cached next-due alarm and notifies on change.
func (s *Scheduler) recomputeNext(now time.Time) {
	s.mu.RLock()
	loc := s.cfg.Location
	alarms := make([]Alarm, 0, len(s.alarms))
	for _, a := range s.alarms {
		alarms = append(alarms, a)
	}
	prev := s.next
	s.mu.RUnlock()

	var best *Upcoming
	for _, a := range alarms {
		when, ok := a.NextOccurrence(now, loc)
		if !ok {
			continue
		}
		if best == nil || when.Before(best.At) {
			best = &Upcoming{AlarmID: a.ID, Label: a.Label, At: when}
		}
	}

	changed := (prev == nil) != (best == nil) ||
		(prev != nil && best != nil && (prev.AlarmID != best.AlarmID || !prev.At.Equal(best.At)))
	if !changed {
		return
	}

	s.mu.Lock()
	s.next = best
	sink := s.sink
	s.mu.Unlock()

	if best == nil {
		s.log.Info("no alarms scheduled")
	} else {
		s.log.Info("next alarm scheduled",
			"alarm_id", best.AlarmID, "at", best.At.Format(time.RFC3339),
			"in", best.At.Sub(now).Round(time.Second))
	}
	if sink != nil {
		sink.ScheduleChanged(best)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
