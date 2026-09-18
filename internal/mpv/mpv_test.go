package mpv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeMPVServer is a unix-socket server that speaks just enough of mpv's JSON IPC
// protocol to exercise the client: it echoes replies for commands and can push
// events.
type fakeMPVServer struct {
	t        *testing.T
	ln       net.Listener
	socket   string
	mu       sync.Mutex
	received []map[string]any
	conns    []net.Conn
	// reply builds the response for a request; nil means a plain success.
	reply func(req map[string]any) map[string]any
}

// tempSocket returns a short unix-socket path. t.TempDir() is not usable here:
// its paths routinely exceed the ~104-byte sun_path limit on macOS and bind
// fails with EINVAL.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tbmpv")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "m.sock")
}

func newFakeMPVServer(t *testing.T) *fakeMPVServer {
	t.Helper()
	socket := tempSocket(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeMPVServer{t: t, ln: ln, socket: socket}
	t.Cleanup(func() { ln.Close() })
	go s.accept()
	return s
}

func (s *fakeMPVServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *fakeMPVServer) serve(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req map[string]any
		if err := dec.Decode(&req); err != nil {
			return
		}
		s.mu.Lock()
		s.received = append(s.received, req)
		reply := s.reply
		s.mu.Unlock()

		resp := map[string]any{"error": "success", "request_id": req["request_id"]}
		if reply != nil {
			if custom := reply(req); custom != nil {
				// A reply carrying __drop stands in for an mpv that accepted the
				// command and then never answered.
				if _, drop := custom["__drop"]; drop {
					continue
				}
				custom["request_id"] = req["request_id"]
				resp = custom
			}
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *fakeMPVServer) pushEvent(ev map[string]any) {
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		_ = json.NewEncoder(c).Encode(ev)
	}
}

// waitForConn blocks until the server has accepted at least one connection, so
// that a pushed event cannot race the accept goroutine.
func (s *fakeMPVServer) waitForConn(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("server never accepted a connection")
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *fakeMPVServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.received...)
}

func (s *fakeMPVServer) closeConns() {
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func TestClientCommandRoundTrip(t *testing.T) {
	srv := newFakeMPVServer(t)
	c, err := Dial(context.Background(), srv.socket, 2*time.Second, testLogger())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.LoadFile(context.Background(), "http://127.0.0.1:8409/iptv/channel/3.m3u8"); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests: %+v", reqs)
	}
	cmd, _ := reqs[0]["command"].([]any)
	if len(cmd) != 3 || cmd[0] != "loadfile" || cmd[2] != "replace" {
		t.Errorf("loadfile command: %+v", cmd)
	}
	if !c.Alive() {
		t.Error("client should be alive")
	}
}

