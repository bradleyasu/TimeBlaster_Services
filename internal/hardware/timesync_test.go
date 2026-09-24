package hardware

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/serialport"
	"github.com/bradsheets/timeblaster/internal/system"
)

// gatedSync is a ClockSync the test flips when it chooses.
type gatedSync struct {
	mu sync.Mutex
	ok bool
}

func (g *gatedSync) Synced() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ok
}

func (g *gatedSync) set(v bool) {
	g.mu.Lock()
	g.ok = v
	g.mu.Unlock()
}

// collector accumulates everything the Pi sends, so a test can assert on what
// was NOT sent as well as what was.
type collector struct {
	mu   sync.Mutex
	msgs []protocol.Message
}

func collectFrom(p *serialport.Pipe) *collector {
	c := &collector{}
	sc := serialport.NewScanner(p)
	go func() {
		for sc.Scan() {
			if m, err := sc.Message(); err == nil {
				c.mu.Lock()
				c.msgs = append(c.msgs, m)
				c.mu.Unlock()
			}
		}
	}()
	return c
}

// hasArgs reports whether any message of typ carried exactly these arguments.
func (c *collector) hasArgs(typ protocol.Type, args ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if m.Type != typ || len(m.Args) != len(args) {
			continue
		}
		same := true
		for i := range args {
			if m.Args[i] != args[i] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

func (c *collector) count(typ protocol.Type) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		if m.Type == typ {
			n++
		}
	}
	return n
}

func TestTimeIsWithheldUntilTheClockIsSynchronised(t *testing.T) {
	// A Pi 5 with no coin cell on its RTC connector boots knowing nothing, and
	// systemd winds the clock forward to roughly when it was last powered
	// rather than leave it in 1970. That time is entirely plausible and a day
	// wrong. Sending it ends the Nano's loading animation and puts yesterday's
	// time on the display until NTP corrects it half a minute later.
	cfg := DefaultConfig()
	// Deliberately long, so the test fails if the first tick is scheduled at the
	// full interval instead of the short re-check poll.
	cfg.TimeSyncInterval = 10 * time.Minute
	cfg.HeartbeatTimeout = time.Hour // this test moves the clock about

	clockSync := &gatedSync{ok: false}
	f, nanos := newLinkFixtureWith(t, cfg, 1, clockSync)
	got := collectFrom(nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	// Resync runs on connect and sends the rest of the device's state.
	waitUntil(t, "the resync to arrive", func() bool { return got.count(protocol.TypeDisplay) > 0 })
	if n := got.count(protocol.TypeTime); n != 0 {
		t.Fatalf("sent %d TIME frames with an unsynchronised clock, want 0", n)
	}

	// It must keep withholding across sync ticks, not just the first one. The
	// clock is nudged in a loop because each tick re-registers the timer, and
	// a single Advance can land in the gap between the two.
	waitUntil(t, "the sync timer", func() bool { return f.clock.Waiters() >= 2 })
	for range 5 {
		f.clock.Advance(3 * time.Second) // past the short re-check poll
		time.Sleep(20 * time.Millisecond)
	}
	if n := got.count(protocol.TypeTime); n != 0 {
		t.Fatalf("sent %d TIME frames while still unsynchronised, want 0", n)
	}

	// NTP lands. The time must follow within the short re-check poll rather
	// than waiting out the full minute, or the animation runs on long after the
	// Pi knows what time it is.
	clockSync.set(true)
	// Only a few simulated seconds are allowed to pass: far less than
	// TimeSyncInterval, so this only succeeds if the link is on its short
	// re-check poll rather than waiting out a full interval.
	const budget = 10 * time.Second
	for advanced := time.Duration(0); advanced < budget; advanced += clockWaitPoll {
		if got.count(protocol.TypeTime) > 0 {
			break
		}
		f.clock.Advance(clockWaitPoll)
		time.Sleep(20 * time.Millisecond)
	}
	if got.count(protocol.TypeTime) == 0 {
		t.Errorf("no TIME frame within %s of the clock becoming trustworthy; "+
			"is the first tick scheduled at the full sync interval?", budget)
	}
}

func TestTimesyncdMarker(t *testing.T) {
	// systemd-timesyncd creates this on its first successful sync. It lives
	// under /run, so it is absent again after every boot -- which is exactly
	// the signal wanted here.
	path := filepath.Join(t.TempDir(), "synchronized")

	m := system.TimesyncdMarker{Path: path}
	if m.Synced() {
		t.Error("no marker yet, so the clock must not be trusted")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.Synced() {
		t.Error("the marker exists, so the clock should be trusted")
	}
	if !(system.AlwaysSynced{}).Synced() {
		t.Error("AlwaysSynced must trust the clock")
	}
}

func TestResyncRestoresTheClockFormat(t *testing.T) {
	// The Nano keeps nothing across a replug, so every reconnect has to put it
	// back into the user's chosen mode. Without this it would come back in
	// 12-hour form whatever the setting said.
	cfg := DefaultConfig()
	cfg.HeartbeatTimeout = time.Hour
	cfg.Clock24h = true

	f, nanos := newLinkFixtureWith(t, cfg, 1, nil)
	got := collectFrom(nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	waitUntil(t, "the config frame", func() bool { return got.count(protocol.TypeConfig) > 0 })
	if !got.hasArgs(protocol.TypeConfig, protocol.ConfigKeyClock24h, "1") {
		t.Error("the stored 24-hour preference was not sent on connect")
	}
}
