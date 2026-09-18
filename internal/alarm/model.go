// Package alarm owns alarm definitions, recurrence arithmetic and the scheduler
// that decides when an alarm rings.
//
// This package is the most important one in Timeblaster, so it has the fewest
// dependencies: the standard library, a Clock, a Store and a Sink. It knows
// nothing about audio devices, mpv, ErsatzTV, the Nano or the network, which is
// precisely what guarantees that an alarm still rings when all of those are broken.
package alarm

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Weekday bitmask values. Bit 0 is Sunday so that the bit index matches
// time.Weekday directly, which removes a whole class of off-by-one bugs.
const (
	Sunday    = 1 << uint(time.Sunday)
	Monday    = 1 << uint(time.Monday)
	Tuesday   = 1 << uint(time.Tuesday)
	Wednesday = 1 << uint(time.Wednesday)
	Thursday  = 1 << uint(time.Thursday)
	Friday    = 1 << uint(time.Friday)
	Saturday  = 1 << uint(time.Saturday)

	// Weekdays is Monday through Friday.
	Weekdays = Monday | Tuesday | Wednesday | Thursday | Friday
	// Weekends is Saturday and Sunday.
	Weekends = Saturday | Sunday
	// EveryDay is all seven days.
	EveryDay = Weekdays | Weekends

	// NoRepeat marks a one-shot alarm.
	NoRepeat = 0
)

// DateLayout is the format used for a one-shot alarm's explicit date.
const DateLayout = "2006-01-02"

// Defaults applied to new alarms.
const (
	DefaultSnoozeMinutes   = 9
	DefaultAutoStopMinutes = 15
	// MaxAutoStopMinutes bounds how long an undismissed alarm may ring. An alarm
	// that rings forever in an empty house is a worse outcome than one that gives
	// up, so there is always a ceiling.
	MaxAutoStopMinutes = 120
)

