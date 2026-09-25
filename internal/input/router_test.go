package input

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/system"
)

// recordingHandler captures routed events for assertions.
type recordingHandler struct {
	mu       sync.Mutex
	channels []ChannelSelect
	volumes  []VolumeChange
	buttons  []ButtonEvent
	reserved []ReservedPotChange
	notify   chan struct{}
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{notify: make(chan struct{}, 64)}
}

func (r *recordingHandler) OnChannelSelect(e ChannelSelect) {
	r.mu.Lock()
	r.channels = append(r.channels, e)
	r.mu.Unlock()
	r.ping()
}
func (r *recordingHandler) OnVolumeChange(e VolumeChange) {
	r.mu.Lock()
	r.volumes = append(r.volumes, e)
	r.mu.Unlock()
	r.ping()
}
func (r *recordingHandler) OnButton(e ButtonEvent) {
	r.mu.Lock()
	r.buttons = append(r.buttons, e)
	r.mu.Unlock()
	r.ping()
}
func (r *recordingHandler) OnReservedPot(e ReservedPotChange) {
	r.mu.Lock()
	r.reserved = append(r.reserved, e)
	r.mu.Unlock()
	r.ping()
}
func (r *recordingHandler) ping() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *recordingHandler) snapshot() ([]ChannelSelect, []VolumeChange, []ButtonEvent, []ReservedPotChange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ChannelSelect(nil), r.channels...),
		append([]VolumeChange(nil), r.volumes...),
		append([]ButtonEvent(nil), r.buttons...),
		append([]ReservedPotChange(nil), r.reserved...)
}

func newTestRouter(t *testing.T) (*Router, *recordingHandler, *system.FakeClock) {
	t.Helper()
	h := newRecordingHandler()
	clk := system.NewFakeClock(t0)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRouter(DefaultConfig(), clk, log, h), h, clk
}

func TestRouterChannelKnobSelectsChannels(t *testing.T) {
	r, h, _ := newTestRouter(t)
	r.SetChannelCount(5) // no pot reading yet, so nothing to select from

	// Knob at 10 % of travel => band 0 of 5.
	r.PotReport(PotChannel, 410)
	// Knob swept to 90 % => band 4.
	r.PotReport(PotChannel, 3686)

	chs, _, _, _ := h.snapshot()
	if len(chs) < 2 {
		t.Fatalf("expected at least two channel selections, got %+v", chs)
	}
	if got := chs[len(chs)-1].Band; got != 4 {
		t.Errorf("final band: %d want 4", got)
	}
}

func TestRouterIgnoresChannelJitter(t *testing.T) {
	r, h, _ := newTestRouter(t)
	r.SetChannelCount(10)

	for _, raw := range []int{405, 412, 408, 415, 402, 411, 409} {
		r.PotReport(PotChannel, raw)
	}
	chs, _, _, _ := h.snapshot()
	if len(chs) != 1 {
		t.Fatalf("jitter produced %d channel events, want 1: %+v", len(chs), chs)
	}
}

func TestRouterVolumeKnobIsPhysical(t *testing.T) {
	r, h, _ := newTestRouter(t)

	r.PotReport(PotAlarmVolume, 2048)
	_, vols, _, _ := h.snapshot()
	if len(vols) != 1 {
		t.Fatalf("got %+v", vols)
	}
	if !vols[0].Physical {
		t.Error("knob-sourced volume must be marked physical")
	}
	if vols[0].Percent < 48 || vols[0].Percent > 52 {
		t.Errorf("midpoint volume: %d", vols[0].Percent)
	}

	// Jitter inside the deadband is swallowed.
	for _, raw := range []int{2050, 2045, 2052} {
		r.PotReport(PotAlarmVolume, raw)
	}
	_, vols, _, _ = h.snapshot()
	if len(vols) != 1 {
		t.Errorf("deadband leaked: %+v", vols)
	}

	// Turning it fully down always reports, so the speaker actually goes quiet.
	r.PotReport(PotAlarmVolume, 0)
	_, vols, _, _ = h.snapshot()
	if got := vols[len(vols)-1].Percent; got != 0 {
		t.Errorf("fully down: %d", got)
	}
}

