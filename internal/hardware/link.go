// Package hardware owns the session with the Arduino Nano: reading its reports,
// queueing commands to it, keeping the link alive, and re-establishing it when
// the USB cable is disturbed.
//
// It sits between serialport (bytes) and input (meaning). It decides *when* to
// talk to the Nano — heartbeats, time sync cadence, resync after reconnect — but
// never what an input event means; that is the application's job.
package hardware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/serialport"
	"github.com/bradsheets/timeblaster/internal/system"
)

// Nano is the command interface the rest of the application uses to drive the
// hardware. Every method is non-blocking: commands are queued, and a command
// issued while the Nano is unplugged is dropped with a debug log rather than
// failing a caller that has better things to worry about.
type Nano interface {
	// SetTime synchronises the Nano's clock.
	SetTime(t time.Time) error
	// SetAlarmActive tells the Nano whether an alarm is ringing.
	SetAlarmActive(active bool) error
	// ShowText displays literal text instead of the clock.
	ShowText(text string) error
	// ShowClock returns the display to normal.
	ShowClock() error
	// SetBrightness sets display brightness, 0-100.
	SetBrightness(pct int) error
	// SetLED sets a named LED.
	SetLED(name string, on bool) error
	// Connected reports whether the Nano is currently reachable.
	Connected() bool
}

// Observer receives hardware reports. The input router implements it.
type Observer interface {
	// PotReport delivers a raw potentiometer reading.
	PotReport(index, raw int)
	// ButtonEdge delivers a debounced button edge.
	ButtonEdge(name string, down bool)
}

// LifecycleHook is called when the link comes up or goes down, so the
// application can resync state and update health.
type LifecycleHook interface {
	// LinkUp is called after a successful connection, before any resync.
	LinkUp(device string)
	// LinkDown is called when the connection is lost.
	LinkDown(reason error)
}

// Config tunes the link.
type Config struct {
	// TimeSyncInterval is how often the Pi pushes the current time. The Nano
	// free-runs its display between syncs, so a minute is plenty; sending the
	// time every second would be pure waste.
	TimeSyncInterval time.Duration
	// HeartbeatTimeout is how long without a PING before the link is considered
	// dead and reopened.
	HeartbeatTimeout time.Duration
	// ReconnectMin and ReconnectMax bound the reconnect backoff.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// WriteQueueSize bounds buffered outbound messages.
	WriteQueueSize int
	// Brightness is applied to the display on every (re)connection.
	Brightness int
}

// DefaultConfig returns sensible link tuning.
func DefaultConfig() Config {
	return Config{
		TimeSyncInterval: time.Minute,
		HeartbeatTimeout: 8 * time.Second,
		ReconnectMin:     500 * time.Millisecond,
		ReconnectMax:     15 * time.Second,
		WriteQueueSize:   64,
		Brightness:       75,
	}
}

// Status describes the link for the health endpoint.
type Status struct {
	Connected       bool      `json:"connected"`
	Device          string    `json:"device,omitempty"`
	Firmware        string    `json:"firmware,omitempty"`
	ConnectedAt     time.Time `json:"connected_at,omitzero"`
	LastMessageAt   time.Time `json:"last_message_at,omitzero"`
	Reconnects      int       `json:"reconnects"`
	MessagesRx      int64     `json:"messages_received"`
	MessagesTx      int64     `json:"messages_sent"`
	MalformedFrames int64     `json:"malformed_frames"`
	DroppedWrites   int64     `json:"dropped_writes"`
	LastError       string    `json:"last_error,omitempty"`
}

// Link is a supervised session with the Nano.
type Link struct {
	cfg    Config
	opener serialport.Opener
	clock  system.Clock
	log    *slog.Logger

	observer  Observer
	lifecycle LifecycleHook

	// outbound carries queued messages to the writer. It is buffered; when full,
	// the oldest message is dropped rather than blocking a caller, because no
	// part of the application should stall waiting on a serial port.
	outbound chan protocol.Message
	seq      protocol.Sequencer

	mu     sync.RWMutex
	status Status
	// alarmActive and displayText hold the state to restore after a reconnect.
	alarmActive bool
	displayText string

	connected atomic.Bool
}

// Deps are the link's collaborators.
type Deps struct {
	Opener    serialport.Opener
	Clock     system.Clock
	Logger    *slog.Logger
	Observer  Observer
	Lifecycle LifecycleHook
}

