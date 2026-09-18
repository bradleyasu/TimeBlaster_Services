package wsocket

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/bradsheets/timeblaster/internal/state"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestHub(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	hub := NewHub(testLogger(), func() state.Snapshot {
		return state.Snapshot{Version: "test", Hostname: "timeblaster"}
	}, 50*time.Millisecond)
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	return hub, srv
}

func dial(t *testing.T, srv *httptest.Server) (*websocket.Conn, context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	return conn, ctx, cancel
}

func TestHubSendsSnapshotOnConnect(t *testing.T) {
	_, srv := newTestHub(t)
	conn, ctx, cancel := dial(t, srv)
	defer cancel()
	defer conn.CloseNow()

	var ev state.Event
	if err := wsjson.Read(ctx, conn, &ev); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ev.Type != state.EventState {
		t.Fatalf("first event should be a snapshot, got %s", ev.Type)
	}
	// The payload must round-trip as the snapshot the app expects.
	b, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatal(err)
	}
	var snap state.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("snapshot did not decode: %v", err)
	}
	if snap.Hostname != "timeblaster" {
		t.Errorf("snapshot: %+v", snap)
	}
}

func TestHubBroadcastsEvents(t *testing.T) {
	hub, srv := newTestHub(t)
	conn, ctx, cancel := dial(t, srv)
	defer cancel()
	defer conn.CloseNow()

	var first state.Event
	if err := wsjson.Read(ctx, conn, &first); err != nil {
		t.Fatal(err)
	}

	waitForClients(t, hub, 1)
	hub.Publish(state.NewEvent(state.EventVolumeChanged, map[string]int{"percent": 65}))

	var ev state.Event
	if err := wsjson.Read(ctx, conn, &ev); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ev.Type != state.EventVolumeChanged {
		t.Fatalf("got %s", ev.Type)
	}
	data, _ := ev.Data.(map[string]any)
	if data["percent"] != float64(65) {
		t.Errorf("payload: %+v", ev.Data)
	}
	if ev.At.IsZero() {
		t.Error("events should be timestamped")
	}
}

func TestHubBroadcastsToEveryClient(t *testing.T) {
	hub, srv := newTestHub(t)

	const n = 3
	conns := make([]*websocket.Conn, n)
	ctxs := make([]context.Context, n)
	for i := range n {
		conn, ctx, cancel := dial(t, srv)
		defer cancel()
		defer conn.CloseNow()
		conns[i], ctxs[i] = conn, ctx
		var snap state.Event
		if err := wsjson.Read(ctx, conn, &snap); err != nil {
			t.Fatal(err)
		}
	}
	waitForClients(t, hub, n)

	hub.Publish(state.NewEvent(state.EventAlarmStarted, nil))
	for i := range n {
		var ev state.Event
		if err := wsjson.Read(ctxs[i], conns[i], &ev); err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		if ev.Type != state.EventAlarmStarted {
			t.Errorf("client %d got %s", i, ev.Type)
		}
	}
}

func TestHubPublishNeverBlocks(t *testing.T) {
	// The important property: a phone that stopped reading must not be able to
	// stall the goroutine that is trying to announce an alarm.
	hub, srv := newTestHub(t)
	conn, ctx, cancel := dial(t, srv)
	defer cancel()
	var snap state.Event
	if err := wsjson.Read(ctx, conn, &snap); err != nil {
		t.Fatal(err)
	}
	waitForClients(t, hub, 1)

	// Stop reading entirely, then flood.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range clientBuffer * 10 {
			hub.Publish(state.NewEvent(state.EventTick, time.Now()))
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked on a slow client")
	}

	// The slow client should have been dropped rather than tolerated forever.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, dropped := hub.Stats(); dropped > 0 {
			conn.CloseNow()
			return
		}
		time.Sleep(time.Millisecond)
	}
	conn.CloseNow()
	t.Error("a slow client was never dropped")
}

func TestHubRemovesDisconnectedClients(t *testing.T) {
	hub, srv := newTestHub(t)
	conn, ctx, cancel := dial(t, srv)
	defer cancel()
	var snap state.Event
	if err := wsjson.Read(ctx, conn, &snap); err != nil {
		t.Fatal(err)
	}
	waitForClients(t, hub, 1)

	conn.CloseNow()
	waitForClients(t, hub, 0)
}

func TestHubCloseDisconnectsEveryone(t *testing.T) {
	hub, srv := newTestHub(t)
	conn, ctx, cancel := dial(t, srv)
	defer cancel()
	defer conn.CloseNow()
	var snap state.Event
	if err := wsjson.Read(ctx, conn, &snap); err != nil {
		t.Fatal(err)
	}
	waitForClients(t, hub, 1)

	hub.Close()
	waitForClients(t, hub, 0)

	// The client should see the connection go away rather than hang.
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	var ev state.Event
	if err := wsjson.Read(readCtx, conn, &ev); err == nil {
		t.Error("expected the connection to be closed")
	}
}

func TestHubPublishWithNoClientsIsSafe(t *testing.T) {
	hub := NewHub(testLogger(), nil, time.Second)
	hub.Publish(state.NewEvent(state.EventTick, nil))
	if n := hub.Clients(); n != 0 {
		t.Errorf("clients: %d", n)
	}
}

func TestEventJSONShape(t *testing.T) {
	// The companion app switches on `type`, so the field name is part of the API.
	b, err := encodeEvent(state.NewEvent(state.EventChannelChanged, map[string]string{"number": "3"}))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "channel_changed" {
		t.Errorf("type: %v", decoded["type"])
	}
	if _, ok := decoded["at"]; !ok {
		t.Error("missing timestamp")
	}
}

func waitForClients(t *testing.T, h *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Clients() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d clients, have %d", want, h.Clients())
}
