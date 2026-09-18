// Package web serves the Timeblaster companion application: a JSON API, a
// WebSocket endpoint and the PWA itself.
//
// Every handler delegates to a subsystem interface. The web layer contains no
// business logic, which is what keeps "the phone app is open" from being able to
// influence anything the alarm clock does. Closing the app has no effect on the
// device at all.
package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/audio"
	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/media"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/wifi"
)

// AlarmService is the alarm subsystem as the API sees it. alarm.Scheduler
// implements it.
type AlarmService interface {
	List() []alarm.Alarm
	Get(id int64) (alarm.Alarm, error)
	Save(a alarm.Alarm) (alarm.Alarm, error)
	Delete(id int64) error
	SetEnabled(id int64, enabled bool) (alarm.Alarm, error)
	Active() *alarm.Active
	Next() *alarm.Upcoming
	Dismiss() error
	Snooze() error
	Trigger(id int64) error
	Location() *time.Location
}

// AudioService is the alarm audio subsystem as the API sees it.
type AudioService interface {
	Sounds() []audio.Sound
	TestAlarm(ctx context.Context, soundID string, d time.Duration) error
	StopAlarm() error
	SetAlarmVolume(ctx context.Context, pct int, physical bool) error
	Volume() (int, bool)
	Health() audio.Health
}

// MediaService is the television subsystem as the API sees it.
type MediaService interface {
	Channels() []ersatztv.Channel
	Current() *ersatztv.Channel
	SelectNumber(ctx context.Context, number string) error
	ShowNoChannel(ctx context.Context) error
	Status() media.Status
	RefreshSoon()
}

// HardwareService is the Nano link as the API sees it.
type HardwareService interface {
	Status() hardware.Status
	Connected() bool
}

// InputService reports the physical controls' positions.
type InputService interface {
	// Position returns a knob's filtered position and whether it has reported.
	Position(index int) (float64, bool)
}

// Settings is the user-adjustable preference set exposed by the API.
type Settings struct {
	Timezone          string `json:"timezone"`
	Clock24h          bool   `json:"clock_24h"`
	DisplayBrightness int    `json:"display_brightness"`
	DefaultSoundID    string `json:"default_sound_id"`
	OverlayEnabled    bool   `json:"channel_overlay_enabled"`
}

// SettingsService reads and writes preferences. The application wires it to
// storage plus the subsystems that need to react.
type SettingsService interface {
	Settings() Settings
	UpdateSettings(ctx context.Context, s Settings) (Settings, error)
}

// SnapshotService builds the full state document.
type SnapshotService interface {
	Snapshot() state.Snapshot
	Health() state.Health
}

// Deps are the web server's collaborators.
type Deps struct {
	Config    config.Config
	Alarms    AlarmService
	Audio     AudioService
	Media     MediaService
	Hardware  HardwareService
	Inputs    InputService
	WiFi      wifi.Manager
	Settings  SettingsService
	Snapshots SnapshotService
	// WebSocket is the hub handler, mounted at /api/ws.
	WebSocket http.Handler
	Logger    *slog.Logger
	Version   string
}

// Server is the companion app's HTTP server.
type Server struct {
	cfg  config.Web
	deps Deps
	log  *slog.Logger
	http *http.Server
}

// NewServer builds the server and its routes.
func NewServer(cfg config.Web, deps Deps) (*Server, error) {
	if deps.Logger == nil {
		return nil, errors.New("web: no logger supplied")
	}
	s := &Server{cfg: cfg, deps: deps, log: deps.Logger}

	handler, err := s.routes()
	if err != nil {
		return nil, err
	}

	s.http = &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout.Duration,
		// WriteTimeout must not apply to the WebSocket endpoint, which is
		// long-lived by design; it is disabled here and enforced per-request by
		// the JSON handlers' own context deadlines.
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}
	return s, nil
}

