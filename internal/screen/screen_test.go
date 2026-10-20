package screen

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/mpv"
)

// journal records the order things happen in across every fake mode, which is
// what the switching tests are about.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(s string) {
	j.mu.Lock()
	j.entries = append(j.entries, s)
	j.mu.Unlock()
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.entries...)
}

type fakeMode struct {
	id   string
	j    *journal
	gate Gate
	// block, when set, holds SelectBand until closed.
	block chan struct{}
}

func (m *fakeMode) ID() string        { return m.id }
func (m *fakeMode) Title() string     { return strings.ToUpper(m.id) }
func (m *fakeMode) ChannelCount() int { return 3 }
func (m *fakeMode) Activate()         { m.gate.Open(); m.j.add(m.id + " activate") }
func (m *fakeMode) Deactivate()       { m.gate.Close(); m.j.add(m.id + " deactivate") }
func (m *fakeMode) Showing() Showing  { return Showing{} }
func (m *fakeMode) SelectBand(_ context.Context, band int) error {
	if m.block != nil {
		<-m.block
	}
	return m.gate.Do(func() error {
		m.j.add(m.id + " select " + string(rune('0'+band)))
		return nil
	})
}
func (m *fakeMode) RestorePlayback(context.Context) { m.j.add(m.id + " restore") }
func (m *fakeMode) HandlePlayerEvent(ev mpv.Event)  { m.j.add(m.id + " event " + ev.Name) }

type recordingObserver struct{ j *journal }

func (o recordingObserver) ModeChanged(from, to Mode) {
	o.j.add("changed " + from.ID() + ">" + to.ID())
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newDirector(t *testing.T) (*Director, *journal, *fakeMode, *fakeMode) {
	t.Helper()
	j := &journal{}
	tv, amb := &fakeMode{id: "tv", j: j}, &fakeMode{id: "amb", j: j}
	d := NewDirector([]Mode{tv, amb}, discard(), recordingObserver{j})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return d, j, tv, amb
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func contains(entries []string, s string) bool {
	for _, e := range entries {
		if e == s {
			return true
		}
	}
	return false
}

func indexOf(entries []string, s string) int {
	for i, e := range entries {
		if e == s {
			return i
		}
	}
	return -1
}

func TestOnlyTheFirstModeStartsActive(t *testing.T) {
	d, j, tv, amb := newDirector(t)
	if d.Active() != Mode(tv) {
		t.Errorf("active = %s, want tv", d.Active().ID())
	}
	if !tv.gate.IsOpen() || amb.gate.IsOpen() {
		t.Errorf("gates: tv open=%v, amb open=%v", tv.gate.IsOpen(), amb.gate.IsOpen())
	}
	// Explicitly, not by default: a mode that begins life thinking it owns the
	// screen would load over whatever the active one is showing.
	if !contains(j.all(), "amb deactivate") {
		t.Error("the second mode was never deactivated")
	}
}

func TestSwitchingDeactivatesTheOldModeBeforeTheNewOneStarts(t *testing.T) {
	d, j, _, _ := newDirector(t)
	d.Request(1, 2)
	waitFor(t, "the selection", func() bool { return contains(j.all(), "amb select 2") })

	e := j.all()
	deact, act := indexOf(e, "tv deactivate"), indexOf(e, "amb activate")
	changed, sel := indexOf(e, "changed tv>amb"), indexOf(e, "amb select 2")
	if !(deact >= 0 && deact < act && act < changed && changed < sel) {
		t.Errorf("wrong order: %v", e)
	}
	if d.Active().ID() != "amb" {
		t.Errorf("active = %s", d.Active().ID())
	}
}

func TestAChannelChangeWithinAModeIsNotAModeChange(t *testing.T) {
	d, j, _, _ := newDirector(t)
	d.Request(0, 1)
	waitFor(t, "the selection", func() bool { return contains(j.all(), "tv select 1") })
	for _, e := range j.all() {
		if strings.HasPrefix(e, "changed") || e == "tv deactivate" {
			t.Errorf("a channel change switched modes: %v", j.all())
		}
	}
}

func TestTheNewestRequestWins(t *testing.T) {
	d, j, tv, _ := newDirector(t)
	// Hold the director inside one request while three more arrive.
	tv.block = make(chan struct{})
	d.Request(0, 0)
	time.Sleep(20 * time.Millisecond)
	d.Request(0, 1)
	d.Request(0, 2)
	d.Request(1, 1)
	close(tv.block)
	waitFor(t, "the last request", func() bool { return contains(j.all(), "amb select 1") })
	time.Sleep(20 * time.Millisecond)
	for _, stale := range []string{"tv select 1", "tv select 2"} {
		if contains(j.all(), stale) {
			t.Errorf("a superseded request was applied: %s", stale)
		}
	}
}

func TestPlayerEventsGoOnlyToTheActiveMode(t *testing.T) {
	d, j, _, _ := newDirector(t)
	d.HandlePlayerEvent(mpv.Event{Name: "end-file"})
	d.Request(1, 0)
	waitFor(t, "the switch", func() bool { return d.Active().ID() == "amb" })
	d.HandlePlayerEvent(mpv.Event{Name: "playback-restart"})

	e := j.all()
	if !contains(e, "tv event end-file") || contains(e, "amb event end-file") {
		t.Errorf("the first event went to the wrong mode: %v", e)
	}
	if !contains(e, "amb event playback-restart") || contains(e, "tv event playback-restart") {
		t.Errorf("the second event went to the wrong mode: %v", e)
	}
}

func TestRestoreGoesToTheActiveMode(t *testing.T) {
	d, j, _, _ := newDirector(t)
	d.Request(1, 0)
	waitFor(t, "the switch", func() bool { return d.Active().ID() == "amb" })
	d.RestorePlayback()
	waitFor(t, "the restore", func() bool { return contains(j.all(), "amb restore") })
	if contains(j.all(), "tv restore") {
		t.Error("an inactive mode was asked to restore its picture")
	}
}

func TestAnUnknownModeIsIgnored(t *testing.T) {
	d, j, _, _ := newDirector(t)
	d.Request(7, 0)
	d.Request(0, 1)
	waitFor(t, "the valid request", func() bool { return contains(j.all(), "tv select 1") })
	if d.Active().ID() != "tv" {
		t.Errorf("active = %s", d.Active().ID())
	}
}

func TestIndexFindsModesByID(t *testing.T) {
	d, _, _, _ := newDirector(t)
	if d.Index("amb") != 1 || d.Index("tv") != 0 || d.Index("guide") != -1 {
		t.Errorf("Index: amb=%d tv=%d guide=%d", d.Index("amb"), d.Index("tv"), d.Index("guide"))
	}
}

func TestGateRefusesWorkWhileClosed(t *testing.T) {
	var g Gate
	ran := false
	if err := g.Do(func() error { ran = true; return nil }); err != ErrInactive || ran {
		t.Errorf("a closed gate ran the work: err=%v ran=%v", err, ran)
	}
	g.Open()
	if err := g.Do(func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("an open gate refused: err=%v ran=%v", err, ran)
	}
}

func TestGateCloseWaitsForWorkInFlight(t *testing.T) {
	// A reload that passed the check just before a mode switch must finish
	// before the switch proceeds, or it lands on top of the next mode.
	var g Gate
	g.Open()
	started, release := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		_ = g.Do(func() error { close(started); <-release; return nil })
		close(finished)
	}()
	<-started

	closed := make(chan struct{})
	go func() { g.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while work was still in flight")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	<-finished
	<-closed
	if g.IsOpen() {
		t.Error("the gate is open after Close")
	}
}
