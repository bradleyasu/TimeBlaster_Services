package alarm

import (
	"testing"
	"time"
)

// A zone with a real DST rule, used to prove wall-clock alarms survive the
// transitions. America/New_York springs forward 2026-03-08 and falls back
// 2026-11-01.
func nyc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func TestValidate(t *testing.T) {
	good := New(6, 30)
	if err := good.Validate(); err != nil {
		t.Fatalf("valid alarm rejected: %v", err)
	}

	tests := []struct {
		name  string
		alarm Alarm
	}{
		{"hour too large", Alarm{Hour: 24}},
		{"negative hour", Alarm{Hour: -1}},
		{"minute too large", Alarm{Hour: 6, Minute: 60}},
		{"repeat mask out of range", Alarm{Hour: 6, RepeatDays: 1 << 8}},
		{"negative repeat mask", Alarm{Hour: 6, RepeatDays: -1}},
		{"snooze too long", Alarm{Hour: 6, SnoozeMinutes: 61}},
		{"auto stop too long", Alarm{Hour: 6, AutoStopMinutes: MaxAutoStopMinutes + 1}},
		{"label too long", Alarm{Hour: 6, Label: string(make([]byte, 65))}},
		{"bad one-shot date", Alarm{Hour: 6, OneShotDate: "tomorrow"}},
		{"one-shot date with repeats", Alarm{Hour: 6, RepeatDays: Monday, OneShotDate: "2026-09-20"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.alarm.Validate(); err == nil {
				t.Errorf("%+v should be invalid", tc.alarm)
			}
		})
	}
}

func TestNormalizeAppliesDefaults(t *testing.T) {
	got := Alarm{Hour: 7, Label: "  Wake up  ", SoundID: " alarm1 "}.Normalize()
	if got.Label != "Wake up" || got.SoundID != "alarm1" {
		t.Errorf("trimming: %+v", got)
	}
	if got.SnoozeMinutes != DefaultSnoozeMinutes {
		t.Errorf("snooze default: %d", got.SnoozeMinutes)
	}
	if got.AutoStopMinutes != DefaultAutoStopMinutes {
		t.Errorf("auto-stop default: %d", got.AutoStopMinutes)
	}
}

func TestRepeatsOn(t *testing.T) {
	a := Alarm{RepeatDays: Weekdays}
	for d, want := range map[time.Weekday]bool{
		time.Sunday: false, time.Monday: true, time.Friday: true, time.Saturday: false,
	} {
		if got := a.RepeatsOn(d); got != want {
			t.Errorf("RepeatsOn(%v) = %v want %v", d, got, want)
		}
	}
	if !a.Repeats() {
		t.Error("weekday alarm should repeat")
	}
	if (Alarm{}).Repeats() {
		t.Error("empty mask should not repeat")
	}
}

