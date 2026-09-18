// Package protocol implements the TB1 wire format spoken between the Raspberry Pi
// and the Arduino Nano ESP32 over USB CDC serial.
//
// The package is deliberately pure: it performs no I/O, knows nothing about serial
// ports, and has no dependencies outside the standard library. That makes the entire
// wire format unit-testable on a development machine.
//
// Frame layout:
//
//	<STX> TB1 | SEQ | TYPE | arg | arg ... | CRC <LF>
//	 0x02 ^------------ checksummed region ------^   0x0A
//
// The checksum is a CRC-8 (polynomial 0x07, init 0x00) over every byte between the
// STX and the final field separator that precedes the CRC field, hex encoded as two
// uppercase characters.
//
// See docs/serial-protocol.md for the full specification.
package protocol

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Framing constants.
const (
	// STX marks the beginning of a frame. Any bytes received before an STX are
	// discarded, which is what allows a reader to resynchronise mid-stream after a
	// reconnect or a partially transmitted frame.
	STX = 0x02
	// ETX marks the end of a frame. A plain newline keeps frames readable in a
	// terminal, which matters a lot when debugging with `screen` or `picocom`.
	ETX = '\n'

	// Version is the protocol version literal carried as field 0 of every frame.
	Version = "TB1"

	// FieldSep separates fields within a frame.
	FieldSep = '|'

	// MaxFrameLen bounds how many bytes we will buffer while looking for an ETX.
	// A frame longer than this is treated as line noise and discarded, which stops a
	// stuck or garbage-emitting peer from growing the reader's buffer without bound.
	MaxFrameLen = 512

	// MaxSeq is the exclusive upper bound of the per-direction sequence counter.
	MaxSeq = 4096
)

// Type identifies the kind of a message. Types are shared across both directions;
// the direction is implied by which peer legitimately sends them.
type Type string

// Messages sent by the Nano to the Pi.
const (
	// TypeHello is sent once when the Nano boots or (re)enumerates. Args:
	// firmware version, hardware id.
	TypeHello Type = "HELLO"
	// TypePing is the Nano's liveness beacon. Args: uptime milliseconds.
	TypePing Type = "PING"
	// TypePot reports a filtered potentiometer reading. Args: index, raw value.
	TypePot Type = "POT"
	// TypeButton reports a debounced button edge. Args: button name, DOWN|UP.
	TypeButton Type = "BUTTON"
	// TypeLog carries a diagnostic line from the firmware. Args: level, text.
	TypeLog Type = "LOG"
)

// Messages sent by the Pi to the Nano.
const (
	// TypePong answers TypePing. Args: none.
	TypePong Type = "PONG"
	// TypeTime synchronises the Nano's local clock. Args: unix seconds, UTC offset
	// seconds, milliseconds elapsed into the current second.
	TypeTime Type = "TIME"
	// TypeAlarm communicates alarm state. Args: ACTIVE, 0|1.
	TypeAlarm Type = "ALARM"
	// TypeDisplay controls the 7-segment display. Args: TEXT|BRIGHTNESS|CLOCK, value.
	TypeDisplay Type = "DISPLAY"
	// TypeLED controls a named LED. Args: led name, 0|1.
	TypeLED Type = "LED"
	// TypeConfig pushes firmware-side tunables. Args: key, value.
	TypeConfig Type = "CONFIG"
)

// Messages used in both directions.
const (
	// TypeAck positively acknowledges a sequence number. Args: acked sequence.
	TypeAck Type = "ACK"
	// TypeNak rejects a sequence number. Args: rejected sequence, reason.
	TypeNak Type = "NAK"
)

// Display subcommands carried as the first argument of TypeDisplay.
const (
	DisplaySubText       = "TEXT"
	DisplaySubBrightness = "BRIGHTNESS"
	DisplaySubClock      = "CLOCK" // return to showing the clock
)

// Button names. The Nano reports logical names rather than pin numbers so that
// rewiring the hardware never requires a change on the Pi.
const (
	ButtonWiFi     = "WIFI"
	ButtonAlarmOff = "ALARM_OFF"
)

// Button edge values.
const (
	EdgeDown = "DOWN"
	EdgeUp   = "UP"
)

// LED names.
const (
	LEDAlarm = "ALARM"
	LEDWiFi  = "WIFI"
	LEDPower = "POWER"
)

// Errors returned by Decode. They are all recoverable: a caller should count and
// drop the offending frame, never terminate the connection.
var (
	ErrEmpty          = errors.New("protocol: empty frame")
	ErrTooShort       = errors.New("protocol: frame has too few fields")
	ErrBadVersion     = errors.New("protocol: unsupported protocol version")
	ErrBadSequence    = errors.New("protocol: malformed sequence number")
	ErrBadChecksum    = errors.New("protocol: checksum mismatch")
	ErrTooLong        = errors.New("protocol: frame exceeds maximum length")
	ErrBadEscape      = errors.New("protocol: malformed escape sequence")
	ErrMissingArgs    = errors.New("protocol: message is missing required arguments")
	ErrInvalidArgType = errors.New("protocol: argument has the wrong type")
)

// Message is a decoded TB1 frame.
type Message struct {
	// Seq is the sender's per-direction sequence number (0..MaxSeq-1).
	Seq int
	// Type is the message type.
	Type Type
	// Args are the type-specific arguments, already unescaped.
	Args []string
}

