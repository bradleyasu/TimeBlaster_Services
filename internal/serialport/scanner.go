package serialport

import (
	"bufio"
	"bytes"
	"io"

	"github.com/bradsheets/timeblaster/internal/protocol"
)

// Scanner extracts TB1 frames from a byte stream.
//
// The resynchronisation rule is the important part: everything before an STX is
// discarded. When the Nano resets mid-frame, or the port is reopened part-way
// through a transmission, the reader silently realigns on the next frame instead
// of emitting a corrupted message or, worse, staying permanently out of step.
type Scanner struct {
	sc *bufio.Scanner
	// Discarded counts bytes thrown away during resynchronisation. A steadily
	// climbing count is a useful health signal: it means line noise or a peer
	// that is not speaking TB1.
	Discarded int
	// Oversized counts unterminated runs longer than a legal frame that were
	// dropped wholesale.
	Oversized int
}

// NewScanner wraps a reader.
func NewScanner(r io.Reader) *Scanner {
	sc := bufio.NewScanner(r)
	// The buffer only needs to hold one frame plus whatever junk precedes it.
	sc.Buffer(make([]byte, 0, 4*protocol.MaxFrameLen), 16*protocol.MaxFrameLen)
	s := &Scanner{sc: sc}
	sc.Split(s.split)
	return s
}

// split is a bufio.SplitFunc returning the bytes between an STX and the next
// newline.
//
// It consumes junk in a loop rather than returning after each discard. That
// matters at end of stream: bufio.Scanner stops as soon as the split function
// returns no token once the underlying reader has reported EOF, so a split that
// discarded garbage one step at a time would throw away perfectly good frames
// sitting behind it in the buffer.
func (s *Scanner) split(data []byte, atEOF bool) (advance int, token []byte, err error) {
	offset := 0
	for {
		rest := data[offset:]
		if len(rest) == 0 {
			return offset, nil, nil
		}

		// Discard anything before the frame start.
		start := bytes.IndexByte(rest, protocol.STX)
		if start == -1 {
			s.Discarded += len(rest)
			return offset + len(rest), nil, nil
		}
		if start > 0 {
			s.Discarded += start
			offset += start
			continue
		}

		// rest[0] is STX. Look for the terminating newline in the body.
		body := rest[1:]
		end := bytes.IndexByte(body, protocol.ETX)

		// A second STX before the terminator means the first frame was truncated
		// (a reset mid-transmission). Abandon it and restart from the newer one.
		if next := bytes.IndexByte(body, protocol.STX); next != -1 && (end == -1 || next < end) {
			s.Discarded += 1 + next
			offset += 1 + next
			continue
		}

		if end == -1 {
			if len(rest) > protocol.MaxFrameLen {
				// An unterminated run longer than any legal frame: drop it so a
				// peer spraying bytes without newlines cannot grow the buffer
				// without bound.
				s.Oversized++
				s.Discarded += len(rest)
				return offset + len(rest), nil, nil
			}
			if atEOF {
				s.Discarded += len(rest)
				return offset + len(rest), nil, nil
			}
			return offset, nil, nil // need more data
		}

		// A trailing CR is tolerated so a firmware using println() interoperates.
		frame := body[:end]
		if n := len(frame); n > 0 && frame[n-1] == '\r' {
			frame = frame[:n-1]
		}
		if len(frame) == 0 {
			// An empty frame carries nothing; skip it without stalling the scan.
			offset += 1 + end + 1
			continue
		}
		return offset + 1 + end + 1, frame, nil
	}
}

// Scan advances to the next frame, reporting false at end of stream.
func (s *Scanner) Scan() bool { return s.sc.Scan() }

// Bytes returns the current frame body, without STX or the terminator. The slice
// is only valid until the next call to Scan.
func (s *Scanner) Bytes() []byte { return s.sc.Bytes() }

// Message decodes the current frame.
func (s *Scanner) Message() (protocol.Message, error) { return protocol.Decode(s.sc.Bytes()) }

// Err returns the underlying read error, if any.
func (s *Scanner) Err() error { return s.sc.Err() }