// Alarm is a stored alarm definition. Times are wall-clock times in the
// configured timezone, not absolute instants: "07:00 on weekdays" must stay at
// 07:00 across a daylight-saving transition.
type Alarm struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`

	// Hour and Minute are local wall-clock time, 0-23 and 0-59.
	Hour   int `json:"hour"`
	Minute int `json:"minute"`

	Enabled bool `json:"enabled"`

	// RepeatDays is a weekday bitmask. NoRepeat means the alarm fires once and
	// then disables itself.
	RepeatDays int `json:"repeat_days"`

	// OneShotDate optionally pins a non-repeating alarm to a specific date
	// (YYYY-MM-DD). Empty means "the next time this wall-clock time comes around".
	OneShotDate string `json:"one_shot_date,omitempty"`

	// SoundID names the sound to play. Empty means "the configured default",
	// which is what keeps an alarm working after its sound file is deleted.
	SoundID string `json:"sound_id"`

	SnoozeMinutes   int `json:"snooze_minutes"`
	AutoStopMinutes int `json:"auto_stop_minutes"`

	// LastFired is when this alarm last started ringing. It is what stops an alarm
	// from firing twice within the same minute and what drives missed-alarm
	// catch-up after a power cut.
	LastFired time.Time `json:"last_fired,omitzero"`

	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// New returns an alarm with sensible defaults applied.
func New(hour, minute int) Alarm {
	return Alarm{
		Hour:            hour,
		Minute:          minute,
		Enabled:         true,
		SnoozeMinutes:   DefaultSnoozeMinutes,
		AutoStopMinutes: DefaultAutoStopMinutes,
	}
}

// Validate checks that the alarm is well formed. It is called on every write, so
// nothing invalid can reach the database or the scheduler.
func (a Alarm) Validate() error {
	if a.Hour < 0 || a.Hour > 23 {
		return fmt.Errorf("alarm: hour must be 0-23, got %d", a.Hour)
	}
	if a.Minute < 0 || a.Minute > 59 {
		return fmt.Errorf("alarm: minute must be 0-59, got %d", a.Minute)
	}
	if a.RepeatDays < 0 || a.RepeatDays > EveryDay {
		return fmt.Errorf("alarm: repeat_days must be a 7-bit mask, got %d", a.RepeatDays)
	}
	if a.SnoozeMinutes < 0 || a.SnoozeMinutes > 60 {
		return fmt.Errorf("alarm: snooze_minutes must be 0-60, got %d", a.SnoozeMinutes)
	}
	if a.AutoStopMinutes < 0 || a.AutoStopMinutes > MaxAutoStopMinutes {
		return fmt.Errorf("alarm: auto_stop_minutes must be 0-%d, got %d", MaxAutoStopMinutes, a.AutoStopMinutes)
	}
	if len(a.Label) > 64 {
		return fmt.Errorf("alarm: label must be 64 characters or fewer, got %d", len(a.Label))
	}
	if a.OneShotDate != "" {
		if a.RepeatDays != NoRepeat {
			return fmt.Errorf("alarm: one_shot_date cannot be combined with repeat days")
		}
		if _, err := time.Parse(DateLayout, a.OneShotDate); err != nil {
			return fmt.Errorf("alarm: one_shot_date must be YYYY-MM-DD, got %q", a.OneShotDate)
		}
	}
	return nil
}

// Normalize fills in defaults for zero-valued fields and trims user text. It runs
// before Validate so that a minimal JSON body from the app is still usable.
func (a Alarm) Normalize() Alarm {
	a.Label = strings.TrimSpace(a.Label)
	a.SoundID = strings.TrimSpace(a.SoundID)
	a.OneShotDate = strings.TrimSpace(a.OneShotDate)
	if a.SnoozeMinutes == 0 {
		a.SnoozeMinutes = DefaultSnoozeMinutes
	}
	if a.AutoStopMinutes == 0 {
		a.AutoStopMinutes = DefaultAutoStopMinutes
	}
	return a
}

// Repeats reports whether this is a recurring alarm.
func (a Alarm) Repeats() bool { return a.RepeatDays != NoRepeat }

// RepeatsOn reports whether the alarm repeats on the given weekday.
func (a Alarm) RepeatsOn(d time.Weekday) bool { return a.RepeatDays&(1<<uint(d)) != 0 }

// TimeString renders the alarm's wall-clock time as HH:MM.
func (a Alarm) TimeString() string { return fmt.Sprintf("%02d:%02d", a.Hour, a.Minute) }

// RepeatString renders the repeat mask for logs and the UI.
func (a Alarm) RepeatString() string {
	switch a.RepeatDays {
	case NoRepeat:
		if a.OneShotDate != "" {
			return "once on " + a.OneShotDate
		}
		return "once"
	case EveryDay:
		return "every day"
	case Weekdays:
		return "weekdays"
	case Weekends:
		return "weekends"
	}
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	var out []string
	for d := time.Sunday; d <= time.Saturday; d++ {
		if a.RepeatsOn(d) {
			out = append(out, names[d])
		}
	}
	return strings.Join(out, ",")
}

// String renders the alarm for logs.
func (a Alarm) String() string {
	state := "disabled"
	if a.Enabled {
		state = "enabled"
	}
	return fmt.Sprintf("alarm#%d %s %s (%s)", a.ID, a.TimeString(), a.RepeatString(), state)
}

// NextOccurrence returns the first instant strictly after `after` at which this
// alarm should ring, in location loc.
//
// Daylight saving is handled by constructing the instant with time.Date in the
// target location on each candidate calendar day, which is what makes "07:00
// every weekday" stay at 07:00 rather than drifting by an hour twice a year.
//
// Two edge cases are worth stating explicitly, because they are the ones that
// actually bite:
//
//   - Spring forward: a wall-clock time that does not exist on that day (02:30 in
//     a zone that jumps 02:00→03:00) is normalised forward by time.Date to 03:30.
//     The alarm still rings, once, on that day.
//   - Fall back: a wall-clock time that occurs twice is fired on its first
//     occurrence, which is time.Date's behaviour, and LastFired then prevents a
//     second ring an hour later.
func (a Alarm) NextOccurrence(after time.Time, loc *time.Location) (time.Time, bool) {
	if !a.Enabled || loc == nil {
		return time.Time{}, false
	}
	after = after.In(loc)

	// One-shot alarm pinned to an explicit date.
	if !a.Repeats() && a.OneShotDate != "" {
		d, err := time.ParseInLocation(DateLayout, a.OneShotDate, loc)
		if err != nil {
			return time.Time{}, false
		}
		when := time.Date(d.Year(), d.Month(), d.Day(), a.Hour, a.Minute, 0, 0, loc)
		if when.After(after) {
			return when, true
		}
		return time.Time{}, false // the date has passed
	}

	// One-shot alarm with no date: the next time this wall clock comes around.
	if !a.Repeats() {
		today := time.Date(after.Year(), after.Month(), after.Day(), a.Hour, a.Minute, 0, 0, loc)
		if today.After(after) {
			return today, true
		}
		tomorrow := after.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), a.Hour, a.Minute, 0, 0, loc), true
	}

	// Recurring alarm: walk forward day by day. Eight days covers every mask,
	// including the case where today's occurrence has already passed.
	for i := range 8 {
		day := after.AddDate(0, 0, i)
		if !a.RepeatsOn(day.Weekday()) {
			continue
		}
		when := time.Date(day.Year(), day.Month(), day.Day(), a.Hour, a.Minute, 0, 0, loc)
		if when.After(after) {
			return when, true
		}
	}
	return time.Time{}, false
}

// Due reports whether the alarm should ring at or before now, given a catch-up
// window. It exists so the scheduler can handle two awkward realities:
//
//   - The Pi was powered off (or the daemon was restarting) across the alarm time.
//     Within the catch-up window we still ring, because waking up five minutes
//     late is far better than not waking up.
//   - The scheduler's timer fired a few milliseconds early or the clock stepped.
//     Comparing against the minute boundary rather than the exact instant avoids
//     a spin where the alarm is perpetually "not quite due".
func (a Alarm) Due(now time.Time, loc *time.Location, catchUp time.Duration) (time.Time, bool) {
	if !a.Enabled || loc == nil {
		return time.Time{}, false
	}
	now = now.In(loc)

	// Look back over the catch-up window plus a day, so we find the most recent
	// scheduled instant at or before now.
	start := now.Add(-catchUp).Add(-time.Minute)
	candidate, ok := a.NextOccurrence(start.Add(-time.Nanosecond), loc)
	if !ok || candidate.After(now) {
		return time.Time{}, false
	}
	if now.Sub(candidate) > catchUp {
		return time.Time{}, false // missed by too much; skip it rather than ring at noon
	}
	// Do not re-fire an occurrence we already rang for.
	if !a.LastFired.IsZero() && !a.LastFired.Before(candidate) {
		return time.Time{}, false
	}
	return candidate, true
}

// SortAlarms orders alarms by time of day and then id, which is the order the
// companion app displays them in.
func SortAlarms(as []Alarm) {
	sort.SliceStable(as, func(i, j int) bool {
		if as[i].Hour != as[j].Hour {
			return as[i].Hour < as[j].Hour
		}
		if as[i].Minute != as[j].Minute {
			return as[i].Minute < as[j].Minute
		}
		return as[i].ID < as[j].ID
	})
}
