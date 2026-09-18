package alarm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/system"
)

// fakeStore is an in-memory Store. It lives here rather than in internal/storage
// because storage imports this package.
type fakeStore struct {
	mu     sync.Mutex
	nextID int64
	items  map[int64]Alarm
	// saveErr, when set, makes every SaveAlarm fail, which exercises the rule that
	// persistence failures must not stop an alarm from ringing.
	saveErr error
	loadErr error
}

func newFakeStore(seed ...Alarm) *fakeStore {
	s := &fakeStore{items: map[int64]Alarm{}}
	for _, a := range seed {
		if _, err := s.SaveAlarm(a); err != nil {
			panic(err)
		}
	}
	return s
}

func (s *fakeStore) Alarms() ([]Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := make([]Alarm, 0, len(s.items))
	for _, a := range s.items {
		out = append(out, a)
	}
	SortAlarms(out)
	return out, nil
}

func (s *fakeStore) SaveAlarm(a Alarm) (Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return Alarm{}, s.saveErr
	}
	if a.ID == 0 {
		s.nextID++
		a.ID = s.nextID
	}
	s.items[a.ID] = a
	return a, nil
}

func (s *fakeStore) DeleteAlarm(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}

func (s *fakeStore) get(id int64) (Alarm, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.items[id]
	return a, ok
}

// recordingSink captures scheduler lifecycle callbacks.
type recordingSink struct {
	mu       sync.Mutex
	started  []Active
	stopped  []stopRecord
	snoozed  []Active
	schedule []*Upcoming
	notify   chan struct{}
}

type stopRecord struct {
	Active Active
	Reason StopReason
}

func newRecordingSink() *recordingSink {
	return &recordingSink{notify: make(chan struct{}, 128)}
}

func (r *recordingSink) AlarmStarted(a Active) {
	r.mu.Lock()
	r.started = append(r.started, a)
	r.mu.Unlock()
	r.ping()
}
func (r *recordingSink) AlarmStopped(a Active, reason StopReason) {
	r.mu.Lock()
	r.stopped = append(r.stopped, stopRecord{a, reason})
	r.mu.Unlock()
	r.ping()
}
func (r *recordingSink) AlarmSnoozed(a Active) {
	r.mu.Lock()
	r.snoozed = append(r.snoozed, a)
	r.mu.Unlock()
	r.ping()
}
func (r *recordingSink) ScheduleChanged(u *Upcoming) {
	r.mu.Lock()
	r.schedule = append(r.schedule, u)
	r.mu.Unlock()
	r.ping()
}
func (r *recordingSink) ping() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *recordingSink) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (r *recordingSink) stops() []stopRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]stopRecord(nil), r.stopped...)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newScheduler wires a scheduler with a fake clock and store, evaluated manually
// (no Run goroutine) so every test is deterministic.
func newScheduler(t *testing.T, now time.Time, seed ...Alarm) (*Scheduler, *fakeStore, *recordingSink, *system.FakeClock) {
	t.Helper()
	store := newFakeStore(seed...)
	clk := system.NewFakeClock(now)
	sink := newRecordingSink()
	cfg := DefaultConfig()
	cfg.Location = time.UTC
	cfg.DefaultSound = "alarm1"
	s := NewScheduler(cfg, store, clk, testLogger(), sink)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s, store, sink, clk
}

func enabledDaily(hour, minute int) Alarm {
	a := New(hour, minute)
	a.RepeatDays = EveryDay
	a.Label = "Test"
	return a
}

func TestSchedulerFiresAtTheScheduledMinute(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	s, _, sink, clk := newScheduler(t, start, enabledDaily(6, 30))

	s.evaluate()
	if sink.startCount() != 0 {
		t.Fatal("alarm fired early")
	}

	clk.SetNow(time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC))
	s.evaluate()

	if sink.startCount() != 1 {
		t.Fatalf("alarm did not fire: %d starts", sink.startCount())
	}
	active := s.Active()
	if active == nil || active.State != StateRinging {
		t.Fatalf("active: %+v", active)
	}
	if active.SoundID != "alarm1" {
		t.Errorf("default sound not applied: %q", active.SoundID)
	}
	if !s.IsRinging() {
		t.Error("IsRinging should be true")
	}

	// Re-evaluating must not start it a second time.
	s.evaluate()
	if sink.startCount() != 1 {
		t.Errorf("alarm re-fired: %d starts", sink.startCount())
	}
}