// NewLink builds a link. Call Run to start the connect/reconnect loop.
func NewLink(cfg Config, d Deps) *Link {
	if cfg.WriteQueueSize <= 0 {
		cfg.WriteQueueSize = 64
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = 500 * time.Millisecond
	}
	if cfg.ReconnectMax < cfg.ReconnectMin {
		cfg.ReconnectMax = 15 * time.Second
	}
	return &Link{
		cfg:       cfg,
		opener:    d.Opener,
		clock:     d.Clock,
		log:       d.Logger,
		observer:  d.Observer,
		lifecycle: d.Lifecycle,
		outbound:  make(chan protocol.Message, cfg.WriteQueueSize),
	}
}

// SetObserver installs the input observer after construction.
func (l *Link) SetObserver(o Observer) {
	l.mu.Lock()
	l.observer = o
	l.mu.Unlock()
}

// SetLifecycle installs the lifecycle hook after construction.
func (l *Link) SetLifecycle(h LifecycleHook) {
	l.mu.Lock()
	l.lifecycle = h
	l.mu.Unlock()
}

// Connected reports whether the Nano is reachable.
func (l *Link) Connected() bool { return l.connected.Load() }

// Status returns a snapshot for the health endpoint.
func (l *Link) Status() Status {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := l.status
	s.Connected = l.connected.Load()
	return s
}

// Run maintains the connection until the context is cancelled.
//
// It always returns ctx.Err(). A Nano that never appears is a degraded
// Timeblaster — no knobs, no display — but alarms, the web UI and television
// playback all keep working, so this must never take the daemon down.
func (l *Link) Run(ctx context.Context) error {
	backoff := l.cfg.ReconnectMin
	l.log.Info("hardware link starting", "looking_in", l.opener.Describe(),
		"time_sync_interval", l.cfg.TimeSyncInterval, "heartbeat_timeout", l.cfg.HeartbeatTimeout)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		transport, device, err := l.opener.Open(ctx)
		if err != nil {
			l.recordError(err)
			if errors.Is(err, serialport.ErrNoDevice) {
				// Expected while the Nano is unplugged or still enumerating.
				l.log.Debug("no Nano present; will retry", "error", err, "retry_in", backoff)
			} else {
				l.log.Warn("could not open the Nano serial port", "error", err, "retry_in", backoff)
			}
			if !l.sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff, l.cfg.ReconnectMax)
			continue
		}

		l.log.Info("Nano connected", "device", device)
		backoff = l.cfg.ReconnectMin

		err = l.session(ctx, transport, device)
		_ = transport.Close()

		l.connected.Store(false)
		l.mu.Lock()
		l.status.Reconnects++
		l.mu.Unlock()

		if ctx.Err() != nil {
			l.notifyDown(ctx.Err())
			return ctx.Err()
		}
		l.recordError(err)
		l.notifyDown(err)
		l.log.Warn("Nano disconnected", "device", device, "error", err, "reconnect_in", backoff)

		if !l.sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = nextBackoff(backoff, l.cfg.ReconnectMax)
	}
}

// session runs one connected session, returning when it ends.
func (l *Link) session(ctx context.Context, t serialport.Transport, device string) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	now := l.clock.Now()
	l.mu.Lock()
	l.status.Device = device
	l.status.ConnectedAt = now
	l.status.LastMessageAt = now
	l.status.LastError = ""
	l.mu.Unlock()
	l.connected.Store(true)

	// Drain anything queued while disconnected: those commands describe a state
	// that may no longer be current, and resync below sends the truth anyway.
	l.drainQueue()
	l.notifyUp(device)
	l.Resync()

	var wg sync.WaitGroup
	readErr := make(chan error, 1)
	lastRx := make(chan time.Time, 64)

	wg.Add(1)
	go func() {
		defer wg.Done()
		readErr <- l.readLoop(sessionCtx, t, lastRx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.writeLoop(sessionCtx, t)
	}()

	err := l.supervise(sessionCtx, readErr, lastRx)
	cancel()
	// Closing the transport is what unblocks a reader parked in Read.
	_ = t.Close()
	wg.Wait()
	return err
}