func TestClientPropertyRoundTrip(t *testing.T) {
	srv := newFakeMPVServer(t)
	srv.reply = func(req map[string]any) map[string]any {
		cmd, _ := req["command"].([]any)
		if len(cmd) >= 2 && cmd[0] == "get_property" && cmd[1] == "volume" {
			return map[string]any{"error": "success", "data": 42.0}
		}
		return nil
	}
	c, err := Dial(context.Background(), srv.socket, 2*time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var vol float64
	if err := c.GetProperty(context.Background(), "volume", &vol); err != nil {
		t.Fatalf("GetProperty: %v", err)
	}
	if vol != 42 {
		t.Errorf("volume: %v", vol)
	}

	if err := c.SetProperty(context.Background(), "volume", 70); err != nil {
		t.Fatalf("SetProperty: %v", err)
	}
}

func TestClientSurfacesMPVErrors(t *testing.T) {
	srv := newFakeMPVServer(t)
	srv.reply = func(map[string]any) map[string]any {
		return map[string]any{"error": "property unavailable"}
	}
	c, err := Dial(context.Background(), srv.socket, 2*time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.GetProperty(context.Background(), "video-params/w", nil)
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("got %v, want a *CommandError", err)
	}
	if cmdErr.Reason != "property unavailable" {
		t.Errorf("reason: %q", cmdErr.Reason)
	}
}

func TestClientDeliversEvents(t *testing.T) {
	srv := newFakeMPVServer(t)
	c, err := Dial(context.Background(), srv.socket, 2*time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	srv.waitForConn(t)
	srv.pushEvent(map[string]any{"event": "end-file", "reason": "error", "file_error": "loading failed"})

	select {
	case ev := <-c.Events():
		if ev.Name != "end-file" || ev.Reason != "error" {
			t.Errorf("event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered")
	}
}

func TestClientIgnoresGarbageLines(t *testing.T) {
	// mpv occasionally emits lines we do not model; a parse failure must not kill
	// the connection and black out the television.
	srv := newFakeMPVServer(t)
	c, err := Dial(context.Background(), srv.socket, 2*time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	srv.waitForConn(t)
	srv.mu.Lock()
	conns := append([]net.Conn(nil), srv.conns...)
	srv.mu.Unlock()
	for _, conn := range conns {
		_, _ = conn.Write([]byte("this is not json\n\n"))
	}

	// The connection must still work afterwards.
	if err := c.SetProperty(context.Background(), "pause", false); err != nil {
		t.Fatalf("connection died on garbage input: %v", err)
	}
	if !c.Alive() {
		t.Error("client should still be alive")
	}
}

func TestClientCommandAfterCloseFails(t *testing.T) {
	srv := newFakeMPVServer(t)
	c, err := Dial(context.Background(), srv.socket, time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	if _, err := c.Command(context.Background(), "get_property", "volume"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("got %v, want ErrNotConnected", err)
	}
	if c.Alive() {
		t.Error("closed client should not be alive")
	}
	// Close is idempotent.
	_ = c.Close()
}

func TestClientUnblocksPendingRequestsWhenTheServerDies(t *testing.T) {
	srv := newFakeMPVServer(t)
	srv.reply = func(map[string]any) map[string]any {
		// Never reply; the connection will be torn down instead.
		return map[string]any{"__drop": true}
	}
	_ = srv
	c, err := Dial(context.Background(), srv.socket, 5*time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	done := make(chan error, 1)
	go func() {
		_, err := c.Command(context.Background(), "get_property", "volume")
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	srv.closeConns()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a request outstanding when the socket dies must fail")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not unblock when the connection died")
	}
}

func TestClientCommandRespectsContextCancellation(t *testing.T) {
	srv := newFakeMPVServer(t)
	srv.reply = func(map[string]any) map[string]any { return map[string]any{"__drop": true} }
	c, err := Dial(context.Background(), srv.socket, 10*time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Command(ctx, "get_property", "volume"); err == nil {
		t.Error("expected a timeout")
	}
}

func TestDialFailsForMissingSocket(t *testing.T) {
	_, err := Dial(context.Background(), tempSocket(t), time.Second, testLogger())
	if err == nil {
		t.Fatal("expected a dial error")
	}
}

func TestBuildArgsAddsIPCServerAndPreservesOptions(t *testing.T) {
	got := BuildArgs(Options{
		Socket: "/run/timeblaster/mpv-tv.sock",
		Args:   []string{"--vo=gpu", "--gpu-context=drm", "--idle=yes"},
	})
	if got[0] != "--input-ipc-server=/run/timeblaster/mpv-tv.sock" {
		t.Errorf("IPC option must come first: %v", got)
	}
	want := []string{"--vo=gpu", "--gpu-context=drm", "--idle=yes"}
	for i, w := range want {
		if got[i+1] != w {
			t.Fatalf("args: got %v want %v after the IPC option", got, want)
		}
	}
}

func TestBuildArgsDefaultConfigTargetsDRMKMS(t *testing.T) {
	// Raspberry Pi OS Lite has no X11 or Wayland session, so the TV instance must
	// render straight to DRM/KMS and must stay alive with nothing loaded.
	args := BuildArgs(Options{Socket: "/tmp/s.sock", Args: config.Default().MPV.Args})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--gpu-context=drm", "--idle=yes", "--force-window=yes", "--fullscreen"} {
		if !strings.Contains(joined, want) {
			t.Errorf("default mpv args are missing %s: %v", want, args)
		}
	}
}

func TestSupervisorStatusBeforeStart(t *testing.T) {
	s := NewSupervisor(Options{Name: "tv", Binary: "mpv", Socket: "/tmp/nope.sock"}, testLogger())
	st := s.Status()
	if st.Alive || st.Restarts != 0 || st.Name != "tv" {
		t.Errorf("status: %+v", st)
	}
	if s.Alive() {
		t.Error("an unstarted supervisor should not be alive")
	}
	// Every Controller method must fail cleanly rather than panicking.
	ctx := context.Background()
	if _, err := s.Command(ctx, "loadfile", "x"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Command: %v", err)
	}
	if err := s.SetProperty(ctx, "volume", 50); !errors.Is(err, ErrNotConnected) {
		t.Errorf("SetProperty: %v", err)
	}
	if err := s.GetProperty(ctx, "volume", nil); !errors.Is(err, ErrNotConnected) {
		t.Errorf("GetProperty: %v", err)
	}
	if err := s.LoadFile(ctx, "x"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("LoadFile: %v", err)
	}
}

func TestSupervisorRestartsACrashingProcess(t *testing.T) {
	// `true` exits immediately, standing in for an mpv that dies on startup.
	s := NewSupervisor(Options{
		Name:           "crashy",
		Binary:         "true",
		Socket:         tempSocket(t),
		StartupTimeout: 150 * time.Millisecond,
		CommandTimeout: time.Second,
		MinBackoff:     10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := s.Run(ctx)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run should only return on context cancellation, got %v", err)
	}
	if st := s.Status(); st.Restarts < 2 {
		t.Errorf("supervisor should have retried repeatedly, got %d starts", st.Restarts)
	}
}

func TestSupervisorRunStopsOnCancel(t *testing.T) {
	s := NewSupervisor(Options{
		Name: "x", Binary: "true", Socket: tempSocket(t),
		StartupTimeout: 50 * time.Millisecond, MinBackoff: 10 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}