func TestSchedulerDismissStopsTheAlarm(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, _, sink, _ := newScheduler(t, start, enabledDaily(6, 30))
	s.evaluate()

	if err := s.Dismiss(); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if s.Active() != nil {
		t.Error("alarm still active after dismissal")
	}
	stops := sink.stops()
	if len(stops) != 1 || stops[0].Reason != ReasonDismissed {
		t.Fatalf("stops: %+v", stops)
	}

	// Pressing the big red button again is harmless.
	if err := s.Dismiss(); !errors.Is(err, ErrNoneActive) {
		t.Errorf("second dismiss: %v", err)
	}
}

func TestSchedulerDoesNotRefireAfterDismissalWithinTheSameMinute(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, _, sink, clk := newScheduler(t, start, enabledDaily(6, 30))
	s.evaluate()
	_ = s.Dismiss()

	// Ten seconds later the occurrence is still "within the catch-up window", but
	// LastFired must prevent it from ringing again.
	clk.Advance(10 * time.Second)
	s.evaluate()
	clk.Advance(2 * time.Minute)
	s.evaluate()

	if got := sink.startCount(); got != 1 {
		t.Fatalf("alarm restarted after dismissal: %d starts", got)
	}
}

func TestSchedulerFiresAgainTheNextDay(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, _, sink, clk := newScheduler(t, start, enabledDaily(6, 30))
	s.evaluate()
	_ = s.Dismiss()

	clk.SetNow(time.Date(2026, 9, 19, 6, 30, 0, 0, time.UTC))
	s.evaluate()
	if got := sink.startCount(); got != 2 {
		t.Fatalf("recurring alarm did not fire the next day: %d starts", got)
	}
}

func TestSchedulerOneShotDisablesItself(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 29, 0, 0, time.UTC)
	one := New(6, 30)
	one.Label = "Once"
	s, store, sink, clk := newScheduler(t, start, one)

	clk.SetNow(time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC))
	s.evaluate()
	if sink.startCount() != 1 {
		t.Fatalf("one-shot did not fire")
	}
	_ = s.Dismiss()

	stored, ok := store.get(1)
	if !ok {
		t.Fatal("alarm vanished")
	}
	if stored.Enabled {
		t.Error("a one-shot alarm must disable itself after firing")
	}

	clk.SetNow(time.Date(2026, 9, 19, 6, 30, 0, 0, time.UTC))
	s.evaluate()
	if sink.startCount() != 1 {
		t.Errorf("disabled one-shot fired again: %d starts", sink.startCount())
	}
}

func TestSchedulerCatchUpAfterPowerLoss(t *testing.T) {
	// The Pi was off across 06:30 and boots at 06:33.
	boot := time.Date(2026, 9, 18, 6, 33, 0, 0, time.UTC)
	s, _, sink, _ := newScheduler(t, boot, enabledDaily(6, 30))
	s.evaluate()

	if sink.startCount() != 1 {
		t.Fatal("an alarm missed by three minutes should still ring")
	}
	if got := s.Active().ScheduledAt; got.Minute() != 30 {
		t.Errorf("scheduled instant should be the original 06:30, got %v", got)
	}
}

func TestSchedulerSkipsAlarmsMissedByTooLong(t *testing.T) {
	// The Pi was unplugged overnight and boots at lunchtime.
	boot := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	s, _, sink, _ := newScheduler(t, boot, enabledDaily(6, 30))
	s.evaluate()
	if sink.startCount() != 0 {
		t.Fatal("an alarm missed by hours must not ring at lunchtime")
	}
}