// supervise drives time sync and heartbeat checking for one session.
func (l *Link) supervise(ctx context.Context, readErr <-chan error, lastRx <-chan time.Time) error {
	syncTimer := l.clock.NewTimer(l.cfg.TimeSyncInterval)
	defer syncTimer.Stop()

	// The heartbeat is checked on its own cadence rather than with a per-message
	// timer reset, so a burst of potentiometer reports cannot mask a Nano that
	// has stopped sending PINGs.
	checkInterval := l.cfg.HeartbeatTimeout / 2
	if checkInterval <= 0 {
		checkInterval = time.Second
	}
	beatTimer := l.clock.NewTimer(checkInterval)
	defer beatTimer.Stop()

	last := l.clock.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-readErr:
			if err == nil {
				return errors.New("hardware: the Nano closed the connection")
			}
			return err

		case t := <-lastRx:
			last = t

		case <-syncTimer.C():
			if err := l.SetTime(l.clock.Now()); err != nil {
				l.log.Debug("periodic time sync could not be queued", "error", err)
			}
			syncTimer.Reset(l.cfg.TimeSyncInterval)

		case <-beatTimer.C():
			if l.cfg.HeartbeatTimeout > 0 && l.clock.Since(last) > l.cfg.HeartbeatTimeout {
				return fmt.Errorf("hardware: no message from the Nano for %s",
					l.clock.Since(last).Round(time.Second))
			}
			beatTimer.Reset(checkInterval)
		}
	}
}

// readLoop parses inbound frames until the transport fails.
func (l *Link) readLoop(ctx context.Context, t serialport.Transport, lastRx chan<- time.Time) error {
	scanner := serialport.NewScanner(t)
	// Malformed frames are logged at most this often, so a peer speaking the
	// wrong protocol cannot fill the journal.
	const malformedLogInterval = 5 * time.Second
	lastMalformedLog := time.Time{}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now := l.clock.Now()

		msg, err := scanner.Message()
		if err != nil {
			l.mu.Lock()
			l.status.MalformedFrames++
			count := l.status.MalformedFrames
			l.mu.Unlock()
			if now.Sub(lastMalformedLog) > malformedLogInterval {
				lastMalformedLog = now
				l.log.Warn("discarding a malformed frame from the Nano",
					"error", err, "total_malformed", count, "frame", string(scanner.Bytes()))
			}
			continue
		}

		l.mu.Lock()
		l.status.MessagesRx++
		l.status.LastMessageAt = now
		l.mu.Unlock()

		select {
		case lastRx <- now:
		default: // the supervisor is behind; its own clock check still covers us
		}

		l.dispatch(msg)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("hardware: reading from the Nano: %w", err)
	}
	return nil
}

// dispatch routes one decoded message.
func (l *Link) dispatch(msg protocol.Message) {
	l.mu.RLock()
	obs := l.observer
	l.mu.RUnlock()

	switch msg.Type {
	case protocol.TypePing:
		// Answering keeps the Nano's own liveness check satisfied.
		l.enqueue(protocol.Pong(l.seq.Next()), false)

	case protocol.TypeHello:
		firmware, hardware := msg.Arg(0), msg.Arg(1)
		l.mu.Lock()
		l.status.Firmware = firmware
		l.mu.Unlock()
		l.log.Info("Nano announced itself", "firmware", firmware, "hardware", hardware)
		// A HELLO means the Nano rebooted and has forgotten everything.
		l.Resync()

	case protocol.TypePot:
		report, err := protocol.ParsePot(msg)
		if err != nil {
			l.log.Debug("ignoring a malformed potentiometer report", "error", err)
			return
		}
		if obs != nil {
			obs.PotReport(report.Index, report.Raw)
		}

	case protocol.TypeButton:
		report, err := protocol.ParseButton(msg)
		if err != nil {
			l.log.Debug("ignoring a malformed button report", "error", err)
			return
		}
		if obs != nil {
			obs.ButtonEdge(report.Name, report.Down)
		}

	case protocol.TypeLog:
		// Firmware diagnostics are surfaced but kept out of the way.
		l.log.Debug("nano log", "level", msg.Arg(0), "message", msg.Arg(1))

	case protocol.TypeAck:
		l.log.Debug("nano acknowledged a command", "seq", msg.Arg(0))

	case protocol.TypeNak:
		l.log.Warn("the Nano rejected a command", "seq", msg.Arg(0), "reason", msg.Arg(1))

	default:
		l.log.Debug("ignoring an unknown message type from the Nano", "type", msg.Type)
	}
}

// writeLoop drains the outbound queue.
func (l *Link) writeLoop(ctx context.Context, t serialport.Transport) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-l.outbound:
			if _, err := t.Write(protocol.Encode(msg)); err != nil {
				// The read loop will notice the same failure and end the session;
				// there is no need to escalate from here.
				l.log.Debug("write to the Nano failed", "type", msg.Type, "error", err)
				return
			}
			l.mu.Lock()
			l.status.MessagesTx++
			l.mu.Unlock()
			l.log.Debug("sent to the Nano", "message", msg.String())
		}
	}
}

