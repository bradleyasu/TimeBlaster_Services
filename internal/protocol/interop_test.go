package protocol

import (
	"bytes"
	"os"
	"testing"
)

// The TB1 wire format is implemented twice: here in Go, and in C++ in
// firmware/timeblaster-nano/src/Protocol.cpp. These tests hold the two
// together.
//
// TestCRCMatchesTheFirmwareImplementation (in protocol_test.go) covers the
// checksum. This file covers the whole frame: real bytes produced by the
// firmware's encoder are decoded here, and the same messages are re-encoded and
// compared byte for byte.
//
// Regenerate the fixture with: make -C firmware/timeblaster-nano fixture

const firmwareFixture = "testdata/firmware-frames.bin"

// wantFrames is what the firmware emitted, in order. Keeping the expectation
// here rather than deriving it from the fixture means a corrupted fixture is
// caught too.
var wantFrames = []Message{
	{Seq: 0, Type: TypeHello, Args: []string{"1.0.0", "nano-esp32"}},
	{Seq: 1, Type: TypePing, Args: []string{"12345"}},
	{Seq: 2, Type: TypePot, Args: []string{"0", "742"}},
	{Seq: 3, Type: TypePot, Args: []string{"3", "4095"}},
	{Seq: 4, Type: TypeButton, Args: []string{ButtonWiFi, EdgeDown}},
	{Seq: 5, Type: TypeButton, Args: []string{ButtonAlarmOff, EdgeUp}},
	// The escaping case: a separator, a backslash and a newline inside one
	// argument, none of which may break framing.
	{Seq: 6, Type: TypeLog, Args: []string{"warn", "a|b\\c\nd"}},
}

// splitFrames extracts frame bodies from a raw capture the way the reader does:
// everything before an STX is discarded, and a frame ends at the next newline.
func splitFrames(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for i := 0; i < len(raw); {
		start := bytes.IndexByte(raw[i:], STX)
		if start < 0 {
			break
		}
		i += start + 1
		end := bytes.IndexByte(raw[i:], ETX)
		if end < 0 {
			t.Fatalf("unterminated frame at offset %d", i)
		}
		out = append(out, raw[i:i+end])
		i += end + 1
	}
	return out
}

func TestDecodesRealFirmwareOutput(t *testing.T) {
	raw, err := os.ReadFile(firmwareFixture)
	if err != nil {
		t.Fatalf("reading the firmware fixture: %v", err)
	}

	frames := splitFrames(t, raw)
	if len(frames) != len(wantFrames) {
		t.Fatalf("got %d frames, want %d", len(frames), len(wantFrames))
	}

	for i, body := range frames {
		got, err := Decode(body)
		if err != nil {
			t.Fatalf("frame %d (%q): %v", i, body, err)
		}
		want := wantFrames[i]

		if got.Seq != want.Seq || got.Type != want.Type {
			t.Errorf("frame %d header: got seq=%d type=%s, want seq=%d type=%s",
				i, got.Seq, got.Type, want.Seq, want.Type)
			continue
		}
		if len(got.Args) != len(want.Args) {
			t.Errorf("frame %d args: got %q, want %q", i, got.Args, want.Args)
			continue
		}
		for j := range want.Args {
			if got.Args[j] != want.Args[j] {
				t.Errorf("frame %d arg %d: got %q, want %q", i, j, got.Args[j], want.Args[j])
			}
		}
	}
}

func TestGoEncoderMatchesTheFirmwareByteForByte(t *testing.T) {
	raw, err := os.ReadFile(firmwareFixture)
	if err != nil {
		t.Fatalf("reading the firmware fixture: %v", err)
	}

	var rebuilt bytes.Buffer
	for _, m := range wantFrames {
		rebuilt.Write(Encode(m))
	}

	if !bytes.Equal(rebuilt.Bytes(), raw) {
		t.Errorf("the Go encoder and the firmware encoder disagree\n got  %q\n want %q",
			rebuilt.String(), string(raw))
	}
}

func TestFirmwareEscapingSurvivesTheRoundTrip(t *testing.T) {
	// The argument containing a separator, a backslash and a newline is the one
	// that would silently corrupt the stream if either side got escaping wrong.
	raw, err := os.ReadFile(firmwareFixture)
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(t, raw)

	last, err := Decode(frames[len(frames)-1])
	if err != nil {
		t.Fatalf("decoding the escaped frame: %v", err)
	}
	if got := last.Arg(1); got != "a|b\\c\nd" {
		t.Errorf("escaped argument: got %q", got)
	}

	// The encoded form must contain exactly one newline: the terminator.
	full := Encode(last)
	if n := bytes.Count(full, []byte{ETX}); n != 1 {
		t.Errorf("an escaped newline leaked into the framing: %d newlines in %q", n, full)
	}
}