func TestSchedulerSnoozeAndRering(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	a := enabledDaily(6, 30)
	a.SnoozeMinutes = 9
	s, _, sink, clk := newScheduler(t, start, a)
	s.evaluate()

	if err := s.Snooze(); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	active := s.Active()
	if active == nil || active.State != StateSnoozed {
		t.Fatalf("active: %+v", active)
	}
	if s.IsRinging() {
		t.Error("a snoozed alarm must not be ringing")
	}
	if want := start.Add(9 * time.Minute); !active.SnoozeUntil.Equal(want) {
		t.Errorf("snooze until: got %v want %v", active.SnoozeUntil, want)
	}

	// Snoozing twice in a row is rejected (it is already snoozed).
	if err := s.Snooze(); !errors.Is(err, ErrNotRinging) {
		t.Errorf("double snooze: %v", err)
	}

	clk.Advance(9 * time.Minute)
	s.evaluate()

	active = s.Active()
	if active == nil || active.State != StateRinging {
		t.Fatalf("alarm did not re-ring: %+v", active)
	}
	if active.SnoozeCount != 1 {
		t.Errorf("snooze count: %d", active.SnoozeCount)
	}
	if sink.startCount() != 2 {
		t.Errorf("starts: %d", sink.startCount())
	}
}

func TestSchedulerSnoozeDisabled(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	a := enabledDaily(6, 30)
	a.SnoozeMinutes = 0
	// Normalize would restore the default, so write it straight into the store.
	store := newFakeStore()
	a.SnoozeMinutes = 0
	if _, err := store.SaveAlarm(a); err != nil {
		t.Fatal(err)
	}
	clk := system.NewFakeClock(start)
	s := NewScheduler(Config{Location: time.UTC, CatchUpWindow: 5 * time.Minute, Tick: time.Second}, store, clk, testLogger(), newRecordingSink())
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	s.evaluate()
	if s.Active() == nil {
		t.Fatal("alarm did not fire")
	}
	if err := s.Snooze(); err == nil {
		t.Error("snooze should be rejected when the interval is zero")
	}
}

func TestSchedulerSnoozeWithNothingActive(t *testing.T) {
	s, _, _, _ := newScheduler(t, time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC))
	if err := s.Snooze(); !errors.Is(err, ErrNoneActive) {
		t.Errorf("got %v", err)
	}
}

func TestSchedulerAutoStops(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	a := enabledDaily(6, 30)
	a.AutoStopMinutes = 15
	s, _, sink, clk := newScheduler(t, start, a)
	s.evaluate()

	clk.Advance(14 * time.Minute)
	s.evaluate()
	if s.Active() == nil {
		t.Fatal("alarm stopped too early")
	}

	clk.Advance(2 * time.Minute)
	s.evaluate()
	if s.Active() != nil {
		t.Fatal("alarm did not auto-stop")
	}
	stops := sink.stops()
	if stops[len(stops)-1].Reason != ReasonAutoStop {
		t.Errorf("reason: %v", stops[len(stops)-1].Reason)
	}
}

func TestSchedulerOnlyOneAlarmRingsAtATime(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, store, sink, _ := newScheduler(t, start, enabledDaily(6, 30), enabledDaily(6, 30))
	s.evaluate()

	if got := sink.startCount(); got != 1 {
		t.Fatalf("two alarms rang at once: %d starts", got)
	}
	// The skipped alarm is still marked fired so it does not ring late.
	skipped, _ := store.get(2)
	if skipped.LastFired.IsZero() {
		t.Error("the skipped alarm should still be marked fired")
	}
}

