package system

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CommandRunner executes an external program. Every shell-out in Timeblaster goes
// through this interface so that command construction can be asserted in tests
// without the program being installed.
type CommandRunner interface {
	// Run executes name with args and returns its combined stdout. A non-zero exit
	// status is returned as an error that includes the captured stderr.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the production CommandRunner.
type ExecRunner struct {
	// Timeout caps how long any single command may run. Zero means 30 seconds.
	// Every helper we invoke (amixer, nmcli, hostnamectl) should complete in well
	// under a second, so a cap turns a hung helper into an error instead of a
	// permanently stuck goroutine.
	Timeout time.Duration
}

// Run executes the command, enforcing the configured timeout.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if ctx.Err() != nil {
			return stdout.Bytes(), fmt.Errorf("%s: %w (%s)", name, ctx.Err(), msg)
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

var _ CommandRunner = ExecRunner{}

// RecordedCommand is one invocation captured by FakeRunner.
type RecordedCommand struct {
	Name string
	Args []string
}

// String renders the command the way it would be typed, which makes test failure
// messages readable.
func (c RecordedCommand) String() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// FakeRunner is a CommandRunner for tests. It records every invocation and returns
// canned output keyed by the program name.
type FakeRunner struct {
	mu      sync.Mutex
	calls   []RecordedCommand
	Outputs map[string][]byte // keyed by program name
	Errors  map[string]error  // keyed by program name
	OnRun   func(RecordedCommand) ([]byte, error)
}

// NewFakeRunner returns an initialised FakeRunner.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{Outputs: map[string][]byte{}, Errors: map[string]error{}}
}

// Run records the invocation and returns the configured response.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	rec := RecordedCommand{Name: name, Args: append([]string(nil), args...)}
	f.mu.Lock()
	f.calls = append(f.calls, rec)
	hook, out, err := f.OnRun, f.Outputs[name], f.Errors[name]
	f.mu.Unlock()

	if hook != nil {
		return hook(rec)
	}
	return out, err
}

// Calls returns a copy of everything recorded so far.
func (f *FakeRunner) Calls() []RecordedCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecordedCommand(nil), f.calls...)
}

// LastCall returns the most recent invocation, or false when there was none.
func (f *FakeRunner) LastCall() (RecordedCommand, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return RecordedCommand{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// Reset discards recorded calls.
func (f *FakeRunner) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

var _ CommandRunner = (*FakeRunner)(nil)
