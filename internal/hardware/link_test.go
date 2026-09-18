package hardware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/protocol"
	"github.com/bradsheets/timeblaster/internal/serialport"
	"github.com/bradsheets/timeblaster/internal/system"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recordingObserver captures reports delivered to the input layer.
type recordingObserver struct {
	mu      sync.Mutex
	pots    [][2]int
	buttons []buttonRecord
	notify  chan struct{}
}

type buttonRecord struct {
	Name string
	Down bool
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{notify: make(chan struct{}, 128)}
}

func (r *recordingObserver) PotReport(index, raw int) {
	r.mu.Lock()
	r.pots = append(r.pots, [2]int{index, raw})
	r.mu.Unlock()
	r.ping()
}

func (r *recordingObserver) ButtonEdge(name string, down bool) {
	r.mu.Lock()
	r.buttons = append(r.buttons, buttonRecord{name, down})
	r.mu.Unlock()
	r.ping()
}

func (r *recordingObserver) ping() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *recordingObserver) snapshot() ([][2]int, []buttonRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]int(nil), r.pots...), append([]buttonRecord(nil), r.buttons...)
}

// recordingLifecycle captures link up/down transitions.
type recordingLifecycle struct {
	mu     sync.Mutex
	ups    []string
	downs  []error
	notify chan struct{}
}

func newRecordingLifecycle() *recordingLifecycle {
	return &recordingLifecycle{notify: make(chan struct{}, 32)}
}

func (r *recordingLifecycle) LinkUp(device string) {
	r.mu.Lock()
	r.ups = append(r.ups, device)
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *recordingLifecycle) LinkDown(err error) {
	r.mu.Lock()
	r.downs = append(r.downs, err)
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *recordingLifecycle) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ups), len(r.downs)
}

// nanoSide is the test's stand-in for the firmware: it reads what the Pi sends
// and can send reports back.
type nanoSide struct {
	t    *testing.T
	pipe *serialport.Pipe
	sc   *serialport.Scanner
	seq  protocol.Sequencer
}

func newNanoSide(t *testing.T, p *serialport.Pipe) *nanoSide {
	return &nanoSide{t: t, pipe: p, sc: serialport.NewScanner(p)}
}

func (n *nanoSide) send(m protocol.Message) {
	if _, err := n.pipe.Write(protocol.Encode(m)); err != nil {
		n.t.Errorf("nano write: %v", err)
	}
}

// expect reads until it sees a message of the given type, or fails.
func (n *nanoSide) expect(typ protocol.Type, timeout time.Duration) protocol.Message {
	n.t.Helper()
	done := make(chan protocol.Message, 1)
	go func() {
		for n.sc.Scan() {
			m, err := n.sc.Message()
			if err != nil {
				continue
			}
			if m.Type == typ {
				done <- m
				return
			}
		}
	}()
	select {
	case m := <-done:
		return m
	case <-time.After(timeout):
		n.t.Fatalf("the Pi never sent a %s message", typ)
		return protocol.Message{}
	}
}

type linkFixture struct {
	link      *Link
	opener    *serialport.PipeOpener
	observer  *recordingObserver
	lifecycle *recordingLifecycle
	clock     *system.FakeClock
	cancel    context.CancelFunc
	done      chan struct{}
}

func newLinkFixture(t *testing.T, cfg Config, nanoEnds int) (*linkFixture, []*serialport.Pipe) {
	t.Helper()

	var piEnds []serialport.Transport
	var nanoPipes []*serialport.Pipe
	for range nanoEnds {
		pi, nano := serialport.NewPipePair()
		piEnds = append(piEnds, pi)
		nanoPipes = append(nanoPipes, nano)
	}

	opener := serialport.NewPipeOpener(piEnds...)
	obs := newRecordingObserver()
	life := newRecordingLifecycle()
	clk := system.NewFakeClock(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC))

	link := NewLink(cfg, Deps{
		Opener: opener, Clock: clk, Logger: testLogger(), Observer: obs, Lifecycle: life,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = link.Run(ctx) }()

	f := &linkFixture{link: link, opener: opener, observer: obs, lifecycle: life, clock: clk, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("link Run did not stop")
		}
	})
	return f, nanoPipes
}