func TestRouterReservedPotsAreSurfacedNotActedOn(t *testing.T) {
	r, h, _ := newTestRouter(t)
	r.SetChannelCount(4)

	r.PotReport(PotReservedA, 1000)
	r.PotReport(PotReservedB, 3000)

	chs, vols, _, reserved := h.snapshot()
	if len(chs) != 0 || len(vols) != 0 {
		t.Errorf("reserved pots must not drive channel or volume: %+v %+v", chs, vols)
	}
	if len(reserved) != 2 {
		t.Fatalf("reserved events: %+v", reserved)
	}
	if reserved[0].Index != PotReservedA || reserved[1].Index != PotReservedB {
		t.Errorf("indices: %+v", reserved)
	}
}

func TestRouterIgnoresUnknownPotIndex(t *testing.T) {
	r, h, _ := newTestRouter(t)
	r.PotReport(99, 500)
	r.PotReport(-1, 500)
	chs, vols, _, reserved := h.snapshot()
	if len(chs)+len(vols)+len(reserved) != 0 {
		t.Errorf("unknown pot produced events")
	}
}

func TestRouterSetChannelCountReselectsImmediately(t *testing.T) {
	r, h, _ := newTestRouter(t)
	// Knob is at 90 % before any channels are known.
	r.PotReport(PotChannel, 3686)
	chs, _, _, _ := h.snapshot()
	if len(chs) != 0 {
		t.Fatalf("no channels exist yet, got %+v", chs)
	}

	// ErsatzTV comes up with 5 channels: the knob's existing position must
	// select straight away, without the user touching anything. The decision is
	// returned rather than dispatched, so the caller makes exactly one.
	sel := r.SetChannelCount(5)
	if sel == nil || sel.Band != 4 {
		t.Fatalf("got %+v", sel)
	}
	// And nothing was pushed at the handler behind the caller's back.
	chs, _, _, _ = h.snapshot()
	if len(chs) != 0 {
		t.Errorf("SetChannelCount must not call the handler itself: %+v", chs)
	}
}

func TestRouterSetChannelCountReportsNoVerdictUntilTheKnobHasSpoken(t *testing.T) {
	// With channels available but no reading from the knob, the router cannot
	// choose. Saying so lets the caller fall back to the standby screen instead
	// of guessing a band the knob never asked for.
	r, _, _ := newTestRouter(t)
	if sel := r.SetChannelCount(5); sel != nil {
		t.Errorf("got %+v, want nil until the knob reports", sel)
	}

	// With no channels at all the answer is "none" regardless.
	if sel := r.SetChannelCount(0); sel == nil || sel.Band != -1 {
		t.Errorf("got %+v, want band -1", sel)
	}
}

func TestRouterChannelCountZeroDeselects(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.SetChannelCount(3)
	r.PotReport(PotChannel, 2048)

	sel := r.SetChannelCount(0) // ErsatzTV lost all its channels
	if sel == nil || sel.Band != -1 {
		t.Errorf("band with no channels: %+v", sel)
	}
	if got := r.ChannelCount(); got != 0 {
		t.Errorf("ChannelCount: %d", got)
	}
}

func TestRouterButtonShortPressAndHold(t *testing.T) {
	r, h, clk := newTestRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()

	// A short press on the big red button.
	r.ButtonEdge(protocol.ButtonAlarmOff, true)
	clk.Advance(150 * time.Millisecond)
	r.ButtonEdge(protocol.ButtonAlarmOff, false)

	waitFor(t, h.notify)
	_, _, buttons, _ := h.snapshot()
	if len(buttons) != 1 || buttons[0].Action != ActionShortPress {
		t.Fatalf("short press: %+v", buttons)
	}

	// A 5-second hold on the Wi-Fi button fires while it is still down.
	r.ButtonEdge(protocol.ButtonWiFi, true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if clk.Waiters() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll loop never started a timer")
		}
		time.Sleep(time.Millisecond)
	}
	clk.Advance(6 * time.Second)

	waitFor(t, h.notify)
	_, _, buttons, _ = h.snapshot()
	last := buttons[len(buttons)-1]
	if last.Name != protocol.ButtonWiFi || last.Action != ActionHold {
		t.Fatalf("hold: %+v", last)
	}

	cancel()
	<-done
}