func (s *Server) routes() (http.Handler, error) {
	mux := http.NewServeMux()

	// Diagnostics.
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/state", s.handleState)

	// Alarms.
	mux.HandleFunc("GET /api/alarms", s.handleListAlarms)
	mux.HandleFunc("POST /api/alarms", s.handleCreateAlarm)
	mux.HandleFunc("GET /api/alarms/{id}", s.handleGetAlarm)
	mux.HandleFunc("PUT /api/alarms/{id}", s.handleUpdateAlarm)
	mux.HandleFunc("DELETE /api/alarms/{id}", s.handleDeleteAlarm)
	mux.HandleFunc("POST /api/alarms/{id}/enabled", s.handleSetAlarmEnabled)
	mux.HandleFunc("POST /api/alarms/{id}/trigger", s.handleTriggerAlarm)

	// The currently ringing alarm.
	mux.HandleFunc("GET /api/alarm/active", s.handleActiveAlarm)
	mux.HandleFunc("POST /api/alarm/dismiss", s.handleDismiss)
	mux.HandleFunc("POST /api/alarm/snooze", s.handleSnooze)

	// Sounds and volume.
	mux.HandleFunc("GET /api/sounds", s.handleListSounds)
	mux.HandleFunc("POST /api/sounds/{id}/preview", s.handlePreviewSound)
	mux.HandleFunc("POST /api/sounds/preview/stop", s.handleStopPreview)
	mux.HandleFunc("GET /api/volume", s.handleGetVolume)
	mux.HandleFunc("PUT /api/volume", s.handleSetVolume)

	// Television.
	mux.HandleFunc("GET /api/channels", s.handleListChannels)
	mux.HandleFunc("POST /api/channels/refresh", s.handleRefreshChannels)
	mux.HandleFunc("POST /api/channels/select", s.handleSelectChannel)
	mux.HandleFunc("POST /api/channels/clear", s.handleClearChannel)

	// Settings.
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)

	// Networking. These proxy to the privileged helper; the daemon itself can do
	// none of it.
	mux.HandleFunc("GET /api/wifi/status", s.handleWiFiStatus)
	mux.HandleFunc("GET /api/wifi/networks", s.handleWiFiScan)
	mux.HandleFunc("POST /api/wifi/setup", s.handleEnterSetup)
	mux.HandleFunc("DELETE /api/wifi/setup", s.handleExitSetup)
	mux.HandleFunc("POST /api/wifi/connect", s.handleWiFiConnect)

	if s.deps.WebSocket != nil {
		mux.Handle("/api/ws", s.deps.WebSocket)
	}

	// The PWA itself, served last so API routes always win.
	static, err := staticHandler(s.cfg.StaticDir)
	if err != nil {
		return nil, err
	}
	mux.Handle("/", static)

	return s.middleware(mux), nil
}

// middleware applies logging, panic recovery and the headers the PWA needs.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if v := recover(); v != nil {
				// A panic in an HTTP handler must never take the daemon — and with
				// it the alarm clock — down.
				s.log.Error("panic in an HTTP handler",
					"panic", v, "method", r.Method, "path", r.URL.Path)
				if !rec.wrote {
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				}
			}
			// Request logging is debug-level: an open companion app polls, and
			// this would otherwise drown the useful entries in the journal.
			s.log.Debug("http request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration", time.Since(start).Round(time.Millisecond), "remote", remoteHost(r))
		}()

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// The app is used from a phone on the same LAN; allowing simple
		// cross-origin reads keeps a bookmarked IP address working alongside the
		// .local name. Credentials are never used, so this exposes nothing that
		// the device does not already serve to anyone on the network.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(rec, r)
	})
}

// Run serves until the context is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("web: listening on %s: %w", s.cfg.ListenAddress, err)
	}

	s.log.Info("companion app listening",
		"address", ln.Addr().String(),
		"url", fmt.Sprintf("http://%s.local%s", s.deps.Config.General.Hostname, portSuffix(s.cfg.ListenAddress)))

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	timeout := s.cfg.ShutdownTimeout.Duration
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		s.log.Warn("the companion app did not shut down cleanly", "error", err)
		_ = s.http.Close()
	}
	s.log.Info("companion app stopped")
	return ctx.Err()
}

// Handler exposes the routed handler, which tests exercise without binding a port.
func (s *Server) Handler() http.Handler { return s.http.Handler }

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets the WebSocket upgrade reach the underlying ResponseWriter's
// hijacker through the recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func portSuffix(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "80" {
		return ""
	}
	return ":" + port
}

// staticHandler serves the PWA, from disk when a directory is configured (which
// is what makes front-end development pleasant) and otherwise from the assets
// embedded in the binary.
func staticHandler(dir string) (http.Handler, error) {
	var fsys fs.FS
	if dir != "" {
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("web: static_dir %s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("web: static_dir %s is not a directory", dir)
		}
		fsys = os.DirFS(dir)
	} else {
		sub, err := fs.Sub(embeddedStatic, "static")
		if err != nil {
			return nil, fmt.Errorf("web: reading the embedded assets: %w", err)
		}
		fsys = sub
	}
	return newSPAHandler(fsys), nil
}
