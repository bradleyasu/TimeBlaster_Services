package system

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeRTC lays down a sysfs-shaped since_epoch file. Passing a negative count
// means "no hardware clock here at all".
func writeRTC(t *testing.T, secs int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "since_epoch")
	if secs < 0 {
		return path
	}
	if err := os.WriteFile(path, []byte(strconv.FormatInt(secs, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRTCClockSynced(t *testing.T) {
	for _, tc := range []struct {
		name string
		secs int64
		want bool
	}{
		{
			// The reading from this device moments after a reboot with the
			// coin cell fitted. This is the case the change exists for.
			"battery carried the date through",
			1790718213,
			true,
		},
		{
			// With no cell the RTC powers up at the epoch and counts from
			// there, so by the time the daemon starts it reads a few seconds
			// past 1970 -- not a date, just an uptime.
			"battery-less pi counts up from the epoch",
			18,
			false,
		},
		{
			"exactly the floor is not after it",
			ClockFloor.Unix(),
			false,
		},
		{
			"a second past the floor counts",
			ClockFloor.Unix() + 1,
			true,
		},
		{
			"no rtc exposed at all",
			-1,
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := RTCClock{Path: writeRTC(t, tc.secs)}
			if got := r.Synced(); got != tc.want {
				t.Fatalf("Synced() = %v, want %v (since_epoch %d)", got, tc.want, tc.secs)
			}
		})
	}
}

func TestRTCClockRejectsUnreadableValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "since_epoch")
	if err := os.WriteFile(path, []byte("not a number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if (RTCClock{Path: path}).Synced() {
		t.Fatal("garbage in since_epoch was treated as a trustworthy clock")
	}
}

func TestTimesyncdMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synchronized")
	m := TimesyncdMarker{Path: path}
	if m.Synced() {
		t.Fatal("reported synced before timesyncd wrote the marker")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.Synced() {
		t.Fatal("reported unsynced after the marker appeared")
	}
}

func TestAnyClockSync(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "synchronized")
	dead := RTCClock{Path: writeRTC(t, 18)}
	live := RTCClock{Path: writeRTC(t, 1790718213)}

	t.Run("neither source trusts the clock", func(t *testing.T) {
		if (AnyClockSync{TimesyncdMarker{Path: marker}, dead}).Synced() {
			t.Fatal("trusted the clock with no NTP sync and an uninitialised RTC")
		}
	})

	t.Run("rtc alone is enough", func(t *testing.T) {
		// The regression: this combination held the seven-segment display on
		// its loading animation for ~26s after every boot, on a device whose
		// battery already knew the time.
		if !(AnyClockSync{TimesyncdMarker{Path: marker}, live}).Synced() {
			t.Fatal("waited for NTP although the RTC held a real date")
		}
	})

	t.Run("ntp alone is enough", func(t *testing.T) {
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if !(AnyClockSync{TimesyncdMarker{Path: marker}, dead}).Synced() {
			t.Fatal("ignored a completed NTP sync because the RTC was flat")
		}
	})

	t.Run("empty and nil members do not trust anything", func(t *testing.T) {
		if (AnyClockSync{}).Synced() || (AnyClockSync{nil}).Synced() {
			t.Fatal("trusted the clock with no working source")
		}
	})
}
