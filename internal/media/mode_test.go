package media

import (
	"context"
	"errors"
	"testing"

	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/screen"
)

// tvOnScreen selects channel 1 and returns the fixture with the player's log
// cleared, ready for the assertions that matter.
func tvOnScreen(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, chans()...)
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SelectBand(ctx, 0); err != nil {
		t.Fatal(err)
	}
	f.player.Reset()
	return f
}

func TestAnInactiveTVNeverTouchesThePlayer(t *testing.T) {
	// Another mode owns the screen. Every path that could load something --
	// the knob, the companion app, the standby image, a restore -- must leave
	// the player alone, or it would replace that mode's picture.
	f := tvOnScreen(t)
	f.svc.Deactivate()
	// Taking down its own tuning card on the way out is the last thing an
	// outgoing TV may do; from here on it must be silent.
	f.player.Reset()
	ctx := context.Background()

	if err := f.svc.SelectBand(ctx, 1); !errors.Is(err, screen.ErrInactive) {
		t.Errorf("SelectBand: got %v, want ErrInactive", err)
	}
	if err := f.svc.SelectNumber(ctx, "2"); !errors.Is(err, screen.ErrInactive) {
		t.Errorf("SelectNumber: got %v, want ErrInactive", err)
	}
	if err := f.svc.ShowNoChannel(ctx); !errors.Is(err, screen.ErrInactive) {
		t.Errorf("ShowNoChannel: got %v, want ErrInactive", err)
	}
	f.svc.RestorePlayback(ctx)
	f.svc.HandlePlayerEvent(mpv.Event{Name: "end-file", Reason: "eof"})

	if calls := f.player.Calls(); len(calls) != 0 {
		t.Errorf("an inactive TV touched the player: %v", calls)
	}
}

func TestDeactivatingForgetsTheChannel(t *testing.T) {
	f := tvOnScreen(t)
	if f.svc.Showing().Number != "1" {
		t.Fatalf("showing %+v before deactivating", f.svc.Showing())
	}
	f.svc.Deactivate()
	if got := f.svc.Showing(); got.Number != "" {
		t.Errorf("an inactive TV still claims %+v is on screen", got)
	}
	// The stall watchdog keys off the selection; a remembered one would have it
	// reading another mode's playback position and "recovering" it.
	if f.svc.Current() != nil {
		t.Error("Current is still set after deactivating")
	}
}

func TestReactivatingReloadsTheSameChannel(t *testing.T) {
	// The other mode replaced the picture. Coming back to the channel the knob
	// was already on must load it again, not decide it is already playing.
	f := tvOnScreen(t)
	f.svc.Deactivate()
	f.svc.Activate()

	if err := f.svc.SelectBand(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if n := len(f.player.CallsNamed("loadfile")); n != 1 {
		t.Errorf("returning to channel 1 issued %d loads, want 1", n)
	}
}

func TestDeactivatingTakesDownAWaitingTuningCard(t *testing.T) {
	// Switched away mid-tune: the card would keep animating over the next
	// mode's picture until its safety-net hold ran out.
	f := tvOnScreen(t)
	if !f.overlay.Awaiting() {
		t.Skip("the default overlay does not hold for the picture")
	}
	f.svc.Deactivate()
	if f.overlay.Awaiting() || f.overlay.Visible() {
		t.Error("the tuning card is still up after switching modes")
	}
}