func waitUntil(t *testing.T, what string, cond func() bool) {
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

func TestLinkConnectsAndResyncs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Brightness = 42
	f, nanos := newLinkFixture(t, cfg, 1)
	nano := newNanoSide(t, nanos[0])

	waitUntil(t, "connection", f.link.Connected)

	// On connection the Pi must push the full authoritative state.
	m := nano.expect(protocol.TypeTime, 3*time.Second)
	unix, err := m.Int64Arg(0)
	if err != nil || unix != f.clock.Now().Unix() {
		t.Errorf("time sync payload: %d, %v", unix, err)
	}

	ups, _ := f.lifecycle.counts()
	if ups != 1 {
		t.Errorf("LinkUp calls: %d", ups)
	}
	st := f.link.Status()
	if !st.Connected || st.Device == "" {
		t.Errorf("status: %+v", st)
	}
}

func TestLinkDeliversInputReports(t *testing.T) {
	f, nanos := newLinkFixture(t, DefaultConfig(), 1)
	nano := newNanoSide(t, nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	nano.send(protocol.Pot(nano.seq.Next(), 0, 742))
	nano.send(protocol.Pot(nano.seq.Next(), 1, 331))
	nano.send(protocol.Button(nano.seq.Next(), protocol.ButtonAlarmOff, true))
	nano.send(protocol.Button(nano.seq.Next(), protocol.ButtonAlarmOff, false))

	waitUntil(t, "input reports", func() bool {
		pots, buttons := f.observer.snapshot()
		return len(pots) == 2 && len(buttons) == 2
	})

	pots, buttons := f.observer.snapshot()
	if pots[0] != [2]int{0, 742} || pots[1] != [2]int{1, 331} {
		t.Errorf("pots: %v", pots)
	}
	if !buttons[0].Down || buttons[1].Down || buttons[0].Name != protocol.ButtonAlarmOff {
		t.Errorf("buttons: %+v", buttons)
	}
	if got := f.link.Status().MessagesRx; got < 4 {
		t.Errorf("messages received: %d", got)
	}
}

func TestLinkAnswersPings(t *testing.T) {
	f, nanos := newLinkFixture(t, DefaultConfig(), 1)
	nano := newNanoSide(t, nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	nano.send(protocol.Ping(nano.seq.Next(), 5*time.Second))
	if m := nano.expect(protocol.TypePong, 3*time.Second); m.Type != protocol.TypePong {
		t.Fatalf("got %+v", m)
	}
}

func TestLinkResyncsOnHello(t *testing.T) {
	// A HELLO means the Nano rebooted (a reset, or the Pi rebooting and cycling
	// USB power) and has forgotten everything it was told.
	f, nanos := newLinkFixture(t, DefaultConfig(), 1)
	nano := newNanoSide(t, nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	if err := f.link.SetAlarmActive(true); err != nil {
		t.Fatal(err)
	}
	nano.send(protocol.Hello(nano.seq.Next(), "1.2.3", "nano-esp32"))

	// The resync must re-assert that the alarm is still ringing.
	deadline := time.Now().Add(3 * time.Second)
	var found bool
	for time.Now().Before(deadline) && !found {
		m := nano.expect(protocol.TypeAlarm, 3*time.Second)
		if m.Arg(1) == "1" {
			found = true
		}
	}
	if !found {
		t.Fatal("alarm state was not re-asserted after HELLO")
	}
	waitUntil(t, "firmware version", func() bool { return f.link.Status().Firmware == "1.2.3" })
}

func TestLinkPeriodicTimeSync(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TimeSyncInterval = 60 * time.Second
	// Keep the heartbeat well out of the way: this test jumps the fake clock by a
	// minute, which would otherwise look like a silent Nano.
	cfg.HeartbeatTimeout = time.Hour
	f, nanos := newLinkFixture(t, cfg, 1)
	nano := newNanoSide(t, nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	nano.expect(protocol.TypeTime, 3*time.Second) // the initial resync

	before := f.link.Status().MessagesTx
	// Wait for the supervisor to park on its timers, then let a minute pass.
	waitUntil(t, "sync timer", func() bool { return f.clock.Waiters() >= 2 })
	f.clock.Advance(61 * time.Second)

	nano.expect(protocol.TypeTime, 3*time.Second)
	if f.link.Status().MessagesTx <= before {
		t.Error("no message was sent on the sync tick")
	}
}

func TestLinkReconnectsAfterDisconnect(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReconnectMin = time.Millisecond
	cfg.ReconnectMax = 2 * time.Millisecond
	f, nanos := newLinkFixture(t, cfg, 2)
	waitUntil(t, "first connection", f.link.Connected)

	// Pull the cable.
	nanos[0].Close()
	waitUntil(t, "disconnect", func() bool { return !f.link.Connected() })

	// The reconnect loop sleeps on the fake clock, so advance it.
	waitUntil(t, "backoff timer", func() bool { return f.clock.Waiters() > 0 })
	f.clock.Advance(time.Second)

	waitUntil(t, "reconnection", f.link.Connected)

	nano := newNanoSide(t, nanos[1])
	nano.expect(protocol.TypeTime, 3*time.Second) // resync on the new session

	ups, downs := f.lifecycle.counts()
	if ups != 2 || downs != 1 {
		t.Errorf("lifecycle: %d up, %d down", ups, downs)
	}
	if got := f.link.Status().Reconnects; got != 1 {
		t.Errorf("reconnect count: %d", got)
	}
}

func TestLinkKeepsRetryingWhenNoNanoIsPresent(t *testing.T) {
	// A Timeblaster with no Nano attached must keep running, not crash or give up.
	cfg := DefaultConfig()
	cfg.ReconnectMin = time.Millisecond
	cfg.ReconnectMax = 2 * time.Millisecond
	f, _ := newLinkFixture(t, cfg, 0)

	for range 5 {
		waitUntil(t, "retry timer", func() bool { return f.clock.Waiters() > 0 })
		f.clock.Advance(time.Second)
	}
	if f.link.Connected() {
		t.Error("link should not report connected")
	}
	if got := f.opener.Opens(); got != 0 {
		t.Errorf("opens: %d", got)
	}
	// Commands must fail cleanly rather than blocking or panicking.
	if err := f.link.SetAlarmActive(true); !errors.Is(err, ErrNotConnected) {
		t.Errorf("SetAlarmActive while disconnected: %v", err)
	}
}

func TestLinkDetectsASilentNano(t *testing.T) {
	// A Nano that stops sending PINGs — a hung firmware, or a half-open USB
	// connection — must be noticed and the port reopened.
	cfg := DefaultConfig()
	cfg.HeartbeatTimeout = 4 * time.Second
	cfg.TimeSyncInterval = time.Hour // keep the sync timer out of the way
	cfg.ReconnectMin = time.Millisecond
	cfg.ReconnectMax = time.Millisecond
	f, nanos := newLinkFixture(t, cfg, 2)
	waitUntil(t, "connection", f.link.Connected)

	nano := newNanoSide(t, nanos[0])
	nano.expect(protocol.TypeTime, 3*time.Second)

	// Say nothing at all and let the heartbeat window elapse.
	waitUntil(t, "heartbeat timer", func() bool { return f.clock.Waiters() >= 2 })
	f.clock.Advance(10 * time.Second)

	waitUntil(t, "heartbeat failure", func() bool { return f.link.Status().Reconnects >= 1 })
	if st := f.link.Status(); st.LastError == "" {
		t.Error("the heartbeat failure should be recorded")
	}
}

func TestLinkCountsMalformedFrames(t *testing.T) {
	f, nanos := newLinkFixture(t, DefaultConfig(), 1)
	waitUntil(t, "connection", f.link.Connected)

	// A frame with a valid structure but a corrupt checksum.
	bad := protocol.Encode(protocol.Pot(1, 0, 742))
	bad[len(bad)-2] ^= 0xFF
	if _, err := nanos[0].Write(bad); err != nil {
		t.Fatal(err)
	}
	// Followed by a good one, to prove the link survived.
	if _, err := nanos[0].Write(protocol.Encode(protocol.Pot(2, 1, 100))); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, "malformed count", func() bool { return f.link.Status().MalformedFrames >= 1 })
	waitUntil(t, "good report", func() bool {
		pots, _ := f.observer.snapshot()
		return len(pots) == 1 && pots[0] == [2]int{1, 100}
	})
	if !f.link.Connected() {
		t.Error("a malformed frame must not drop the link")
	}
}

func TestLinkIgnoresMalformedReports(t *testing.T) {
	f, nanos := newLinkFixture(t, DefaultConfig(), 1)
	nano := newNanoSide(t, nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	// Structurally valid frames whose arguments make no sense.
	nano.send(protocol.Message{Seq: 1, Type: protocol.TypePot, Args: []string{"x", "y"}})
	nano.send(protocol.Message{Seq: 2, Type: protocol.TypeButton, Args: []string{"WIFI", "SIDEWAYS"}})
	nano.send(protocol.Message{Seq: 3, Type: protocol.Type("FUTURE"), Args: []string{"1"}})
	// A valid one afterwards proves the dispatcher kept going.
	nano.send(protocol.Pot(4, 0, 500))

	waitUntil(t, "valid report", func() bool {
		pots, _ := f.observer.snapshot()
		return len(pots) == 1
	})
	_, buttons := f.observer.snapshot()
	if len(buttons) != 0 {
		t.Errorf("a malformed button report reached the observer: %+v", buttons)
	}
}

func TestLinkCommandsReachTheNano(t *testing.T) {
	f, nanos := newLinkFixture(t, DefaultConfig(), 1)
	nano := newNanoSide(t, nanos[0])
	waitUntil(t, "connection", f.link.Connected)

	if err := f.link.ShowText("SETUP"); err != nil {
		t.Fatalf("ShowText: %v", err)
	}
	m := nano.expect(protocol.TypeDisplay, 3*time.Second)
	for m.Arg(0) != protocol.DisplaySubText {
		m = nano.expect(protocol.TypeDisplay, 3*time.Second)
	}
	if m.Arg(1) != "SETUP" {
		t.Errorf("display text: %+v", m.Args)
	}

	if err := f.link.SetLED(protocol.LEDWiFi, true); err != nil {
		t.Fatalf("SetLED: %v", err)
	}
	led := nano.expect(protocol.TypeLED, 3*time.Second)
	if led.Arg(0) != protocol.LEDWiFi || led.Arg(1) != "1" {
		t.Errorf("led: %+v", led.Args)
	}
}

func TestLinkDropsWritesWhenTheQueueIsFull(t *testing.T) {
	// A saturated queue must not block callers; non-critical commands are dropped
	// and critical ones displace older entries.
	cfg := DefaultConfig()
	cfg.WriteQueueSize = 2
	link := NewLink(cfg, Deps{
		Opener: serialport.NewPipeOpener(), Clock: system.NewFakeClock(time.Now()), Logger: testLogger(),
	})
	link.connected.Store(true) // pretend we are connected but nothing drains

	// Fill the queue.
	for range cfg.WriteQueueSize {
		if err := link.SetBrightness(50); err != nil {
			t.Fatalf("filling the queue: %v", err)
		}
	}
	// A non-critical command is dropped.
	if err := link.SetLED(protocol.LEDPower, true); !errors.Is(err, ErrQueueFull) {
		t.Errorf("non-critical command: %v", err)
	}
	// A critical one makes room for itself.
	if err := link.SetAlarmActive(true); err != nil {
		t.Errorf("critical command: %v", err)
	}
	if got := link.Status().DroppedWrites; got < 2 {
		t.Errorf("dropped writes: %d", got)
	}
}

func TestLinkRunStopsOnCancel(t *testing.T) {
	f, _ := newLinkFixture(t, DefaultConfig(), 1)
	waitUntil(t, "connection", f.link.Connected)
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

func TestFakeNano(t *testing.T) {
	f := NewFakeNano()
	if !f.Connected() {
		t.Error("fake should start connected")
	}
	if err := f.SetAlarmActive(true); err != nil {
		t.Fatal(err)
	}
	if err := f.ShowText("SETUP"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetLED(protocol.LEDAlarm, true); err != nil {
		t.Fatal(err)
	}
	if err := f.SetBrightness(30); err != nil {
		t.Fatal(err)
	}
	if err := f.ShowClock(); err != nil {
		t.Fatal(err)
	}
	if err := f.SetTime(time.Now()); err != nil {
		t.Fatal(err)
	}

	if got, ok := f.LastOfKind("display-text"); !ok || got.Text != "SETUP" {
		t.Errorf("display-text: %+v", got)
	}
	if len(f.Commands()) != 6 {
		t.Errorf("commands: %d", len(f.Commands()))
	}

	f.SetConnected(false)
	if f.Connected() {
		t.Error("SetConnected(false) had no effect")
	}
	f.Reset()
	if len(f.Commands()) != 0 {
		t.Error("Reset did not clear commands")
	}
	f.Err = errors.New("boom")
	if err := f.ShowClock(); err == nil {
		t.Error("configured error not returned")
	}
}
