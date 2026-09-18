package mpv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Call is one command recorded by Fake.
type Call struct {
	Args []any
}

// Name returns the command verb.
func (c Call) Name() string {
	if len(c.Args) == 0 {
		return ""
	}
	s, _ := c.Args[0].(string)
	return s
}

// String renders the call for test failure messages.
func (c Call) String() string {
	parts := make([]string, len(c.Args))
	for i, a := range c.Args {
		parts[i] = fmt.Sprint(a)
	}
	return strings.Join(parts, " ")
}

// Fake is an in-memory Controller for tests. It records every command and can be
// made to fail, which is how the recovery paths get exercised without killing a
// real mpv.
//
// It lives in the non-test file set because media, audio and app tests all need it.
type Fake struct {
	mu    sync.Mutex
	calls []Call
	props map[string]any

	// NotAlive makes Alive report false and every command return ErrNotConnected,
	// simulating a crashed mpv.
	NotAlive bool
	// Err, when set, is returned by every command.
	Err error
	// OnCommand lets a test intercept specific commands.
	OnCommand func(Call) (json.RawMessage, error)
}

// NewFake returns an initialised fake controller.
func NewFake() *Fake { return &Fake{props: map[string]any{}} }

// Command records the call and returns the configured response.
func (f *Fake) Command(_ context.Context, args ...any) (json.RawMessage, error) {
	f.mu.Lock()
	call := Call{Args: append([]any(nil), args...)}
	f.calls = append(f.calls, call)
	notAlive, err, hook := f.NotAlive, f.Err, f.OnCommand
	f.mu.Unlock()

	if notAlive {
		return nil, ErrNotConnected
	}
	if hook != nil {
		return hook(call)
	}
	if err != nil {
		return nil, err
	}

	// Emulate just enough of mpv for the property helpers to work.
	if call.Name() == "get_property" && len(args) > 1 {
		name, _ := args[1].(string)
		f.mu.Lock()
		v, ok := f.props[name]
		f.mu.Unlock()
		if !ok {
			return nil, &CommandError{Command: "get_property", Reason: "property unavailable"}
		}
		return json.Marshal(v)
	}
	if call.Name() == "set_property" && len(args) > 2 {
		name, _ := args[1].(string)
		f.mu.Lock()
		f.props[name] = args[2]
		f.mu.Unlock()
	}
	return nil, nil
}

// SetProperty sets a property.
func (f *Fake) SetProperty(ctx context.Context, name string, value any) error {
	_, err := f.Command(ctx, "set_property", name, value)
	return err
}

// GetProperty reads a property.
func (f *Fake) GetProperty(ctx context.Context, name string, out any) error {
	data, err := f.Command(ctx, "get_property", name)
	if err != nil {
		return err
	}
	if out == nil || data == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// LoadFile records a loadfile command.
func (f *Fake) LoadFile(ctx context.Context, url string) error {
	_, err := f.Command(ctx, "loadfile", url, "replace")
	return err
}

// Alive reports whether the fake is pretending to be connected.
func (f *Fake) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.NotAlive
}

// SetProp seeds a property value without recording a call.
func (f *Fake) SetProp(name string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.props[name] = value
}

// Calls returns every recorded command.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallsNamed returns the recorded commands with the given verb.
func (f *Fake) CallsNamed(name string) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if c.Name() == name {
			out = append(out, c)
		}
	}
	return out
}

// LastCall returns the most recent command.
func (f *Fake) LastCall() (Call, bool) {
	calls := f.Calls()
	if len(calls) == 0 {
		return Call{}, false
	}
	return calls[len(calls)-1], true
}

// Reset clears recorded calls.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

var _ Controller = (*Fake)(nil)
