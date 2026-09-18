// Package mpv drives mpv through its JSON IPC interface and keeps the process
// alive.
//
// Timeblaster runs two independent mpv instances: one for television video on
// HDMI via DRM/KMS, and one audio-only instance for the alarm speaker on USB.
// They share this code but nothing else — an alarm must be able to ring while
// the TV instance is crashed, and a crashed alarm player must not disturb video.
//
// Channel changes are a `loadfile` command over IPC rather than a process
// restart, which is both much faster and avoids a black screen and an HDMI mode
// renegotiation on every turn of the channel knob.
package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Controller is what the rest of the application uses to drive a player. Both the
// real client and the test fake implement it.
type Controller interface {
	// Command sends a raw mpv command and returns its data payload.
	Command(ctx context.Context, args ...any) (json.RawMessage, error)
	// SetProperty sets an mpv property.
	SetProperty(ctx context.Context, name string, value any) error
	// GetProperty reads an mpv property into out.
	GetProperty(ctx context.Context, name string, out any) error
	// LoadFile replaces what is playing.
	LoadFile(ctx context.Context, url string) error
	// Alive reports whether the IPC connection is usable.
	Alive() bool
}

// Event is an asynchronous notification from mpv, such as end-of-file or a
// property change. Timeblaster uses these to notice a dead stream without
// polling.
type Event struct {
	Name string          `json:"event"`
	ID   int             `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	// Reason accompanies end-file: eof, stop, quit, error, redirect, unknown.
	Reason string `json:"reason,omitempty"`
	// Error accompanies a failed end-file.
	Error string `json:"file_error,omitempty"`
}

// Errors returned by the client.
var (
	// ErrNotConnected means the IPC socket is not currently usable. Callers should
	// treat it as transient: the supervisor is already working on reconnecting.
	ErrNotConnected = errors.New("mpv: not connected")
	// ErrClosed means the client was shut down deliberately.
	ErrClosed = errors.New("mpv: client closed")
)

// CommandError is an error reported by mpv itself, as opposed to a transport
// failure. It is distinguished because "mpv said property unavailable" is normal
// and should not trigger a restart, whereas a transport failure should.
type CommandError struct {
	Command string
	Reason  string
}

func (e *CommandError) Error() string { return fmt.Sprintf("mpv: %s: %s", e.Command, e.Reason) }

// response is one line read from the IPC socket.
type response struct {
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
	RequestID int64           `json:"request_id"`
	Event     string          `json:"event"`
	ID        int             `json:"id"`
	Reason    string          `json:"reason"`
	FileError string          `json:"file_error"`
}

// Client is a connected mpv JSON IPC session.
type Client struct {
	log     *slog.Logger
	timeout time.Duration

	conn net.Conn
	// writeMu serialises writes; mpv's protocol is line oriented and interleaved
	// partial writes would corrupt it.
	writeMu sync.Mutex

	nextID  atomic.Int64
	pending sync.Map // int64 -> chan response

	events chan Event

	closeOnce sync.Once
	closed    chan struct{}
	// readErr records why the read loop ended, so Alive and subsequent commands
	// can report something more useful than "not connected".
	readErr atomic.Pointer[error]
}

// Dial connects to an mpv IPC socket.
func Dial(ctx context.Context, socket string, timeout time.Duration, log *slog.Logger) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("mpv: connecting to %s: %w", socket, err)
	}
	c := &Client{
		log:     log,
		timeout: timeout,
		conn:    conn,
		events:  make(chan Event, 64),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Events returns the asynchronous event stream. The channel is closed when the
// client shuts down. Events are dropped rather than buffered without bound if the
// consumer falls behind: losing a property-change notification is survivable,
// blocking the read loop is not.
func (c *Client) Events() <-chan Event { return c.events }

// Alive reports whether commands can currently be sent.
func (c *Client) Alive() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}

// Close shuts the client down. It is safe to call more than once.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.conn.Close()
	})
	return err
}

func (c *Client) readLoop() {
	defer func() {
		c.Close()
		close(c.events)
		// Fail every in-flight request so no caller waits for its timeout.
		c.pending.Range(func(key, value any) bool {
			c.pending.Delete(key)
			close(value.(chan response))
			return true
		})
	}()

	scanner := bufio.NewScanner(c.conn)
	// mpv can emit long property payloads (a full track list, say); the default
	// 64 KiB token limit is not always enough.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var r response
		if err := json.Unmarshal(line, &r); err != nil {
			// A malformed line is logged and skipped. It must never kill the
			// connection: the TV would go black over a cosmetic parse failure.
			c.log.Debug("ignoring unparsable mpv IPC line", "error", err, "line", string(line))
			continue
		}

		if r.Event != "" {
			select {
			case c.events <- Event{Name: r.Event, ID: r.ID, Data: r.Data, Reason: r.Reason, Error: r.FileError}:
			default:
				c.log.Debug("dropping mpv event; consumer is behind", "event", r.Event)
			}
			continue
		}

		if ch, ok := c.pending.LoadAndDelete(r.RequestID); ok {
			ch.(chan response) <- r
			close(ch.(chan response))
		}
	}
	if err := scanner.Err(); err != nil {
		c.log.Debug("mpv IPC read loop ended", "error", err)
		c.readErr.Store(&err)
	}
}

// Command sends a command and waits for its reply.
func (c *Client) Command(ctx context.Context, args ...any) (json.RawMessage, error) {
	if !c.Alive() {
		return nil, ErrNotConnected
	}

	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	payload, err := json.Marshal(struct {
		Command   []any `json:"command"`
		RequestID int64 `json:"request_id"`
	}{Command: args, RequestID: id})
	if err != nil {
		return nil, fmt.Errorf("mpv: encoding command: %w", err)
	}
	payload = append(payload, '\n')

	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	_, werr := c.conn.Write(payload)
	c.writeMu.Unlock()
	if werr != nil {
		c.Close()
		return nil, fmt.Errorf("mpv: writing command: %w", werr)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	select {
	case <-timeoutCtx.Done():
		return nil, fmt.Errorf("mpv: command %v: %w", args, timeoutCtx.Err())
	case <-c.closed:
		return nil, ErrNotConnected
	case r, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		if r.Error != "" && r.Error != "success" {
			return r.Data, &CommandError{Command: commandName(args), Reason: r.Error}
		}
		return r.Data, nil
	}
}

// SetProperty sets an mpv property.
func (c *Client) SetProperty(ctx context.Context, name string, value any) error {
	_, err := c.Command(ctx, "set_property", name, value)
	return err
}

// GetProperty reads an mpv property into out.
func (c *Client) GetProperty(ctx context.Context, name string, out any) error {
	data, err := c.Command(ctx, "get_property", name)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("mpv: decoding property %s: %w", name, err)
	}
	return nil
}

// LoadFile replaces what is playing. "replace" rather than "append" is what makes
// a channel change instant instead of queueing behind the current stream.
func (c *Client) LoadFile(ctx context.Context, url string) error {
	_, err := c.Command(ctx, "loadfile", url, "replace")
	return err
}

// ObserveProperty asks mpv to send a property-change event whenever name changes.
// Timeblaster observes playback state this way instead of polling, which keeps
// the daemon idle while a channel plays.
func (c *Client) ObserveProperty(ctx context.Context, id int, name string) error {
	_, err := c.Command(ctx, "observe_property", id, name)
	return err
}

func commandName(args []any) string {
	if len(args) == 0 {
		return "(empty)"
	}
	if s, ok := args[0].(string); ok {
		return s
	}
	return fmt.Sprint(args[0])
}

var _ Controller = (*Client)(nil)
