package system

import "os"

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

// AlwaysSynced trusts the clock unconditionally. It backs tests, and the
// configuration for an installation that deliberately runs without NTP.
type AlwaysSynced struct{}

func (AlwaysSynced) Synced() bool { return true }
