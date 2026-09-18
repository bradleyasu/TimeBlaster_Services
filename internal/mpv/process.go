package mpv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Options configure a supervised mpv instance.
type Options struct {
	// Name identifies the instance in logs ("tv" or "alarm").
	Name string
	// Binary is the mpv executable.
	Binary string
	// Socket is the JSON IPC socket path. The supervisor creates and removes it.
	Socket string
	// Args are the instance-specific mpv options. --input-ipc-server is added
	// automatically, so it must not appear here.
	Args []string
	// StartupTimeout is how long to wait for the socket to appear and accept a
	// connection after spawning mpv.
	StartupTimeout time.Duration
	// CommandTimeout bounds a single IPC request.
	CommandTimeout time.Duration
	// MinBackoff and MaxBackoff bound the restart loop.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// OnConnect is called after each successful (re)connection, on the
	// supervisor's goroutine. It is where the caller restores state — reloading
	// the current channel, re-applying volume — after a crash.
	OnConnect func(ctx context.Context, c *Client)
	// OnEvent receives mpv's asynchronous events.
	OnEvent func(Event)
}

// BuildArgs produces the full mpv argument list for these options.
//
// It is split out from the spawn path so that the exact command line can be
// asserted in tests without mpv being installed — the DRM/KMS flags are the sort
// of thing that silently regress and are painful to debug on a television.
func BuildArgs(o Options) []string {
	args := make([]string, 0, len(o.Args)+2)
	args = append(args, "--input-ipc-server="+o.Socket)
	args = append(args, o.Args...)
	return args
}

// Supervisor keeps one mpv process running and exposes a Controller for it.
//
// The contract the rest of the application relies on: calling a method when mpv
// is down returns ErrNotConnected rather than blocking or panicking, and the
// supervisor is already busy restarting it. No caller needs restart logic.
type Supervisor struct {
	opts Options
	log  *slog.Logger

	mu      sync.RWMutex
	client  *Client
	cmd     *exec.Cmd
	started time.Time
	// restarts counts process starts, which the health endpoint reports: an mpv
	// that is restarting every ten seconds is a configuration problem worth
	// seeing without reading the journal.
	restarts int
	lastErr  error
}

// NewSupervisor creates a supervisor. Call Run to start it.
func NewSupervisor(opts Options, log *slog.Logger) *Supervisor {
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 15 * time.Second
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 5 * time.Second
	}
	return &Supervisor{opts: opts, log: log.With("mpv", opts.Name)}
}

// Run supervises mpv until the context is cancelled. It always returns
// ctx.Err(): an mpv that cannot be started is a degraded Timeblaster, not a
// reason to stop the daemon and take the alarm clock down with it.
func (s *Supervisor) Run(ctx context.Context) error {
	backoff := s.opts.MinBackoff

	for {
		if ctx.Err() != nil {
			s.shutdown()
			return ctx.Err()
		}

		err := s.runOnce(ctx)
		s.shutdown()

		if ctx.Err() != nil {
			return ctx.Err()
		}

		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()

		if err != nil {
			s.log.Error("mpv exited; restarting", "error", err, "backoff", backoff)
		} else {
			s.log.Warn("mpv exited unexpectedly; restarting", "backoff", backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.opts.MaxBackoff)
	}
}

// runOnce spawns mpv, connects, and blocks until the process or connection dies.
func (s *Supervisor) runOnce(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.opts.Socket), 0o750); err != nil {
		return fmt.Errorf("mpv: creating socket directory: %w", err)
	}
	// A stale socket from a killed process stops mpv from binding.
	_ = os.Remove(s.opts.Socket)

	args := BuildArgs(s.opts)
	cmd := exec.CommandContext(ctx, s.opts.Binary, args...)
	// Put mpv in its own process group so that killing it never signals the whole
	// service, and so a hung mpv can be killed as a group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mpv: starting %s: %w", s.opts.Binary, err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.started = time.Now()
	s.restarts++
	restarts := s.restarts
	s.mu.Unlock()

	s.log.Info("mpv started", "pid", cmd.Process.Pid, "start_count", restarts, "args", args)

	// Wait for mpv to create its IPC socket.
	client, err := s.connect(ctx)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}

	s.mu.Lock()
	s.client = client
	s.lastErr = nil
	s.mu.Unlock()

	s.log.Info("mpv IPC connected", "socket", s.opts.Socket)

	if s.opts.OnEvent != nil {
		go func() {
			for ev := range client.Events() {
				s.opts.OnEvent(ev)
			}
		}()
	}
	if s.opts.OnConnect != nil {
		s.opts.OnConnect(ctx, client)
	}

	// Wait for whichever fails first: the process or the IPC connection.
	procDone := make(chan error, 1)
	go func() { procDone <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-procDone:
		client.Close()
		if err != nil {
			return fmt.Errorf("mpv: process exited: %w", err)
		}
		return errors.New("mpv: process exited cleanly")
	case <-clientClosed(client):
		// IPC died but the process may still be running; kill it so the restart
		// is clean rather than leaving an orphan holding the HDMI output.
		s.log.Warn("mpv IPC connection lost; terminating the process")
		_ = cmd.Process.Kill()
		<-procDone
		return errors.New("mpv: IPC connection lost")
	}
}