func TestRouterRunIsIdleUntilAButtonIsPressed(t *testing.T) {
	r, _, clk := newTestRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	// Give Run a moment to reach its idle select, then confirm it registered no
	// timer: an idle Timeblaster must not be polling.
	time.Sleep(20 * time.Millisecond)
	if got := clk.Waiters(); got != 0 {
		t.Errorf("idle router has %d pending timers, want 0", got)
	}
}

func TestRouterResetUnprimesEverything(t *testing.T) {
	r, h, _ := newTestRouter(t)
	r.SetChannelCount(4)
	r.PotReport(PotChannel, 2048)
	r.PotReport(PotAlarmVolume, 2048)
	r.ButtonEdge(protocol.ButtonWiFi, true)

	r.Reset()

	if _, primed := r.Position(PotChannel); primed {
		t.Error("channel filter still primed after reset")
	}
	if got := r.RawValue(PotChannel); got != 0 {
		t.Errorf("raw value survived reset: %d", got)
	}

	// After a reconnect the first report must re-establish state from scratch.
	before, _, _, _ := h.snapshot()
	r.PotReport(PotChannel, 2048)
	after, _, _, _ := h.snapshot()
	if len(after) <= len(before) {
		t.Error("post-reset report did not re-emit a channel selection")
	}
}

func TestRouterPositionBoundsCheck(t *testing.T) {
	r, _, _ := newTestRouter(t)
	if _, ok := r.Position(-1); ok {
		t.Error("negative index should report not-primed")
	}
	if _, ok := r.Position(PotCount); ok {
		t.Error("out-of-range index should report not-primed")
	}
	if got := r.RawValue(PotCount); got != 0 {
		t.Errorf("RawValue out of range: %d", got)
	}
}

func TestRouterNilHandlerIsSafe(t *testing.T) {
	clk := system.NewFakeClock(t0)
	r := NewRouter(DefaultConfig(), clk, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	r.SetChannelCount(3)
	r.PotReport(PotChannel, 100)
	r.PotReport(PotAlarmVolume, 100)
	r.ButtonEdge(protocol.ButtonWiFi, true)
	r.ButtonEdge(protocol.ButtonWiFi, false)
	// Reaching here without panicking is the assertion.
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
}

func TestAnUnfittedVolumeKnobCannotMoveTheVolume(t *testing.T) {
	// An unconnected analog pin does not read zero. It floats at whatever charge
	// its input capacitance holds, coupled from the neighbouring channels of the
	// multiplexed ADC. On real hardware with nothing attached, this pin wandered
	// between 13% and 22% while the connected channel pot beside it read exactly
	// 1.000 every sample -- and that noise was setting the alarm volume, because
	// the knob is authoritative by design.
	h := newRecordingHandler()
	clk := system.NewFakeClock(t0)
	cfg := DefaultConfig()
	cfg.VolumePotFitted = false
	r := NewRouter(cfg, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), h)

	for _, raw := range []int{520, 640, 760, 530, 900, 610} {
		r.PotReport(PotAlarmVolume, raw)
	}

	_, vols, _, _ := h.snapshot()
	if len(vols) != 0 {
		t.Errorf("an unfitted knob moved the volume %d times: %+v", len(vols), vols)
	}

	// It must still be visible, or a disconnected knob looks like a dead one.
	if pos, primed := r.Position(PotAlarmVolume); !primed || pos == 0 {
		t.Errorf("the reading should still be reported for diagnostics, got %v primed=%v", pos, primed)
	}
}

func TestAFittedVolumeKnobStillWorks(t *testing.T) {
	r, h, _ := newTestRouter(t)
	r.PotReport(PotAlarmVolume, 2048)

	_, vols, _, _ := h.snapshot()
	if len(vols) != 1 {
		t.Fatalf("a fitted knob should report: %+v", vols)
	}
}
