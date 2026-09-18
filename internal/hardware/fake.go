package hardware

import (
	"sync"
	"time"
)

// Command is one call recorded by FakeNano.
type Command struct {
	Kind  string
	Text  string
	Value int
	Flag  bool
	Time  time.Time
}

// FakeNano is an in-memory Nano for tests and for running the daemon on a
// development machine with no hardware attached.
type FakeNano struct {
	mu        sync.Mutex
	commands  []Command
	connected bool
	// Err, when set, is returned by every command.
	Err error
}

// NewFakeNano returns a fake that reports itself as connected.
func NewFakeNano() *FakeNano { return &FakeNano{connected: true} }

func (f *FakeNano) record(c Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.commands = append(f.commands, c)
	return nil
}

// SetTime records a time sync.
func (f *FakeNano) SetTime(t time.Time) error { return f.record(Command{Kind: "time", Time: t}) }

// SetAlarmActive records an alarm state change.
func (f *FakeNano) SetAlarmActive(active bool) error {
	return f.record(Command{Kind: "alarm", Flag: active})
}

// ShowText records a display text command.
func (f *FakeNano) ShowText(text string) error {
	return f.record(Command{Kind: "display-text", Text: text})
}

// ShowClock records a return to the clock display.
func (f *FakeNano) ShowClock() error { return f.record(Command{Kind: "display-clock"}) }

// SetBrightness records a brightness change.
func (f *FakeNano) SetBrightness(pct int) error {
	return f.record(Command{Kind: "brightness", Value: pct})
}

// SetLED records an LED change.
func (f *FakeNano) SetLED(name string, on bool) error {
	return f.record(Command{Kind: "led", Text: name, Flag: on})
}

// Connected reports the simulated link state.
func (f *FakeNano) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// SetConnected simulates the cable being plugged or pulled.
func (f *FakeNano) SetConnected(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = v
}

// Commands returns everything recorded.
func (f *FakeNano) Commands() []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Command(nil), f.commands...)
}

// CommandsOfKind returns the recorded commands of one kind.
func (f *FakeNano) CommandsOfKind(kind string) []Command {
	var out []Command
	for _, c := range f.Commands() {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// LastOfKind returns the most recent command of a kind.
func (f *FakeNano) LastOfKind(kind string) (Command, bool) {
	cs := f.CommandsOfKind(kind)
	if len(cs) == 0 {
		return Command{}, false
	}
	return cs[len(cs)-1], true
}

// Reset clears recorded commands.
func (f *FakeNano) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = nil
}

var _ Nano = (*FakeNano)(nil)
