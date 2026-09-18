package serialport

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/protocol"
)

func frame(m protocol.Message) []byte { return protocol.Encode(m) }

func TestScannerReadsFrames(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(protocol.Pot(1, 0, 742)))
	buf.Write(frame(protocol.Pot(2, 1, 331)))
	buf.Write(frame(protocol.Button(3, protocol.ButtonAlarmOff, true)))

	sc := NewScanner(&buf)
	var got []protocol.Message
	for sc.Scan() {
		m, err := sc.Message()
		if err != nil {
			t.Fatalf("Message: %v", err)
		}
		got = append(got, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	if got[0].Type != protocol.TypePot || got[2].Type != protocol.TypeButton {
		t.Errorf("types: %+v", got)
	}
}

func TestScannerResynchronisesAfterGarbage(t *testing.T) {
	// The Nano's boot banner, a partially transmitted frame, then a good one.
	var buf bytes.Buffer
	buf.WriteString("ESP-ROM:esp32s3-20210327\r\nrst:0x1 (POWERON)\r\n")
	good := frame(protocol.Pot(1, 0, 742))
	buf.Write(good[:len(good)/2]) // truncated frame, no terminator
	buf.Write(frame(protocol.Hello(2, "1.0.0", "nano-esp32")))
	buf.Write(frame(protocol.Pot(3, 1, 100)))

	sc := NewScanner(&buf)
	var types []protocol.Type
	for sc.Scan() {
		m, err := sc.Message()
		if err != nil {
			continue // a corrupted frame is counted and dropped, never fatal
		}
		types = append(types, m.Type)
	}
	if len(types) != 2 || types[0] != protocol.TypeHello || types[1] != protocol.TypePot {
		t.Fatalf("resynchronisation failed: %v", types)
	}
	if sc.Discarded == 0 {
		t.Error("discarded byte count should have advanced")
	}
}

func TestScannerHandlesSplitReads(t *testing.T) {
	// Feed one frame a byte at a time, which is what a slow USB CDC link looks
	// like in practice.
	full := frame(protocol.Pot(7, 2, 2048))
	r := &dribbleReader{data: full}

	sc := NewScanner(r)
	if !sc.Scan() {
		t.Fatalf("no frame read: %v", sc.Err())
	}
	m, err := sc.Message()
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if m.Seq != 7 || m.Type != protocol.TypePot {
		t.Errorf("got %+v", m)
	}
}

func TestScannerToleratesCRLF(t *testing.T) {
	// A firmware using println() emits \r\n; the trailing CR must not break the
	// checksum check.
	f := frame(protocol.Pot(1, 0, 500))
	withCR := append(f[:len(f)-1], '\r', '\n')

	sc := NewScanner(bytes.NewReader(withCR))
	if !sc.Scan() {
		t.Fatal("no frame")
	}
	if _, err := sc.Message(); err != nil {
		t.Fatalf("CRLF frame rejected: %v", err)
	}
}

func TestScannerRecoversAfterOverlongJunk(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(protocol.STX)
	buf.WriteString(strings.Repeat("x", protocol.MaxFrameLen*3))
	buf.Write(frame(protocol.Pong(1)))

	sc := NewScanner(&buf)
	var got []protocol.Type
	for sc.Scan() {
		if m, err := sc.Message(); err == nil {
			got = append(got, m.Type)
		}
	}
	if len(got) != 1 || got[0] != protocol.TypePong {
		t.Fatalf("got %v", got)
	}
	if sc.Discarded < protocol.MaxFrameLen {
		t.Errorf("discarded count did not reflect the junk: %d", sc.Discarded)
	}
}

func TestScannerCountsOversizedUnterminatedRuns(t *testing.T) {
	// A peer emitting bytes with no terminator must not grow the buffer without
	// bound; the run is dropped and counted.
	var buf bytes.Buffer
	buf.WriteByte(protocol.STX)
	buf.WriteString(strings.Repeat("x", protocol.MaxFrameLen*2))

	sc := NewScanner(&buf)
	for sc.Scan() {
		t.Fatalf("unexpected frame: %q", sc.Bytes())
	}
	if sc.Oversized == 0 {
		t.Error("oversized run was not counted")
	}
	if err := sc.Err(); err != nil {
		t.Errorf("Err: %v", err)
	}
}

func TestScannerTruncatedFrameFollowedByNewFrame(t *testing.T) {
	// STX, some bytes, then another STX with no newline in between: the first is
	// abandoned in favour of the newer one.
	var buf bytes.Buffer
	buf.WriteByte(protocol.STX)
	buf.WriteString("TB1|1|PO")
	buf.Write(frame(protocol.Pong(9)))

	sc := NewScanner(&buf)
	var got []protocol.Message
	for sc.Scan() {
		if m, err := sc.Message(); err == nil {
			got = append(got, m)
		}
	}
	if len(got) != 1 || got[0].Seq != 9 {
		t.Fatalf("got %+v", got)
	}
}

func TestScannerEmptyStream(t *testing.T) {
	sc := NewScanner(bytes.NewReader(nil))
	if sc.Scan() {
		t.Error("empty stream should yield no frames")
	}
	if err := sc.Err(); err != nil {
		t.Errorf("Err: %v", err)
	}
}

func TestPipeRoundTrip(t *testing.T) {
	a, b := NewPipePair()

	go func() {
		_, _ = a.Write(frame(protocol.Pot(1, 0, 742)))
		_, _ = a.Write(frame(protocol.Pot(2, 1, 331)))
	}()

	sc := NewScanner(b)
	for i := range 2 {
		if !sc.Scan() {
			t.Fatalf("frame %d not received: %v", i, sc.Err())
		}
		if _, err := sc.Message(); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}

	// Closing simulates the cable being pulled: the reader sees EOF.
	a.Close()
	if sc.Scan() {
		t.Error("scan should stop after the pipe closes")
	}

	if _, err := a.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("write after close: %v", err)
	}
}

