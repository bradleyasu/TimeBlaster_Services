package protocol

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []Message{
		{Seq: 0, Type: TypePing, Args: []string{"1234"}},
		{Seq: 4095, Type: TypePot, Args: []string{"0", "742"}},
		{Seq: 7, Type: TypeButton, Args: []string{ButtonAlarmOff, EdgeDown}},
		{Seq: 12, Type: TypePong},
		{Seq: 99, Type: TypeDisplay, Args: []string{DisplaySubText, "SETUP"}},
		// Arguments containing every character that would otherwise break framing.
		{Seq: 1, Type: TypeLog, Args: []string{"warn", "a|b\\c\nd\re" + string(rune(STX))}},
		{Seq: 2, Type: TypeConfig, Args: []string{"pot_threshold", ""}},
	}
	for _, want := range cases {
		t.Run(string(want.Type), func(t *testing.T) {
			frame := Encode(want)
			if frame[0] != STX || frame[len(frame)-1] != ETX {
				t.Fatalf("frame is not delimited: %q", frame)
			}
			got, err := Decode(frame[1 : len(frame)-1])
			if err != nil {
				t.Fatalf("Decode(%q): %v", frame, err)
			}
			if got.Seq != want.Seq || got.Type != want.Type {
				t.Fatalf("header mismatch: got %+v want %+v", got, want)
			}
			if len(got.Args) != len(want.Args) {
				t.Fatalf("arg count: got %v want %v", got.Args, want.Args)
			}
			for i := range want.Args {
				if got.Args[i] != want.Args[i] {
					t.Errorf("arg %d: got %q want %q", i, got.Args[i], want.Args[i])
				}
			}
		})
	}
}

