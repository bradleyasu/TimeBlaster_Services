// Package serialport isolates every detail of talking to a USB serial device.
//
// Everything above this package deals in TB1 messages, never in ports, globs or
// baud rates. That separation is what lets the whole Nano integration — framing,
// reconnects, time sync, input routing — be tested with an in-memory pipe and no
// hardware attached.
package serialport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// Transport is an open connection to the Nano.
type Transport io.ReadWriteCloser

// Opener establishes a connection. It is an interface so tests can supply a pipe.
type Opener interface {
	// Open returns a connected transport and the device path it used.
	Open(ctx context.Context) (Transport, string, error)
	// Describe returns a human-readable description of where it will look, for
	// log messages when nothing is found.
	Describe() string
}

// ErrNoDevice means no matching serial device is present. It is expected during
// boot and whenever the Nano is unplugged, so callers treat it as "retry later"
// rather than as a fault.
var ErrNoDevice = errors.New("serialport: no matching serial device")

// Config describes how to find and open the Nano.
type Config struct {
	// Device is an explicit path. When set, globs are ignored.
	Device string
	// Globs are candidate patterns, tried in order. Stable paths such as
	// /dev/serial/by-id/* are strongly preferred over /dev/ttyACM0, which is
	// assigned in enumeration order and changes when USB devices are replugged.
	Globs []string
	// BaudRate is the line speed. USB CDC ignores it, but a real UART would not.
	BaudRate int
	// ReadTimeout bounds a single read so a wedged port surfaces as an error
	// instead of a goroutine blocked forever.
	ReadTimeout time.Duration
}

// DeviceOpener opens a real serial port.
type DeviceOpener struct{ Config Config }

// Describe reports where the opener will look.
func (o DeviceOpener) Describe() string {
	if o.Config.Device != "" {
		return o.Config.Device
	}
	return strings.Join(o.Config.Globs, ", ")
}

// Open resolves and opens the port.
func (o DeviceOpener) Open(_ context.Context) (Transport, string, error) {
	path, err := ResolveDevice(o.Config.Device, o.Config.Globs)
	if err != nil {
		return nil, "", err
	}

	mode := &serial.Mode{
		BaudRate: o.Config.BaudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(path, mode)
	if err != nil {
		return nil, path, fmt.Errorf("serialport: opening %s: %w", path, err)
	}
	if o.Config.ReadTimeout > 0 {
		if err := port.SetReadTimeout(o.Config.ReadTimeout); err != nil {
			port.Close()
			return nil, path, fmt.Errorf("serialport: setting read timeout on %s: %w", path, err)
		}
	}
	// Assert DTR/RTS. The Nano ESP32 presents a USB CDC device; some hosts leave
	// the lines deasserted, which on ESP32-S3 boards can leave the peer waiting
	// for a host that appears absent.
	_ = port.SetDTR(true)
	_ = port.SetRTS(true)

	return port, path, nil
}

// ResolveDevice picks the serial device to use.
//
// An explicit path wins. Otherwise each glob is tried in order and, within a
// glob, matches are sorted so the choice is deterministic rather than dependent
// on directory order — an appliance that picks a different port on alternate
// boots is maddening to diagnose.
func ResolveDevice(explicit string, globs []string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%w: %s: %w", ErrNoDevice, explicit, err)
		}
		return explicit, nil
	}
	for _, pattern := range globs {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			// A malformed pattern is a configuration error, not a missing device.
			return "", fmt.Errorf("serialport: bad device glob %q: %w", pattern, err)
		}
		matches = filterCharDevices(matches)
		if len(matches) == 0 {
			continue
		}
		sort.Strings(matches)
		return matches[0], nil
	}
	return "", fmt.Errorf("%w: tried %s", ErrNoDevice, strings.Join(globs, ", "))
}

// filterCharDevices drops anything that is not a character device or a symlink to
// one, so a stray regular file in /dev/serial/by-id cannot be selected.
func filterCharDevices(paths []string) []string {
	out := paths[:0]
	for _, p := range paths {
		info, err := os.Stat(p) // follows symlinks, which by-id entries are
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ListPorts returns the serial ports currently present, for diagnostics and for
// the tbctl helper.
func ListPorts() ([]string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, fmt.Errorf("serialport: listing ports: %w", err)
	}
	sort.Strings(ports)
	return ports, nil
}

// --- test transport ----------------------------------------------------------

// Pipe is an in-memory bidirectional Transport for tests: writes to one end are
// readable at the other. It stands in for the USB cable.
type Pipe struct {
	rx *pipeBuffer // what this end reads
	tx *pipeBuffer // what this end writes
}

// NewPipePair returns two connected ends.
func NewPipePair() (*Pipe, *Pipe) {
	a := newPipeBuffer()
	b := newPipeBuffer()
	return &Pipe{rx: a, tx: b}, &Pipe{rx: b, tx: a}
}

// Read reads bytes written by the other end.
func (p *Pipe) Read(b []byte) (int, error) { return p.rx.Read(b) }

// Write sends bytes to the other end.
func (p *Pipe) Write(b []byte) (int, error) { return p.tx.Write(b) }

// Close closes both directions, which is what simulates unplugging the cable.
func (p *Pipe) Close() error {
	p.rx.Close()
	p.tx.Close()
	return nil
}

// pipeBuffer is a blocking byte queue.
type pipeBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newPipeBuffer() *pipeBuffer {
	b := &pipeBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *pipeBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.buf) == 0 && b.closed {
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

func (b *pipeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	b.buf = append(b.buf, p...)
	b.cond.Broadcast()
	return len(p), nil
}

func (b *pipeBuffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

// PipeOpener hands out pre-made pipes, simulating a device that appears,
// disappears and reappears.
type PipeOpener struct {
	mu    sync.Mutex
	queue []Transport
	// Err, when set, is returned instead of a transport.
	Err error
	// opens counts successful opens, so reconnect behaviour can be asserted.
	opens int
}

// NewPipeOpener returns an opener that will hand out the given transports in
// order, then report ErrNoDevice.
func NewPipeOpener(transports ...Transport) *PipeOpener {
	return &PipeOpener{queue: transports}
}

// Enqueue adds a transport to be returned by a future Open.
func (o *PipeOpener) Enqueue(t Transport) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queue = append(o.queue, t)
	o.Err = nil
}

// Open returns the next queued transport.
func (o *PipeOpener) Open(context.Context) (Transport, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Err != nil {
		return nil, "", o.Err
	}
	if len(o.queue) == 0 {
		return nil, "", ErrNoDevice
	}
	t := o.queue[0]
	o.queue = o.queue[1:]
	o.opens++
	return t, fmt.Sprintf("/dev/fake-nano-%d", o.opens), nil
}

// Describe reports the fake device name.
func (o *PipeOpener) Describe() string { return "in-memory pipe" }

// Opens reports how many transports have been handed out.
func (o *PipeOpener) Opens() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opens
}

// SetError makes subsequent opens fail.
func (o *PipeOpener) SetError(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Err = err
}

var (
	_ Opener    = DeviceOpener{}
	_ Opener    = (*PipeOpener)(nil)
	_ Transport = (*Pipe)(nil)
)