func TestSchedulerDisablingARingingAlarmStopsIt(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, _, sink, _ := newScheduler(t, start, enabledDaily(6, 30))
	s.evaluate()

	if _, err := s.SetEnabled(1, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if s.Active() != nil {
		t.Error("disabling a ringing alarm must silence it")
	}
	if got := sink.stops()[0].Reason; got != ReasonDeleted {
		t.Errorf("reason: %v", got)
	}
}

func TestSchedulerDeletingARingingAlarmStopsIt(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, _, _, _ := newScheduler(t, start, enabledDaily(6, 30))
	s.evaluate()

	if err := s.Delete(1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Active() != nil {
		t.Error("alarm still ringing after deletion")
	}
	if _, err := s.Get(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	if err := s.Delete(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
}

func TestSchedulerRingsDespitePersistenceFailure(t *testing.T) {
	// The SD card has gone read-only. The alarm must still ring.
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, store, sink, _ := newScheduler(t, start, enabledDaily(6, 30))
	store.mu.Lock()
	store.saveErr = errors.New("disk is read-only")
	store.mu.Unlock()

	s.evaluate()
	if sink.startCount() != 1 {
		t.Fatal("a storage failure stopped the alarm from ringing")
	}
	// And the in-memory copy is still updated, so it does not ring in a loop.
	s.evaluate()
	if sink.startCount() != 1 {
		t.Errorf("alarm re-fired after a storage failure: %d starts", sink.startCount())
	}
}

func TestSchedulerNextReportsTheSoonestAlarm(t *testing.T) {
	start := time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC)
	s, _, _, _ := newScheduler(t, start, enabledDaily(7, 0), enabledDaily(6, 30))
	s.evaluate()

	next := s.Next()
	if next == nil {
		t.Fatal("no next alarm")
	}
	if want := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC); !next.At.Equal(want) {
		t.Errorf("next: got %v want %v", next.At, want)
	}
}

func TestSchedulerNextIsNilWithNoEnabledAlarms(t *testing.T) {
	start := time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC)
	a := enabledDaily(6, 30)
	a.Enabled = false
	s, _, _, _ := newScheduler(t, start, a)
	s.evaluate()
	if s.Next() != nil {
		t.Errorf("next: %+v", s.Next())
	}
}

func TestSchedulerTriggerFiresImmediately(t *testing.T) {
	start := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	s, _, sink, _ := newScheduler(t, start, enabledDaily(6, 30))

	if err := s.Trigger(1); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if sink.startCount() != 1 || s.Active() == nil {
		t.Fatal("Trigger did not start the alarm")
	}
	if err := s.Trigger(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("Trigger unknown: %v", err)
	}
}

func TestSchedulerSetLocationRescheduling(t *testing.T) {
	start := time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC)
	s, _, _, _ := newScheduler(t, start, enabledDaily(6, 30))
	s.evaluate()
	utcNext := s.Next().At

	loc := nyc(t)
	s.SetLocation(loc)
	s.evaluate()

	if s.Location().String() != "America/New_York" {
		t.Errorf("location: %v", s.Location())
	}
	if s.Next().At.Equal(utcNext) {
		t.Error("changing timezone should change the next occurrence instant")
	}
}

func TestSchedulerSaveValidates(t *testing.T) {
	s, _, _, _ := newScheduler(t, time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC))
	if _, err := s.Save(Alarm{Hour: 99}); err == nil {
		t.Error("an invalid alarm must be rejected before it reaches the store")
	}
}

func TestSchedulerLoadSurfacesStoreErrors(t *testing.T) {
	store := newFakeStore()
	store.loadErr = errors.New("database is locked")
	s := NewScheduler(DefaultConfig(), store, system.NewFakeClock(time.Now()), testLogger(), nil)
	if err := s.Load(); err == nil {
		t.Error("Load should surface store errors")
	}
}

func TestSchedulerNilSinkIsSafe(t *testing.T) {
	store := newFakeStore(enabledDaily(6, 30))
	clk := system.NewFakeClock(time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC))
	cfg := DefaultConfig()
	cfg.Location = time.UTC
	s := NewScheduler(cfg, store, clk, testLogger(), nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	s.evaluate()
	_ = s.Dismiss()
	// Reaching here without a nil dereference is the assertion.
}

func TestSchedulerRunStopsOnContextCancel(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	s, _, sink, _ := newScheduler(t, start, enabledDaily(6, 30))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait for the alarm to fire, proving Run evaluates on entry.
	select {
	case <-sink.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not evaluate")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}

	// Shutdown must release a ringing alarm so the sink can silence the speaker.
	found := false
	for _, s := range sink.stops() {
		if s.Reason == ReasonShutdown {
			found = true
		}
	}
	if !found {
		t.Error("shutdown did not stop the active alarm")
	}
}

func TestSchedulerSleepDurationNeverBusyLoops(t *testing.T) {
	start := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	s, _, _, _ := newScheduler(t, start, enabledDaily(6, 30))
	s.evaluate()

	// The next alarm is 30 minutes out but the tick is 30 seconds, so the tick wins.
	if got := s.sleepDuration(); got > 30*time.Second || got < 10*time.Millisecond {
		t.Errorf("sleep duration: %v", got)
	}

	// With a deadline in the past, the floor applies rather than a zero wait.
	s.mu.Lock()
	s.next = &Upcoming{At: start.Add(-time.Hour)}
	s.mu.Unlock()
	if got := s.sleepDuration(); got < 10*time.Millisecond {
		t.Errorf("past deadline produced %v", got)
	}
}