func TestEncodeWireFormat(t *testing.T) {
	// Pin the exact bytes so a firmware change that breaks compatibility fails here
	// rather than on the bench.
	got := string(Encode(Message{Seq: 3, Type: TypePot, Args: []string{"0", "742"}}))
	const body = "TB1|3|POT|0|742"
	want := string(rune(STX)) + body + "|" + checksumHex(body) + "\n"
	if got != want {
		t.Fatalf("wire format drift:\n got %q\nwant %q", got, want)
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	good := Encode(Message{Seq: 5, Type: TypePot, Args: []string{"1", "331"}})
	body := string(good[1 : len(good)-1])

	tests := []struct {
		name  string
		frame string
		want  error
	}{
		{"empty", "", ErrEmpty},
		{"too few fields", "TB1|1|X", ErrTooShort},
		{"bad version", strings.Replace(body, "TB1", "TB9", 1), ErrBadVersion},
		{"flipped bit in payload", strings.Replace(body, "331", "332", 1), ErrBadChecksum},
		{"truncated checksum", body[:len(body)-1], ErrBadChecksum},
		{"oversized", strings.Repeat("x", MaxFrameLen+1), ErrTooLong},
		{"dangling escape", "TB1|1|LOG|bad\\", ErrBadEscape},
		{"unknown escape", "TB1|1|LOG|ba\\qd|00", ErrBadEscape},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.frame)); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeRejectsBadSequence(t *testing.T) {
	// Build a frame with a valid checksum but an out-of-range sequence number, so we
	// know the sequence check and not the checksum is what rejects it.
	body := "TB1|99999|PONG"
	frame := body + "|" + checksumHex(body)
	if _, err := Decode([]byte(frame)); !errors.Is(err, ErrBadSequence) {
		t.Fatalf("got %v, want ErrBadSequence", err)
	}
}

func TestTimeMessageCarriesOffsetAndMillis(t *testing.T) {
	loc := time.FixedZone("TEST", -5*3600)
	when := time.Date(2026, 9, 18, 6, 30, 15, 250*int(time.Millisecond), loc)

	m := Time(1, when)
	if m.Type != TypeTime {
		t.Fatalf("type: %s", m.Type)
	}
	unix, err := m.Int64Arg(0)
	if err != nil || unix != when.Unix() {
		t.Fatalf("unix: %d, %v", unix, err)
	}
	if off, _ := m.IntArg(1); off != -5*3600 {
		t.Errorf("utc offset: got %d want %d", off, -5*3600)
	}
	if ms, _ := m.IntArg(2); ms != 250 {
		t.Errorf("millis: got %d want 250", ms)
	}
}

func TestParsePot(t *testing.T) {
	got, err := ParsePot(Pot(1, 2, 4095))
	if err != nil {
		t.Fatalf("ParsePot: %v", err)
	}
	if got.Index != 2 || got.Raw != 4095 {
		t.Fatalf("got %+v", got)
	}

	for _, bad := range []Message{
		{Type: TypePot},
		{Type: TypePot, Args: []string{"0"}},
		{Type: TypePot, Args: []string{"x", "1"}},
		{Type: TypePot, Args: []string{"-1", "1"}},
		{Type: TypePing, Args: []string{"0", "1"}},
	} {
		if _, err := ParsePot(bad); err == nil {
			t.Errorf("ParsePot(%v) should have failed", bad)
		}
	}
}

func TestParseButton(t *testing.T) {
	down, err := ParseButton(Button(1, ButtonWiFi, true))
	if err != nil || !down.Down || down.Name != ButtonWiFi {
		t.Fatalf("down: %+v, %v", down, err)
	}
	up, err := ParseButton(Button(2, ButtonAlarmOff, false))
	if err != nil || up.Down || up.Name != ButtonAlarmOff {
		t.Fatalf("up: %+v, %v", up, err)
	}
	for _, bad := range []Message{
		{Type: TypeButton, Args: []string{ButtonWiFi}},
		{Type: TypeButton, Args: []string{ButtonWiFi, "SIDEWAYS"}},
		{Type: TypeButton, Args: []string{"", EdgeUp}},
	} {
		if _, err := ParseButton(bad); err == nil {
			t.Errorf("ParseButton(%v) should have failed", bad)
		}
	}
}

func TestSequencerWrapsAndIsMonotonic(t *testing.T) {
	var s Sequencer
	for i := range MaxSeq {
		if got := s.Next(); got != i {
			t.Fatalf("Next() #%d = %d", i, got)
		}
	}
	if got := s.Next(); got != 0 {
		t.Fatalf("sequence did not wrap: %d", got)
	}
}

func TestEncodeClampsSequence(t *testing.T) {
	m, err := Decode(trim(Encode(Message{Seq: MaxSeq + 5, Type: TypePong})))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.Seq != 5 {
		t.Fatalf("seq: got %d want 5", m.Seq)
	}
}

func TestCRC8KnownVectors(t *testing.T) {
	// CRC-8/ATM (poly 0x07, init 0x00) check value for "123456789" is 0xF4.
	if got := CRC8([]byte("123456789")); got != 0xF4 {
		t.Fatalf("CRC8 check value: got %#02x want 0xF4", got)
	}
	if got := CRC8(nil); got != 0x00 {
		t.Fatalf("CRC8(nil): got %#02x want 0x00", got)
	}
}

func TestDisplayBrightnessClamps(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"-10", "0"}, {"0", "0"}, {"75", "75"}, {"100", "100"}, {"250", "100"},
	} {
		var v int
		_, _ = fmtSscan(tc.in, &v)
		if got := DisplayBrightness(0, v).Arg(1); got != tc.want {
			t.Errorf("brightness %s: got %s want %s", tc.in, got, tc.want)
		}
	}
}

func trim(frame []byte) []byte { return frame[1 : len(frame)-1] }

func fmtSscan(s string, v *int) (int, error) {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	*v = n
	return 1, nil
}

// firmwareCRC8 is a line-for-line transcription of the bitwise CRC in
// firmware/timeblaster-nano/timeblaster-nano.ino. The wire format is implemented
// twice — once in Go, once in C++ — and this test pins them together so a change
// to either fails here rather than on the bench with a Timeblaster that has
// stopped responding to its own knobs.
func firmwareCRC8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc ^= b
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func TestCRCMatchesTheFirmwareImplementation(t *testing.T) {
	inputs := [][]byte{
		nil,
		[]byte(""),
		[]byte("TB1|0|PING|1234"),
		[]byte("TB1|4095|POT|0|742"),
		[]byte("TB1|7|DISPLAY|TEXT|SETUP"),
		[]byte("TB1|1|TIME|1758220642|-18000|250"),
		[]byte("TB1|2|LOG|warn|a\\pb\\nc"),
		{0x00, 0xFF, 0x80, 0x7F, 0x01},
	}
	for _, in := range inputs {
		if got, want := CRC8(in), firmwareCRC8(in); got != want {
			t.Errorf("CRC8(%q) = %#02x, firmware computes %#02x", in, got, want)
		}
	}

	// And across every single byte value, since the table is built once and a
	// transcription error would otherwise hide in an untested entry.
	for i := range 256 {
		b := []byte{byte(i)}
		if got, want := CRC8(b), firmwareCRC8(b); got != want {
			t.Fatalf("CRC8([%d]) = %#02x, firmware computes %#02x", i, got, want)
		}
	}
}