// connect polls for the IPC socket until mpv creates it.
func (s *Supervisor) connect(ctx context.Context) (*Client, error) {
	deadline := time.Now().Add(s.opts.StartupTimeout)
	// A short poll is fine here: it runs only during startup, not steady state.
	const pollInterval = 100 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		client, err := Dial(ctx, s.opts.Socket, s.opts.CommandTimeout, s.log)
		if err == nil {
			return client, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("mpv: IPC socket %s did not become ready within %s: %w",
				s.opts.Socket, s.opts.StartupTimeout, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (s *Supervisor) shutdown() {
	s.mu.Lock()
	client, cmd := s.client, s.cmd
	s.client, s.cmd = nil, nil
	s.mu.Unlock()

	if client != nil {
		client.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = os.Remove(s.opts.Socket)
}

// Client returns the current client, or nil when mpv is down.
func (s *Supervisor) Client() *Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// Status describes the supervised process for the health endpoint.
type Status struct {
	Name      string    `json:"name"`
	Alive     bool      `json:"alive"`
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"started_at,omitzero"`
	LastError string    `json:"last_error,omitempty"`
}

// Status reports the supervisor's state.
func (s *Supervisor) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{Name: s.opts.Name, Alive: s.client != nil && s.client.Alive(), Restarts: s.restarts}
	if s.client != nil {
		st.StartedAt = s.started
	}
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
	}
	return st
}

// --- Controller, delegating to whatever client is currently connected --------

// Alive reports whether mpv is currently controllable.
func (s *Supervisor) Alive() bool {
	c := s.Client()
	return c != nil && c.Alive()
}

// Command sends a raw mpv command.
func (s *Supervisor) Command(ctx context.Context, args ...any) (json.RawMessage, error) {
	c := s.Client()
	if c == nil {
		return nil, ErrNotConnected
	}
	return c.Command(ctx, args...)
}

// SetProperty sets an mpv property.
func (s *Supervisor) SetProperty(ctx context.Context, name string, value any) error {
	c := s.Client()
	if c == nil {
		return ErrNotConnected
	}
	return c.SetProperty(ctx, name, value)
}

// GetProperty reads an mpv property.
func (s *Supervisor) GetProperty(ctx context.Context, name string, out any) error {
	c := s.Client()
	if c == nil {
		return ErrNotConnected
	}
	return c.GetProperty(ctx, name, out)
}

// LoadFile replaces what is playing.
func (s *Supervisor) LoadFile(ctx context.Context, url string) error {
	c := s.Client()
	if c == nil {
		return ErrNotConnected
	}
	return c.LoadFile(ctx, url)
}

func clientClosed(c *Client) <-chan struct{} { return c.closed }

var _ Controller = (*Supervisor)(nil)
