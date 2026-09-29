package system

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// ClockSync reports whether the system clock can be trusted.
//
// This matters because a Raspberry Pi 5 with no coin cell on its RTC connector
// boots with no idea what time it is. The kernel log says so plainly:
//
//	rpi-rtc: setting system clock to 1970-01-01T00:00:18 UTC (18)
//
// systemd-timesyncd will not leave the clock in 1970, so it winds it forward to
// the timestamp it last recorded -- roughly when the Pi was last powered. The
// result is a clock that looks entirely plausible and is a day wrong, until NTP
// corrects it half a minute later. Forwarding that to the seven-segment display
// shows the user yesterday's time as though it were now.
type ClockSync interface {
	Synced() bool
}

// TimesyncdMarker reads systemd-timesyncd's synchronisation marker.
//
// timesyncd creates this file the first time it successfully sets the clock
// from a server. It lives under /run, so it is absent again after every boot,
// which is exactly the signal wanted here. This is the same file
// systemd-time-wait-sync waits on.
type TimesyncdMarker struct {
	// Path overrides the marker location. Empty means the standard one.
	Path string
}

// DefaultTimesyncdMarker is where systemd-timesyncd writes the marker.
const DefaultTimesyncdMarker = "/run/systemd/timesync/synchronized"

func (m TimesyncdMarker) Synced() bool {
	path := m.Path
	if path == "" {
		path = DefaultTimesyncdMarker
	}
	_, err := os.Stat(path)
	return err == nil
}

// DefaultRTCPath is where the kernel exposes the hardware clock's value in
// seconds since the epoch.
const DefaultRTCPath = "/sys/class/rtc/rtc0/since_epoch"

// ClockFloor is the earliest value that can be a real current date.
//
// Nothing about this device existed before it, so a clock reading earlier is
// not merely wrong, it is uninitialised.
var ClockFloor = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// RTCClock reports whether the hardware clock carried a real date through the
// power cut.
//
// A Pi with a coin cell on its RTC connector knows the time the moment it
// boots, and making it wait for a network sync before showing anything wastes
// half a minute for no reason -- which is what the timesyncd marker alone did,
// ironically to a device whose battery had just fixed the problem the marker
// was there to work around.
//
// Without a cell the RTC starts from the epoch and counts up from power-on, so
// seconds after boot it reads 1970 and fails this check. That is exactly the
// case where waiting for NTP is right.
type RTCClock struct {
	// Path is the sysfs file to read. Empty means the standard one.
	Path string
	// NotBefore is the earliest value taken as a real date. Zero means
	// ClockFloor.
	NotBefore time.Time
}

func (r RTCClock) Synced() bool {
	path := r.Path
	if path == "" {
		path = DefaultRTCPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false // no hardware clock to trust
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return false
	}
	floor := r.NotBefore
	if floor.IsZero() {
		floor = ClockFloor
	}
	return time.Unix(secs, 0).After(floor)
}

// AnyClockSync trusts the clock as soon as any of its members does.
//
// The two that matter disagree about what "trustworthy" means and are both
// right: a network sync proves the time, and a hardware clock that survived
// the power cut already knows it.
type AnyClockSync []ClockSync

func (a AnyClockSync) Synced() bool {
	for _, s := range a {
		if s != nil && s.Synced() {
			return true
		}
	}
	return false
}

// AlwaysSynced trusts the clock unconditionally. It backs tests, and the
// configuration for an installation that deliberately runs without NTP.
type AlwaysSynced struct{}

func (AlwaysSynced) Synced() bool { return true }
