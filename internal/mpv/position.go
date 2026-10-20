package mpv

import "context"

// PositionState distinguishes the three answers that matter to a stall
// watchdog, because "no position" and "position deliberately not moving" must
// not be confused: treating a pause as a stall would reload a stream that is
// behaving.
type PositionState int

const (
	// PositionKnown means the position was read and can be compared.
	PositionKnown PositionState = iota
	// PositionUnknown means it could not be read, which is not evidence of
	// health: during the observed freezes several properties went unavailable
	// while the picture sat still.
	PositionUnknown
	// PositionHalted means something legitimately explains a still position.
	PositionHalted
)

// ProbePosition reads the playback position, first checking for the states in
// which it is meant to stand still: paused, buffering, or finished.
func ProbePosition(ctx context.Context, player Controller) (float64, PositionState) {
	for _, name := range []string{"pause", "paused-for-cache", "eof-reached"} {
		var flag bool
		if err := player.GetProperty(ctx, name, &flag); err != nil {
			continue // unreadable: fall through to the position check
		}
		if flag {
			return 0, PositionHalted
		}
	}
	var pos float64
	if err := player.GetProperty(ctx, "time-pos", &pos); err != nil {
		return 0, PositionUnknown
	}
	return pos, PositionKnown
}