// Arg returns argument i, or "" when it is absent. Callers that require an
// argument should use the typed accessors below instead.
func (m Message) Arg(i int) string {
	if i < 0 || i >= len(m.Args) {
		return ""
	}
	return m.Args[i]
}

// IntArg returns argument i parsed as an integer.
func (m Message) IntArg(i int) (int, error) {
	if i >= len(m.Args) {
		return 0, fmt.Errorf("%w: wanted arg %d of %s", ErrMissingArgs, i, m.Type)
	}
	v, err := strconv.Atoi(strings.TrimSpace(m.Args[i]))
	if err != nil {
		return 0, fmt.Errorf("%w: arg %d of %s is %q", ErrInvalidArgType, i, m.Type, m.Args[i])
	}
	return v, nil
}

// Int64Arg returns argument i parsed as a 64-bit integer, which the TIME message
// needs for unix timestamps on 32-bit builds.
func (m Message) Int64Arg(i int) (int64, error) {
	if i >= len(m.Args) {
		return 0, fmt.Errorf("%w: wanted arg %d of %s", ErrMissingArgs, i, m.Type)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(m.Args[i]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: arg %d of %s is %q", ErrInvalidArgType, i, m.Type, m.Args[i])
	}
	return v, nil
}

// String renders the message in its wire form, which is also exactly what we want
// in debug logs.
func (m Message) String() string { return string(Encode(m)) }

// Encode serialises a message into a complete frame including STX, checksum and
// terminating newline. It never fails: arguments are escaped as needed.
func Encode(m Message) []byte {
	var body strings.Builder
	body.WriteString(Version)
	body.WriteByte(FieldSep)
	body.WriteString(strconv.Itoa(m.Seq % MaxSeq))
	body.WriteByte(FieldSep)
	body.WriteString(string(m.Type))
	for _, a := range m.Args {
		body.WriteByte(FieldSep)
		writeEscaped(&body, a)
	}

	payload := body.String()
	out := make([]byte, 0, len(payload)+8)
	out = append(out, STX)
	out = append(out, payload...)
	out = append(out, FieldSep)
	out = append(out, checksumHex(payload)...)
	out = append(out, ETX)
	return out
}

// Decode parses one frame body. The input must not contain the leading STX or the
// trailing newline; splitting frames out of a byte stream is the reader's job (see
// serialport.Scanner), which keeps this function trivially testable.
func Decode(frame []byte) (Message, error) {
	if len(frame) == 0 {
		return Message{}, ErrEmpty
	}
	if len(frame) > MaxFrameLen {
		return Message{}, ErrTooLong
	}

	// Split on unescaped separators only, so that escaped '|' inside display text
	// survives the round trip.
	fields, err := splitEscaped(string(frame))
	if err != nil {
		return Message{}, err
	}
	// version | seq | type | crc  == 4 minimum
	if len(fields) < 4 {
		return Message{}, ErrTooShort
	}

	if fields[0] != Version {
		return Message{}, fmt.Errorf("%w: %q", ErrBadVersion, fields[0])
	}

	// The checksum covers everything up to (but excluding) the separator before it.
	crcField := fields[len(fields)-1]
	covered := string(frame[:len(frame)-len(crcField)-1])
	if want := checksumHex(covered); !strings.EqualFold(crcField, want) {
		return Message{}, fmt.Errorf("%w: got %q want %q", ErrBadChecksum, crcField, want)
	}

	seq, err := strconv.Atoi(fields[1])
	if err != nil || seq < 0 || seq >= MaxSeq {
		return Message{}, fmt.Errorf("%w: %q", ErrBadSequence, fields[1])
	}

	msg := Message{
		Seq:  seq,
		Type: Type(fields[2]),
		Args: fields[3 : len(fields)-1],
	}
	if len(msg.Args) == 0 {
		msg.Args = nil
	}
	return msg, nil
}

// writeEscaped appends s to b, backslash-escaping the bytes that would otherwise
// break framing.
func writeEscaped(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case FieldSep:
			b.WriteString(`\p`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case STX:
			b.WriteString(`\s`)
		default:
			b.WriteByte(c)
		}
	}
}

// splitEscaped splits a frame body on unescaped field separators and unescapes each
// resulting field.
func splitEscaped(s string) ([]string, error) {
	var (
		fields []string
		cur    strings.Builder
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			i++
			if i >= len(s) {
				return nil, ErrBadEscape
			}
			switch s[i] {
			case '\\':
				cur.WriteByte('\\')
			case 'p':
				cur.WriteByte(FieldSep)
			case 'n':
				cur.WriteByte('\n')
			case 'r':
				cur.WriteByte('\r')
			case 's':
				cur.WriteByte(STX)
			default:
				return nil, fmt.Errorf("%w: \\%c", ErrBadEscape, s[i])
			}
		case c == FieldSep:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	fields = append(fields, cur.String())
	return fields, nil
}

// crc8Table is the precomputed table for CRC-8 with polynomial 0x07.
var crc8Table = func() [256]byte {
	var t [256]byte
	for i := range t {
		c := byte(i)
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

// CRC8 computes the frame checksum. Exported so the firmware's implementation can be
// cross-checked against it in tests and so tooling can build frames by hand.
func CRC8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc = crc8Table[crc^b]
	}
	return crc
}

func checksumHex(s string) string {
	return fmt.Sprintf("%02X", CRC8([]byte(s)))
}