func TestRepeatString(t *testing.T) {
	for _, tc := range []struct {
		alarm Alarm
		want  string
	}{
		{Alarm{RepeatDays: NoRepeat}, "once"},
		{Alarm{RepeatDays: NoRepeat, OneShotDate: "2026-09-20"}, "once on 2026-09-20"},
		{Alarm{RepeatDays: EveryDay}, "every day"},
		{Alarm{RepeatDays: Weekdays}, "weekdays"},
		{Alarm{RepeatDays: Weekends}, "weekends"},
		{Alarm{RepeatDays: Monday | Wednesday}, "Mon,Wed"},
	} {
		if got := tc.alarm.RepeatString(); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

func TestNextOccurrenceOneShot(t *testing.T) {
	loc := time.UTC
	a := New(7, 0)

	// Before the time today => today.
	now := time.Date(2026, 9, 18, 6, 0, 0, 0, loc)
	got, ok := a.NextOccurrence(now, loc)
	if !ok || !got.Equal(time.Date(2026, 9, 18, 7, 0, 0, 0, loc)) {
		t.Fatalf("got %v, %v", got, ok)
	}

	// After the time today => tomorrow.
	now = time.Date(2026, 9, 18, 8, 0, 0, 0, loc)
	got, ok = a.NextOccurrence(now, loc)
	if !ok || !got.Equal(time.Date(2026, 9, 19, 7, 0, 0, 0, loc)) {
		t.Fatalf("got %v, %v", got, ok)
	}

	// Exactly at the time => the next one, never the instant we are already at.
	now = time.Date(2026, 9, 18, 7, 0, 0, 0, loc)
	got, _ = a.NextOccurrence(now, loc)
	if !got.Equal(time.Date(2026, 9, 19, 7, 0, 0, 0, loc)) {
		t.Fatalf("got %v", got)
	}
}

func TestNextOccurrenceOneShotWithDate(t *testing.T) {
	loc := time.UTC
	a := New(7, 0)
	a.OneShotDate = "2026-09-25"

	now := time.Date(2026, 9, 18, 6, 0, 0, 0, loc)
	got, ok := a.NextOccurrence(now, loc)
	if !ok || !got.Equal(time.Date(2026, 9, 25, 7, 0, 0, 0, loc)) {
		t.Fatalf("got %v, %v", got, ok)
	}

	// Once the date has passed there is no next occurrence at all.
	past := time.Date(2026, 9, 26, 0, 0, 0, 0, loc)
	if _, ok := a.NextOccurrence(past, loc); ok {
		t.Error("a past one-shot date should have no next occurrence")
	}

	// A malformed date is treated as unschedulable rather than panicking.
	bad := New(7, 0)
	bad.OneShotDate = "nonsense"
	if _, ok := bad.NextOccurrence(now, loc); ok {
		t.Error("malformed date should not schedule")
	}
}

func TestNextOccurrenceRecurring(t *testing.T) {
	loc := time.UTC
	a := New(6, 30)
	a.RepeatDays = Weekdays

	// Friday evening => Monday morning.
	friday := time.Date(2026, 9, 18, 20, 0, 0, 0, loc)
	if friday.Weekday() != time.Friday {
		t.Fatalf("test fixture is not a Friday: %v", friday.Weekday())
	}
	got, ok := a.NextOccurrence(friday, loc)
	if !ok || !got.Equal(time.Date(2026, 9, 21, 6, 30, 0, 0, loc)) {
		t.Fatalf("got %v, %v", got, ok)
	}

	// Monday before the time => the same morning.
	monday := time.Date(2026, 9, 21, 5, 0, 0, 0, loc)
	got, _ = a.NextOccurrence(monday, loc)
	if !got.Equal(time.Date(2026, 9, 21, 6, 30, 0, 0, loc)) {
		t.Fatalf("got %v", got)
	}
}

func TestNextOccurrenceDisabledOrNoLocation(t *testing.T) {
	a := New(7, 0)
	a.Enabled = false
	if _, ok := a.NextOccurrence(time.Now(), time.UTC); ok {
		t.Error("a disabled alarm should never schedule")
	}
	a.Enabled = true
	if _, ok := a.NextOccurrence(time.Now(), nil); ok {
		t.Error("a nil location should never schedule")
	}
}

func TestNextOccurrenceSurvivesSpringForward(t *testing.T) {
	loc := nyc(t)
	a := New(7, 0)
	a.RepeatDays = EveryDay

	// 2026-03-08 is the spring-forward day in America/New_York.
	before := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	got, ok := a.NextOccurrence(before, loc)
	if !ok {
		t.Fatal("no occurrence")
	}
	// The alarm must still be at 07:00 wall-clock on the transition day, which
	// means a 23-hour gap rather than a drift to 06:00 or 08:00.
	if h, m := got.Hour(), got.Minute(); h != 7 || m != 0 {
		t.Errorf("wall-clock time drifted across DST: %v", got)
	}
	if got.Day() != 8 {
		t.Errorf("expected the 8th, got %v", got)
	}
}

func TestNextOccurrenceHandlesNonexistentWallClockTime(t *testing.T) {
	loc := nyc(t)
	// 02:30 does not exist on 2026-03-08: the clock jumps 02:00 -> 03:00.
	a := New(2, 30)
	a.RepeatDays = EveryDay

	got, ok := a.NextOccurrence(time.Date(2026, 3, 8, 0, 0, 0, 0, loc), loc)
	if !ok {
		t.Fatal("an alarm in the skipped hour must still schedule")
	}
	// time.Date normalises it forward; the alarm rings once, not never.
	if got.Day() != 8 {
		t.Errorf("got %v", got)
	}
	if got.Before(time.Date(2026, 3, 8, 0, 0, 0, 0, loc)) {
		t.Errorf("occurrence went backwards: %v", got)
	}
}

func TestNextOccurrenceSurvivesFallBack(t *testing.T) {
	loc := nyc(t)
	a := New(7, 0)
	a.RepeatDays = EveryDay

	// 2026-11-01 is the fall-back day.
	got, ok := a.NextOccurrence(time.Date(2026, 10, 31, 12, 0, 0, 0, loc), loc)
	if !ok {
		t.Fatal("no occurrence")
	}
	if h := got.Hour(); h != 7 {
		t.Errorf("wall-clock time drifted across DST: %v", got)
	}
	if got.Day() != 1 || got.Month() != time.November {
		t.Errorf("got %v", got)
	}
}

func TestDueWithinCatchUpWindow(t *testing.T) {
	loc := time.UTC
	a := New(6, 30)
	a.ID = 1
	a.RepeatDays = EveryDay

	scheduled := time.Date(2026, 9, 18, 6, 30, 0, 0, loc)

	// Exactly on time.
	when, ok := a.Due(scheduled, loc, 5*time.Minute)
	if !ok || !when.Equal(scheduled) {
		t.Fatalf("on time: %v, %v", when, ok)
	}

	// Three minutes late (a reboot) still rings.
	when, ok = a.Due(scheduled.Add(3*time.Minute), loc, 5*time.Minute)
	if !ok || !when.Equal(scheduled) {
		t.Fatalf("slightly late: %v, %v", when, ok)
	}

	// Two hours late does not ring.
	if _, ok := a.Due(scheduled.Add(2*time.Hour), loc, 5*time.Minute); ok {
		t.Error("an alarm missed by two hours must not ring")
	}

	// Before the time does not ring.
	if _, ok := a.Due(scheduled.Add(-time.Minute), loc, 5*time.Minute); ok {
		t.Error("an alarm should not ring early")
	}
}

func TestDueDoesNotRefireTheSameOccurrence(t *testing.T) {
	loc := time.UTC
	a := New(6, 30)
	a.RepeatDays = EveryDay
	scheduled := time.Date(2026, 9, 18, 6, 30, 0, 0, loc)
	a.LastFired = scheduled

	if _, ok := a.Due(scheduled.Add(time.Minute), loc, 5*time.Minute); ok {
		t.Error("an already-fired occurrence must not fire again")
	}
	// The next day's occurrence is still eligible.
	next := scheduled.AddDate(0, 0, 1)
	if _, ok := a.Due(next, loc, 5*time.Minute); !ok {
		t.Error("the next day's occurrence should fire")
	}
}

func TestDueDisabled(t *testing.T) {
	a := New(6, 30)
	a.RepeatDays = EveryDay
	a.Enabled = false
	if _, ok := a.Due(time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC), time.UTC, time.Minute); ok {
		t.Error("a disabled alarm must never be due")
	}
}

func TestSortAlarms(t *testing.T) {
	as := []Alarm{
		{ID: 3, Hour: 7, Minute: 0},
		{ID: 1, Hour: 6, Minute: 30},
		{ID: 2, Hour: 6, Minute: 0},
		{ID: 4, Hour: 6, Minute: 30},
	}
	SortAlarms(as)
	want := []int64{2, 1, 4, 3}
	for i, id := range want {
		if as[i].ID != id {
			t.Fatalf("order: got %v want %v", ids(as), want)
		}
	}
}

func ids(as []Alarm) []int64 {
	out := make([]int64, len(as))
	for i, a := range as {
		out[i] = a.ID
	}
	return out
}
