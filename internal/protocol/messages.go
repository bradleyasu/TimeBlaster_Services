package protocol

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// Sequencer hands out per-direction sequence numbers. It is safe for concurrent use
// so that the writer goroutine and any synchronous callers can share one.
type Sequencer struct{ n atomic.Uint64 }

// Next returns the next sequence number in 0..MaxSeq-1.
func (s *Sequencer) Next() int { return int(s.n.Add(1)-1) % MaxSeq }

// --- Pi -> Nano constructors -------------------------------------------------

// Pong answers a Nano PING.
func Pong(seq int) Message {
	return Message{Seq: seq, Type: TypePong}
}

// Time builds a clock synchronisation message. The Nano uses the millisecond field
// to align its local second boundary, so its display advances in step with the Pi
// instead of drifting by up to a second on every resync.
func Time(seq int, t time.Time) Message {
	_, offset := t.Zone()
	return Message{
		Seq:  seq,
		Type: TypeTime,
		Args: []string{
			strconv.FormatInt(t.Unix(), 10),
			strconv.Itoa(offset),
			strconv.Itoa(t.Nanosecond() / int(time.Millisecond)),
		},
	}
}

// AlarmActive tells the Nano whether an alarm is currently ringing, which is all
// the Nano needs to know about the alarm system.
func AlarmActive(seq int, active bool) Message {
	return Message{Seq: seq, Type: TypeAlarm, Args: []string{"ACTIVE", boolArg(active)}}
}

// DisplayText asks the Nano to show literal text instead of the clock.
func DisplayText(seq int, text string) Message {
	return Message{Seq: seq, Type: TypeDisplay, Args: []string{DisplaySubText, text}}
}

// DisplayClock returns the Nano to its normal clock display.
func DisplayClock(seq int) Message {
	return Message{Seq: seq, Type: TypeDisplay, Args: []string{DisplaySubClock}}
}

// DisplayBrightness sets display brightness as a percentage (0..100).
func DisplayBrightness(seq int, pct int) Message {
	return Message{Seq: seq, Type: TypeDisplay, Args: []string{DisplaySubBrightness, strconv.Itoa(clampPct(pct))}}
}

// LED sets a named LED on or off.
func LED(seq int, name string, on bool) Message {
	return Message{Seq: seq, Type: TypeLED, Args: []string{name, boolArg(on)}}
}

// Config pushes a firmware tunable, for example the potentiometer report threshold.
func Config(seq int, key, value string) Message {
	return Message{Seq: seq, Type: TypeConfig, Args: []string{key, value}}
}

// ConfigClock24h selects 12- or 24-hour time on the 7-segment display.
func ConfigClock24h(seq int, on bool) Message {
	return Config(seq, ConfigKeyClock24h, boolArg(on))
}

// Ack positively acknowledges the peer's sequence number.
func Ack(seq, acked int) Message {
	return Message{Seq: seq, Type: TypeAck, Args: []string{strconv.Itoa(acked)}}
}

// Nak rejects the peer's sequence number with a human-readable reason.
func Nak(seq, rejected int, reason string) Message {
	return Message{Seq: seq, Type: TypeNak, Args: []string{strconv.Itoa(rejected), reason}}
}

// --- Nano -> Pi constructors (used by tests and the fake Nano) ---------------

// Hello announces firmware identity after a reset.
func Hello(seq int, firmware, hardware string) Message {
	return Message{Seq: seq, Type: TypeHello, Args: []string{firmware, hardware}}
}

// Ping is the Nano's liveness beacon.
func Ping(seq int, uptime time.Duration) Message {
	return Message{Seq: seq, Type: TypePing, Args: []string{strconv.FormatInt(uptime.Milliseconds(), 10)}}
}

// Pot reports a potentiometer reading.
func Pot(seq, index, raw int) Message {
	return Message{Seq: seq, Type: TypePot, Args: []string{strconv.Itoa(index), strconv.Itoa(raw)}}
}

// Button reports a button edge.
func Button(seq int, name string, down bool) Message {
	edge := EdgeUp
	if down {
		edge = EdgeDown
	}
	return Message{Seq: seq, Type: TypeButton, Args: []string{name, edge}}
}

// --- Typed parsing -----------------------------------------------------------

// PotReport is a decoded TypePot message.
type PotReport struct {
	Index int
	Raw   int
}

// ParsePot validates and extracts a potentiometer report.
func ParsePot(m Message) (PotReport, error) {
	if m.Type != TypePot {
		return PotReport{}, fmt.Errorf("protocol: expected %s, got %s", TypePot, m.Type)
	}
	idx, err := m.IntArg(0)
	if err != nil {
		return PotReport{}, err
	}
	raw, err := m.IntArg(1)
	if err != nil {
		return PotReport{}, err
	}
	if idx < 0 {
		return PotReport{}, fmt.Errorf("%w: negative pot index %d", ErrInvalidArgType, idx)
	}
	return PotReport{Index: idx, Raw: raw}, nil
}

// ButtonReport is a decoded TypeButton message.
type ButtonReport struct {
	Name string
	Down bool
}

// ParseButton validates and extracts a button edge report.
func ParseButton(m Message) (ButtonReport, error) {
	if m.Type != TypeButton {
		return ButtonReport{}, fmt.Errorf("protocol: expected %s, got %s", TypeButton, m.Type)
	}
	if len(m.Args) < 2 {
		return ButtonReport{}, fmt.Errorf("%w: %s needs name and edge", ErrMissingArgs, TypeButton)
	}
	name := m.Args[0]
	if name == "" {
		return ButtonReport{}, fmt.Errorf("%w: empty button name", ErrInvalidArgType)
	}
	switch m.Args[1] {
	case EdgeDown:
		return ButtonReport{Name: name, Down: true}, nil
	case EdgeUp:
		return ButtonReport{Name: name, Down: false}, nil
	default:
		return ButtonReport{}, fmt.Errorf("%w: unknown button edge %q", ErrInvalidArgType, m.Args[1])
	}
}

func boolArg(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func clampPct(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
