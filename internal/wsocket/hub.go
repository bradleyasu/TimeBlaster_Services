// Package wsocket fans application events out to connected companion apps.
//
// The contract that matters: a slow or dead client is dropped, never waited on.
// A phone that went to sleep with the app open must not be able to stall the
// alarm scheduler, so every send is non-blocking and a client that cannot keep
// up loses its connection rather than applying backpressure upstream.
package wsocket

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/bradsheets/timeblaster/internal/state"
)

// clientBuffer is how many events may be queued for one client before it is
// considered too slow to keep.
const clientBuffer = 32

// SnapshotFunc returns the current state, sent to each client on connection so
// it starts from the truth rather than from whatever it last remembered.
type SnapshotFunc func() state.Snapshot

// Hub broadcasts events to connected WebSocket clients.
type Hub struct {
	log      *slog.Logger
	snapshot SnapshotFunc
	// pingInterval keeps connections alive through phone power management and
	// consumer NAT timeouts.
	pingInterval time.Duration

	mu      sync.RWMutex
	clients map[*client]struct{}

	dropped atomic.Int64
	total   atomic.Int64
}

type client struct {
	conn   *websocket.Conn
	send   chan state.Event
	closed chan struct{}
	once   sync.Once
}

func (c *client) close() {
	c.once.Do(func() { close(c.closed) })
}

// NewHub creates a hub.
func NewHub(log *slog.Logger, snapshot SnapshotFunc, pingInterval time.Duration) *Hub {
	if pingInterval <= 0 {
		pingInterval = 20 * time.Second
	}
	return &Hub{
		log:          log,
		snapshot:     snapshot,
		pingInterval: pingInterval,
		clients:      map[*client]struct{}{},
	}
}

// Publish broadcasts an event to every connected client.
//
// It never blocks: a client whose buffer is full is disconnected, on the
// principle that a phone falling behind is its problem, not the alarm clock's.
func (h *Hub) Publish(ev state.Event) {
	h.mu.RLock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		select {
		case c.send <- ev:
		default:
			h.dropped.Add(1)
			h.log.Debug("dropping a slow WebSocket client", "event", ev.Type)
			c.close()
		}
	}
}

// Clients reports how many companion apps are connected.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Stats reports connection counters for diagnostics.
func (h *Hub) Stats() (connected int, total, dropped int64) {
	return h.Clients(), h.total.Load(), h.dropped.Load()
}

// ServeHTTP upgrades a request and serves one client until it disconnects.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The companion app is served from this same origin, but a user may also
		// reach the device by IP address or by a different .local name, so origin
		// checking is relaxed. The API behind it is read-mostly and the device
		// lives on a home LAN; this is a deliberate, documented trade-off rather
		// than an oversight.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		h.log.Debug("WebSocket upgrade failed", "error", err, "remote", r.RemoteAddr)
		return
	}

	c := &client{
		conn:   conn,
		send:   make(chan state.Event, clientBuffer),
		closed: make(chan struct{}),
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	count := len(h.clients)
	h.mu.Unlock()
	h.total.Add(1)

	h.log.Info("companion app connected", "remote", r.RemoteAddr, "clients", count)

	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		remaining := len(h.clients)
		h.mu.Unlock()
		c.close()
		_ = conn.CloseNow()
		h.log.Info("companion app disconnected", "remote", r.RemoteAddr, "clients", remaining)
	}()

	ctx := r.Context()

	// Send the current state immediately so the app renders correctly before any
	// change happens to be broadcast.
	if h.snapshot != nil {
		snap := h.snapshot()
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := wsjson.Write(writeCtx, conn, state.NewEvent(state.EventState, snap))
		cancel()
		if err != nil {
			h.log.Debug("could not send the initial snapshot", "error", err)
			return
		}
	}

	// Drain inbound frames. The companion app does not send commands over the
	// socket — it uses the HTTP API — but reading is what detects a dropped
	// connection promptly.
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				c.close()
				return
			}
		}
	}()

	ticker := time.NewTicker(h.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case ev := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := wsjson.Write(writeCtx, conn, ev)
			cancel()
			if err != nil {
				h.log.Debug("WebSocket write failed", "error", err)
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				h.log.Debug("WebSocket ping failed", "error", err)
				return
			}
		}
	}
}

// Close disconnects every client, used during graceful shutdown.
func (h *Hub) Close() {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.clients = map[*client]struct{}{}
	h.mu.Unlock()

	for _, c := range clients {
		c.close()
		_ = c.conn.Close(websocket.StatusGoingAway, "timeblaster is shutting down")
	}
}

// encodeEvent is used by tests to assert the JSON shape the app receives.
func encodeEvent(ev state.Event) ([]byte, error) { return json.Marshal(ev) }

var _ state.Publisher = (*Hub)(nil)