func TestPipeBidirectional(t *testing.T) {
	pi, nano := NewPipePair()

	go func() {
		_, _ = pi.Write(frame(protocol.Time(1, time.Unix(1758220642, 0))))
	}()
	sc := NewScanner(nano)
	if !sc.Scan() {
		t.Fatal("nano did not receive")
	}
	m, err := sc.Message()
	if err != nil || m.Type != protocol.TypeTime {
		t.Fatalf("got %+v, %v", m, err)
	}

	go func() {
		_, _ = nano.Write(frame(protocol.Ack(1, 1)))
	}()
	sc2 := NewScanner(pi)
	if !sc2.Scan() {
		t.Fatal("pi did not receive")
	}
	if m, _ := sc2.Message(); m.Type != protocol.TypeAck {
		t.Fatalf("got %+v", m)
	}
}

func TestResolveDeviceExplicitPath(t *testing.T) {
	// A regular file stands in for the device node here; only existence matters
	// for the explicit-path branch.
	dir := t.TempDir()
	path := filepath.Join(dir, "timeblaster-nano")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveDevice(path, nil)
	if err != nil || got != path {
		t.Fatalf("got %q, %v", got, err)
	}

	if _, err := ResolveDevice(filepath.Join(dir, "absent"), nil); !errors.Is(err, ErrNoDevice) {
		t.Errorf("missing explicit device: %v", err)
	}
}

func TestResolveDeviceReportsNoDevice(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveDevice("", []string{filepath.Join(dir, "nothing-*")})
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("got %v, want ErrNoDevice", err)
	}
	// The error should say where it looked, so the journal is actionable.
	if !strings.Contains(err.Error(), "nothing-") {
		t.Errorf("error should list the patterns tried: %v", err)
	}
}

func TestResolveDeviceRejectsRegularFilesFromGlobs(t *testing.T) {
	// A stray regular file in a glob directory must not be opened as a port.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("not a device"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDevice("", []string{filepath.Join(dir, "*")}); !errors.Is(err, ErrNoDevice) {
		t.Errorf("got %v, want ErrNoDevice", err)
	}
}

func TestResolveDeviceFindsCharacterDevices(t *testing.T) {
	// /dev/null is a character device present on every supported platform, which
	// makes it a safe stand-in for the Nano's device node.
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip("no /dev/null")
	}
	got, err := ResolveDevice("", []string{"/dev/nonexistent-*", "/dev/nul?"})
	if err != nil {
		t.Fatalf("ResolveDevice: %v", err)
	}
	if got != "/dev/null" {
		t.Errorf("got %q", got)
	}
}

func TestResolveDeviceRejectsBadGlob(t *testing.T) {
	if _, err := ResolveDevice("", []string{"[invalid"}); err == nil {
		t.Error("a malformed pattern should be reported as a configuration error")
	} else if errors.Is(err, ErrNoDevice) {
		t.Error("a malformed pattern is not a missing device")
	}
}

func TestPipeOpener(t *testing.T) {
	a, _ := NewPipePair()
	o := NewPipeOpener(a)

	tr, path, err := o.Open(t.Context())
	if err != nil || tr == nil {
		t.Fatalf("Open: %v", err)
	}
	if path == "" {
		t.Error("no path reported")
	}
	if o.Opens() != 1 {
		t.Errorf("opens: %d", o.Opens())
	}

	if _, _, err := o.Open(t.Context()); !errors.Is(err, ErrNoDevice) {
		t.Errorf("exhausted opener: %v", err)
	}

	boom := errors.New("permission denied")
	o.SetError(boom)
	if _, _, err := o.Open(t.Context()); !errors.Is(err, boom) {
		t.Errorf("SetError: %v", err)
	}

	b, _ := NewPipePair()
	o.Enqueue(b)
	if _, _, err := o.Open(t.Context()); err != nil {
		t.Errorf("Enqueue should clear the error: %v", err)
	}
}

func TestDeviceOpenerDescribe(t *testing.T) {
	if got := (DeviceOpener{Config: Config{Device: "/dev/x"}}).Describe(); got != "/dev/x" {
		t.Errorf("got %q", got)
	}
	got := (DeviceOpener{Config: Config{Globs: []string{"/a/*", "/b/*"}}}).Describe()
	if got != "/a/*, /b/*" {
		t.Errorf("got %q", got)
	}
}

// dribbleReader returns one byte per Read, simulating a slow serial link.
type dribbleReader struct {
	data []byte
	pos  int
}

func (d *dribbleReader) Read(p []byte) (int, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	p[0] = d.data[d.pos]
	d.pos++
	return 1, nil
}
