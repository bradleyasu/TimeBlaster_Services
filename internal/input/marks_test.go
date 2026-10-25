package input

import (
	"io"
	"log/slog"
	"testing"

	"github.com/bradsheets/timeblaster/internal/system"
)

// A dial whose housing limits it to the middle of the pot's travel, with marks
// that are not evenly spaced -- the case equal slices get wrong.
var housingMarks = []int{900, 1350, 1900, 2350, 2900}

func markRouter(t *testing.T, marks []int, channels int) (*Router, *recordingHandler) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ChannelMarks = marks
	h := newRecordingHandler()
	r := NewRouter(cfg, system.NewFakeClock(t0), slog.New(slog.NewTextHandler(io.Discard, nil)), h)
	r.SetChannelCount(channels)
	return r, h
}

// dialAt settles the channel knob at raw and returns the band it reports.
func dialAt(r *Router, h *recordingHandler, raw int) int {
	for range 40 {
		r.PotReport(PotChannel, raw)
	}
	chs, _, _, _ := h.snapshot()
	if len(chs) == 0 {
		return -99
	}
	return chs[len(chs)-1].Band
}

func TestEachMarkIsItsChannel(t *testing.T) {
	r, h := markRouter(t, housingMarks, 5)
	for i, raw := range housingMarks {
		if got := dialAt(r, h, raw); got != i {
			t.Errorf("dial on mark %d (raw %d) selected band %d, want %d", i+1, raw, got, i)
		}
	}
}

func TestPastTheEndMarksStaysOnThem(t *testing.T) {
	// The housing leaves some travel before 1 and after 5. Turned all the way,
	// the dial must stay on the end channel, not wander off it.
	r, h := markRouter(t, housingMarks, 5)
	if got := dialAt(r, h, 200); got != 0 {
		t.Errorf("before mark 1: band %d, want 0", got)
	}
	if got := dialAt(r, h, 3900); got != 4 {
		t.Errorf("after mark 5: band %d, want 4", got)
	}
}

func TestADialBetweenMarksDoesNotFlicker(t *testing.T) {
	r, h := markRouter(t, housingMarks, 5)
	dialAt(r, h, 1350) // on mark 2
	before := len(h.channels)
	// Just past the midpoint towards mark 3 (1625), but not by the margin.
	dialAt(r, h, 1680)
	dialAt(r, h, 1600)
	dialAt(r, h, 1680)
	if n := len(h.channels); n != before {
		t.Errorf("a dial hovering between marks changed channel %d times", n-before)
	}
	// Well past the midpoint, it moves.
	if got := dialAt(r, h, 1800); got != 2 {
		t.Errorf("most of the way to mark 3: band %d, want 2", got)
	}
}

func TestAMarkWithNoChannelIsStandby(t *testing.T) {
	// Two ambience channels on a five-mark dial: marks 3 to 5 have nothing
	// behind them.
	r, h := markRouter(t, housingMarks, 2)
	if got := dialAt(r, h, housingMarks[1]); got != 1 {
		t.Errorf("mark 2: band %d, want 1", got)
	}
	if got := dialAt(r, h, housingMarks[3]); got != -1 {
		t.Errorf("mark 4 with two channels: band %d, want -1 (standby)", got)
	}
	if got := r.ChannelCount(); got != 2 {
		t.Errorf("ChannelCount %d, want 2", got)
	}
}

func TestMarksMayRunBackwards(t *testing.T) {
	// A pot wired the other way round reads high at mark 1.
	rev := []int{2900, 2350, 1900, 1350, 900}
	r, h := markRouter(t, rev, 5)
	if got := dialAt(r, h, 2900); got != 0 {
		t.Errorf("mark 1: band %d, want 0", got)
	}
	if got := dialAt(r, h, 900); got != 4 {
		t.Errorf("mark 5: band %d, want 4", got)
	}
}

func TestMarksNearTheEndsAreNotSnappedTogether(t *testing.T) {
	// The default calibration snaps the outer 2 % to the ends. Applied to
	// marks, two near one end would collapse into the same position.
	r, h := markRouter(t, []int{10, 70, 2000}, 3)
	if got := dialAt(r, h, 10); got != 0 {
		t.Errorf("mark 1: band %d, want 0", got)
	}
	if got := dialAt(r, h, 70); got != 1 {
		t.Errorf("mark 2: band %d, want 1", got)
	}
}

func TestWithoutMarksTheTravelIsStillDividedEqually(t *testing.T) {
	r, h := markRouter(t, nil, 4)
	if got := dialAt(r, h, 3800); got != 3 {
		t.Errorf("fully clockwise with 4 channels: band %d, want 3", got)
	}
}
