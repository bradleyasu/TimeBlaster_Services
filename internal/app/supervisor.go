// Package app is Timeblaster's composition root.
//
// It is the only package that knows about all the others: it constructs the
// subsystems, wires them together, routes events between them, and supervises
// their goroutines. Every cross-subsystem policy decision — what the big red
// button means, what a turn of the channel knob does, what happens when the Nano
// reconnects — lives here and nowhere else, so the device's behaviour can be
// read in one place.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// task is one supervised goroutine.
type task struct {
	name string
	// critical marks a task whose permanent failure should stop the daemon.
	// Only the things the alarm clock itself needs are critical; the television,
	// the Nano and the network are all allowed to fail indefinitely.
	critical bool
	run      func(context.Context) error
	// restartDelay is how long to wait before restarting after a panic.
	restartDelay time.Duration
}

// supervisor runs tasks, restarting any that panic.
//
// The guarantee it provides is the one the whole design rests on: a bug in the
// television code cannot stop alarms from ringing. A panicking task is logged,
// its stack captured, and it is restarted; it never unwinds into the process.
type supervisor struct {
	log *slog.Logger

	mu     sync.Mutex
	tasks  []task
	failed map[string]error
}

func newSupervisor(log *slog.Logger) *supervisor {
	return &supervisor{log: log, failed: map[string]error{}}
}

// add registers a task. It must be called before run.
func (s *supervisor) add(name string, critical bool, run func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, task{name: name, critical: critical, run: run, restartDelay: time.Second})
}

// run starts every task and blocks until the context is cancelled or a critical
// task fails.
func (s *supervisor) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	tasks := append([]task(nil), s.tasks...)
	s.mu.Unlock()

	var wg sync.WaitGroup
	fatal := make(chan error, len(tasks))

	for _, t := range tasks {
		wg.Add(1)
		go func(t task) {
			defer wg.Done()
			if err := s.runTask(ctx, t); err != nil {
				s.mu.Lock()
				s.failed[t.name] = err
				s.mu.Unlock()
				if t.critical {
					fatal <- fmt.Errorf("app: critical subsystem %q failed: %w", t.name, err)
					cancel()
				}
			}
		}(t)
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-fatal:
	}
	cancel()

	// Give the tasks a bounded window to unwind so shutdown cannot hang forever
	// on a stuck subsystem.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.log.Warn("some subsystems did not stop within the shutdown window")
	}

	if err != nil {
		return err
	}
	return ctx.Err()
}

// runTask runs one task, restarting it if it panics.
func (s *supervisor) runTask(ctx context.Context, t task) error {
	for {
		err := s.runOnce(ctx, t)
		if ctx.Err() != nil {
			return nil // a clean shutdown, not a failure
		}
		if err == nil {
			s.log.Warn("subsystem returned without an error; restarting", "subsystem", t.name)
		} else if isPanic(err) {
			s.log.Error("subsystem panicked; restarting",
				"subsystem", t.name, "error", err)
		} else {
			// A task that returns an ordinary error has decided it cannot
			// continue. Non-critical tasks simply stop; the rest of the device
			// carries on without them.
			if !t.critical {
				s.log.Error("subsystem stopped", "subsystem", t.name, "error", err)
				return err
			}
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(t.restartDelay):
		}
	}
}

func (s *supervisor) runOnce(ctx context.Context, t task) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &panicError{value: v, stack: string(debug.Stack())}
		}
	}()
	return t.run(ctx)
}

// failures returns the tasks that stopped with an error, for the health endpoint.
func (s *supervisor) failures() map[string]error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]error, len(s.failed))
	for k, v := range s.failed {
		out[k] = v
	}
	return out
}

// panicError carries a recovered panic and its stack.
type panicError struct {
	value any
	stack string
}

func (e *panicError) Error() string {
	return fmt.Sprintf("panic: %v\n%s", e.value, e.stack)
}

func isPanic(err error) bool {
	_, ok := err.(*panicError)
	return ok
}