// enqueue queues a message. critical messages displace an older queued message
// when the queue is full; non-critical ones are simply dropped.
func (l *Link) enqueue(msg protocol.Message, critical bool) error {
	if !l.connected.Load() {
		l.log.Debug("dropping a command because the Nano is not connected", "type", msg.Type)
		return ErrNotConnected
	}
	select {
	case l.outbound <- msg:
		return nil
	default:
	}

	l.mu.Lock()
	l.status.DroppedWrites++
	l.mu.Unlock()

	if !critical {
		l.log.Debug("outbound queue is full; dropping a command", "type", msg.Type)
		return ErrQueueFull
	}
	// Make room by discarding the oldest queued message. An alarm state change
	// must reach the Nano even if the queue is backed up with display updates.
	select {
	case <-l.outbound:
	default:
	}
	select {
	case l.outbound <- msg:
		l.log.Warn("outbound queue was full; dropped an older command to make room", "type", msg.Type)
		return nil
	default:
		return ErrQueueFull
	}
}

func (l *Link) drainQueue() {
	for {
		select {
		case <-l.outbound:
		default:
			return
		}
	}
}

// Errors returned by command methods.
var (
	// ErrNotConnected means the Nano is unplugged. Callers should treat it as
	// informational: the state will be resent on reconnect.
	ErrNotConnected = errors.New("hardware: the Nano is not connected")
	// ErrQueueFull means the outbound queue is saturated.
	ErrQueueFull = errors.New("hardware: outbound queue is full")
)

// Resync pushes the full authoritative state to the Nano.
//
// It runs on every connection and on every HELLO, which is what makes the
// unplug/replug path work without anyone having to think about it: the Nano
// comes back knowing nothing, and a moment later it knows the time, the alarm
// state, the brightness and what to display.
func (l *Link) Resync() {
	l.mu.RLock()
	alarmActive, text, brightness := l.alarmActive, l.displayText, l.cfg.Brightness
	l.mu.RUnlock()

	_ = l.SetTime(l.clock.Now())
	_ = l.SetBrightness(brightness)
	_ = l.SetAlarmActive(alarmActive)
	_ = l.SetLED(protocol.LEDAlarm, alarmActive)
	if text != "" {
		_ = l.ShowText(text)
	} else {
		_ = l.ShowClock()
	}
	l.log.Debug("resynchronised Nano state", "alarm_active", alarmActive, "display_text", text)
}

// SetTime synchronises the Nano's clock.
func (l *Link) SetTime(t time.Time) error {
	return l.enqueue(protocol.Time(l.seq.Next(), t), true)
}

// SetAlarmActive tells the Nano whether an alarm is ringing.
func (l *Link) SetAlarmActive(active bool) error {
	l.mu.Lock()
	l.alarmActive = active
	l.mu.Unlock()
	// Critical: the display and LEDs must reflect a dismissed alarm even under
	// queue pressure, or the Timeblaster looks like it is still going off.
	return l.enqueue(protocol.AlarmActive(l.seq.Next(), active), true)
}

// ShowText displays literal text instead of the clock.
func (l *Link) ShowText(text string) error {
	l.mu.Lock()
	l.displayText = text
	l.mu.Unlock()
	return l.enqueue(protocol.DisplayText(l.seq.Next(), text), true)
}

// ShowClock returns the display to normal.
func (l *Link) ShowClock() error {
	l.mu.Lock()
	l.displayText = ""
	l.mu.Unlock()
	return l.enqueue(protocol.DisplayClock(l.seq.Next()), true)
}

// SetBrightness sets display brightness, 0-100.
func (l *Link) SetBrightness(pct int) error {
	l.mu.Lock()
	l.cfg.Brightness = pct
	l.mu.Unlock()
	return l.enqueue(protocol.DisplayBrightness(l.seq.Next(), pct), false)
}

// SetLED sets a named LED.
func (l *Link) SetLED(name string, on bool) error {
	return l.enqueue(protocol.LED(l.seq.Next(), name, on), false)
}

func (l *Link) recordError(err error) {
	if err == nil {
		return
	}
	l.mu.Lock()
	l.status.LastError = err.Error()
	l.mu.Unlock()
}

func (l *Link) notifyUp(device string) {
	l.mu.RLock()
	h := l.lifecycle
	l.mu.RUnlock()
	if h != nil {
		h.LinkUp(device)
	}
}

func (l *Link) notifyDown(err error) {
	l.mu.RLock()
	h := l.lifecycle
	l.mu.RUnlock()
	if h != nil {
		h.LinkDown(err)
	}
}

func (l *Link) sleep(ctx context.Context, d time.Duration) bool {
	t := l.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C():
		return true
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

var _ Nano = (*Link)(nil)
