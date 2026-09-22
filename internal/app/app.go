package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bradsheets/timeblaster/internal/alarm"
	"github.com/bradsheets/timeblaster/internal/audio"
	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/input"
	"github.com/bradsheets/timeblaster/internal/media"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/serialport"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/storage"
	"github.com/bradsheets/timeblaster/internal/system"
	"github.com/bradsheets/timeblaster/internal/web"
	"github.com/bradsheets/timeblaster/internal/wifi"
	"github.com/bradsheets/timeblaster/internal/wsocket"
)

// Deps lets a caller substitute any collaborator. Production passes an empty
// Deps and New builds the real implementations; tests pass fakes and get the
// entire application graph with no hardware, no processes and no network.
type Deps struct {
	Clock       system.Clock
	Runner      system.CommandRunner
	Store       storage.Store
	SerialOpen  serialport.Opener
	Nano        hardware.Nano
	Player      mpv.Controller
	ErsatzTV    ersatztv.API
	AudioPlayer audio.Player
	WiFi        wifi.Manager
	ALSAProbe   *system.ALSAProbe
}

// App is the assembled Timeblaster daemon.
type App struct {
	cfg     config.Config
	log     *slog.Logger
	clock   system.Clock
	version string

	store     storage.Store
	tracker   *state.Tracker
	scheduler *alarm.Scheduler
	audio     *audio.AlarmService
	library   *audio.Library
	router    *input.Router
	link      *hardware.Link
	nano      hardware.Nano
	tvPlayer  mpv.Controller
	tvSuper   *mpv.Supervisor
	overlay   *mpv.OverlayRenderer
	media     *media.Service
	wifi      wifi.Manager
	hub       *wsocket.Hub
	web       *web.Server
	sup       *supervisor

	// ownsStore records whether we opened the database and must close it.
	ownsStore bool
}

// New assembles the application.
//
// Construction order matters: storage, then the alarm path, then everything
// else. If anything after the alarm path fails to build, the daemon still
// starts — an appliance that will not boot because ErsatzTV is missing would be
// a poor alarm clock.
func New(cfg config.Config, log *slog.Logger, version string, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("app: invalid configuration: %w", err)
	}

	clock := deps.Clock
	if clock == nil {
		clock = system.RealClock{}
	}
	runner := deps.Runner
	if runner == nil {
		runner = system.ExecRunner{}
	}

	a := &App{
		cfg:     cfg,
		log:     log,
		clock:   clock,
		version: version,
		sup:     newSupervisor(log),
		tracker: state.NewTracker(version, cfg.General.Hostname, clock.Now()),
	}

	if err := ensureRuntimeDir(cfg.Storage.RuntimeDir); err != nil {
		// Several subsystems write sockets and the fallback tone here. systemd
		// normally provides it via RuntimeDirectory=, so a failure is unusual but
		// not fatal: the affected features degrade individually.
		log.Warn("could not create the runtime directory", "path", cfg.Storage.RuntimeDir, "error", err)
	}
	if err := a.initStorage(deps); err != nil {
		return nil, err
	}
	if err := a.initAudio(deps, runner); err != nil {
		return nil, err
	}
	if err := a.initAlarms(); err != nil {
		return nil, err
	}
	a.initHardware(deps)
	if err := a.initMedia(deps); err != nil {
		// The television is optional. Log loudly and carry on: alarms matter more.
		a.log.Error("television subsystem unavailable; continuing without it", "error", err)
	}
	a.initWiFi(deps)
	if err := a.initWeb(); err != nil {
		return nil, err
	}
	a.registerTasks()
	return a, nil
}

func (a *App) initStorage(deps Deps) error {
	if deps.Store != nil {
		a.store = deps.Store
		return nil
	}
	db, err := storage.Open(a.cfg.Storage.DatabasePath)
	if err != nil {
		// A Timeblaster that rings alarms but forgets them on reboot is far
		// better than one that refuses to start, so fall back to memory and say
		// so at every level of loudness available.
		a.log.Error("could not open the database; running with in-memory state only. "+
			"Alarms created now will not survive a restart",
			"path", a.cfg.Storage.DatabasePath, "error", err)
		a.store = storage.NewMemory()
		return nil
	}
	a.store = db
	a.ownsStore = true
	a.log.Info("database opened", "path", a.cfg.Storage.DatabasePath)
	return nil
}

func (a *App) initAudio(deps Deps, runner system.CommandRunner) error {
	a.library = audio.NewLibrary(a.cfg.Storage.AlarmSoundsDir)
	if err := a.library.Refresh(); err != nil {
		a.log.Error("could not read the alarm sounds directory", "error", err)
	}

	// The built-in tone guarantees an alarm always makes a noise, even if every
	// sound file is missing or corrupt.
	tonePath := filepath.Join(a.cfg.Storage.RuntimeDir, "fallback-tone.wav")
	if tone, err := audio.WriteFallbackTone(tonePath); err != nil {
		a.log.Error("could not create the fallback alarm tone", "path", tonePath, "error", err)
	} else {
		a.library.SetFallback(tone)
	}

	player := deps.AudioPlayer
	if player == nil {
		player = audio.NewMPVPlayer(audio.MPVPlayerOptions{
			Binary:    a.cfg.Audio.PlayerBinary,
			SocketDir: a.cfg.Storage.RuntimeDir,
		}, a.log.With("component", "alarm-player"))
	}

	probe := system.ALSAProbe{Runner: runner}
	if deps.ALSAProbe != nil {
		probe = *deps.ALSAProbe
		if probe.Runner == nil {
			probe.Runner = runner
		}
	}

	audioCfg := a.cfg.Audio
	if id := storage.GetString(a.store, storage.KeyDefaultSoundID, ""); id != "" {
		audioCfg.DefaultSoundID = id
	}

	a.audio = audio.NewService(audioCfg, audio.Deps{
		Library: a.library, Player: player, Probe: probe,
		Runner: runner, Clock: a.clock, Logger: a.log.With("component", "audio"),
	})

	// A missing speaker is logged, not fatal: the alarm still fires, the red
	// button still works, and the device re-probes when one appears.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.audio.ResolveDevice(ctx); err != nil {
		a.log.Error("alarm audio device is not available at startup; "+
			"alarms will still fire and can still be dismissed", "error", err)
	}
	return nil
}

func (a *App) initAlarms() error {
	loc := a.resolveLocation()

	cfg := alarm.DefaultConfig()
	cfg.Location = loc
	cfg.DefaultSound = a.cfg.Audio.DefaultSoundID
	if id := storage.GetString(a.store, storage.KeyDefaultSoundID, ""); id != "" {
		cfg.DefaultSound = id
	}

	a.scheduler = alarm.NewScheduler(cfg, a.store, a.clock, a.log.With("component", "alarm"), nil)
	a.scheduler.SetSink(a)
	if err := a.scheduler.Load(); err != nil {
		return fmt.Errorf("app: loading alarms: %w", err)
	}
	return nil
}

// resolveLocation picks the timezone: the stored user preference wins, then the
// configuration file, then the system default.
func (a *App) resolveLocation() *time.Location {
	if name := storage.GetString(a.store, storage.KeyTimezone, ""); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
		a.log.Warn("stored timezone is not recognised; ignoring it", "timezone", name)
	}
	if a.cfg.General.Timezone != "" {
		if loc, err := time.LoadLocation(a.cfg.General.Timezone); err == nil {
			return loc
		}
		a.log.Warn("configured timezone is not recognised; using the system default",
			"timezone", a.cfg.General.Timezone)
	}
	// time.Local reports itself as "Local", which is useless in the companion
	// app and in logs. Resolve it to the real IANA name where the system records
	// one, so the UI shows "America/New_York" rather than "Local".
	if name := SystemTimezoneName(); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			a.log.Info("using the system timezone", "timezone", name)
			return loc
		}
	}
	return time.Local
}

// SystemTimezoneName reads the host's configured IANA timezone.
//
// Debian and Raspberry Pi OS record it in /etc/timezone; systemd systems also
// expose it as the target of the /etc/localtime symlink. Both are checked
// because neither is universally present, and an empty result simply means
// "fall back to time.Local".
func SystemTimezoneName() string {
	if tz := os.Getenv("TZ"); tz != "" && tz != "Local" {
		return tz
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(b)); name != "" {
			return name
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		// e.g. ../usr/share/zoneinfo/America/New_York
		const marker = "zoneinfo/"
		if i := strings.LastIndex(target, marker); i >= 0 {
			return target[i+len(marker):]
		}
	}
	return ""
}

func (a *App) initHardware(deps Deps) {
	inputCfg := input.DefaultConfig()
	inputCfg.Calibration = input.Calibration{
		Min:              a.cfg.Input.ADCMin,
		Max:              a.cfg.Input.ADCMax,
		EndMarginPercent: a.cfg.Input.EndMarginPercent,
	}
	inputCfg.FilterAlpha = a.cfg.Input.FilterAlpha
	inputCfg.FilterSnap = a.cfg.Input.FilterSnapThreshold
	inputCfg.ChannelHysteresis = a.cfg.Input.ChannelHysteresis
	inputCfg.VolumeDeadband = a.cfg.Input.VolumeDeadbandPercent
	inputCfg.HoldDuration = a.cfg.Input.WiFiHoldDuration.Duration
	inputCfg.MinPressDuration = a.cfg.Input.MinPressDuration.Duration
	inputCfg.PollInterval = a.cfg.Input.HoldPollInterval.Duration

	a.router = input.NewRouter(inputCfg, a.clock, a.log.With("component", "input"), a)

	if deps.Nano != nil {
		a.nano = deps.Nano
		return
	}

	opener := deps.SerialOpen
	if opener == nil {
		opener = serialport.DeviceOpener{Config: serialport.Config{
			Device:      a.cfg.Serial.Device,
			Globs:       a.cfg.Serial.DeviceGlobs,
			BaudRate:    a.cfg.Serial.BaudRate,
			ReadTimeout: a.cfg.Serial.ReadTimeout.Duration,
		}}
	}

	linkCfg := hardware.Config{
		TimeSyncInterval: a.cfg.Serial.TimeSyncInterval.Duration,
		HeartbeatTimeout: a.cfg.Serial.HeartbeatTimeout.Duration,
		ReconnectMin:     a.cfg.Serial.ReconnectMinBackoff.Duration,
		ReconnectMax:     a.cfg.Serial.ReconnectMaxBackoff.Duration,
		WriteQueueSize:   a.cfg.Serial.WriteQueueSize,
		DisplayOn:        a.storedDisplayOn(),
	}
	a.link = hardware.NewLink(linkCfg, hardware.Deps{
		Opener: opener, Clock: a.clock, Logger: a.log.With("component", "nano"),
		Observer: a.router, Lifecycle: a,
	})
	a.nano = a.link
}

func (a *App) initMedia(deps Deps) error {
	etv := deps.ErsatzTV
	if etv == nil {
		client, err := ersatztv.New(ersatztv.Options{
			BaseURL:      a.cfg.ErsatzTV.BaseURL,
			Timeout:      a.cfg.ErsatzTV.RequestTimeout.Duration,
			StreamMode:   a.cfg.ErsatzTV.StreamMode,
			StreamFormat: a.cfg.ErsatzTV.StreamFormat,
		})
		if err != nil {
			return err
		}
		etv = client
	}

	player := deps.Player
	if player == nil {
		a.tvSuper = mpv.NewSupervisor(mpv.Options{
			Name:           "tv",
			Binary:         a.cfg.MPV.Binary,
			Socket:         a.cfg.MPV.IPCSocket,
			Args:           a.cfg.MPV.Args,
			StartupTimeout: a.cfg.MPV.StartupTimeout.Duration,
			CommandTimeout: a.cfg.MPV.CommandTimeout.Duration,
			MinBackoff:     a.cfg.MPV.RestartMinBackoff.Duration,
			MaxBackoff:     a.cfg.MPV.RestartMaxBackoff.Duration,
			OnConnect:      a.onPlayerConnect,
			OnEvent:        a.onPlayerEvent,
		}, a.log.With("component", "mpv"))
		player = a.tvSuper
	}
	a.tvPlayer = player

	overlayCfg := a.cfg.Overlay
	if v, ok, _ := a.store.GetSetting(storage.KeyChannelOverlayOn); ok {
		overlayCfg.Enabled = v == "true"
	}
	a.overlay = mpv.NewOverlayRenderer(overlayCfg, player, a.clock, a.log.With("component", "overlay"))

	a.media = media.NewService(a.cfg.ErsatzTV, a.cfg.MPV, media.Deps{
		ErsatzTV: etv, Player: player, Overlay: a.overlay,
		Clock: a.clock, Logger: a.log.With("component", "media"), Observer: a,
	})
	return nil
}

func (a *App) initWiFi(deps Deps) {
	if deps.WiFi != nil {
		a.wifi = deps.WiFi
		return
	}
	a.wifi = wifi.NewClient(a.cfg.WiFi.HelperSocket,
		a.cfg.WiFi.ConnectTimeout.Duration+a.cfg.WiFi.ValidateTimeout.Duration+30*time.Second,
		a.log.With("component", "wifi"))
}

func (a *App) initWeb() error {
	a.hub = wsocket.NewHub(a.log.With("component", "websocket"), a.Snapshot,
		a.cfg.Web.WebSocketPingInterval.Duration)

	var mediaSvc web.MediaService
	if a.media != nil {
		mediaSvc = a.media
	} else {
		mediaSvc = unavailableMedia{}
	}

	srv, err := web.NewServer(a.cfg.Web, web.Deps{
		Config: a.cfg, Alarms: a.scheduler, Audio: a.audio, Media: mediaSvc,
		Hardware: a.hardwareStatus(), Inputs: a.router, WiFi: a.wifi,
		Settings: a, Snapshots: a, WebSocket: http.Handler(a.hub),
		Logger: a.log.With("component", "web"), Version: a.version,
	})
	if err != nil {
		return err
	}
	a.web = srv
	return nil
}

// registerTasks declares the supervised goroutines.
//
// Only the alarm scheduler is critical. Everything else is allowed to fail
// permanently without taking the daemon down, which is the whole point.
func (a *App) registerTasks() {
	a.sup.add("alarm-scheduler", true, a.scheduler.Run)
	a.sup.add("input-router", false, a.router.Run)
	a.sup.add("web", false, a.web.Run)

	if a.link != nil {
		a.sup.add("nano-link", false, a.link.Run)
	}
	if a.tvSuper != nil {
		a.sup.add("tv-player", false, a.tvSuper.Run)
	}
	if a.media != nil {
		a.sup.add("channel-refresh", false, a.media.Run)
	}
	a.sup.add("clock-broadcast", false, a.runClockBroadcast)
	a.sup.add("sound-library-refresh", false, a.runLibraryRefresh)
}

// Run starts the application and blocks until the context is cancelled.
func (a *App) Run(ctx context.Context) error {
	a.log.Info("timeblaster starting",
		"version", a.version,
		"hostname", a.cfg.General.Hostname,
		"timezone", a.scheduler.Location().String(),
		"database", a.cfg.Storage.DatabasePath,
		"web", a.cfg.Web.ListenAddress)

	// Put something on the television before anything else can, so a Linux
	// console is never visible.
	if a.media != nil {
		go func() {
			showCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := a.media.ShowNoChannel(showCtx); err != nil {
				a.log.Debug("could not show the standby image at startup", "error", err)
			}
		}()
	}

	err := a.sup.run(ctx)

	a.log.Info("timeblaster shutting down")
	a.shutdown()
	return err
}

// shutdown releases resources in an order that leaves the device quiet and the
// database consistent.
func (a *App) shutdown() {
	// Silence the speaker first: an alarm still ringing after the service stops
	// would be the single most annoying possible failure.
	if err := a.audio.StopAlarm(); err != nil {
		a.log.Warn("could not stop alarm audio during shutdown", "error", err)
	}
	if a.nano != nil {
		_ = a.nano.SetAlarmActive(false)
		_ = a.nano.ShowClock()
	}
	if a.hub != nil {
		a.hub.Close()
	}
	if a.ownsStore {
		if err := a.store.Close(); err != nil {
			a.log.Warn("could not close the database cleanly", "error", err)
		}
	}
	a.log.Info("timeblaster stopped")
}

// runClockBroadcast pushes the time to connected apps once a second.
//
// It is a cheap way to keep the companion app's clock anchored to the Pi rather
// than to the phone, and it doubles as a heartbeat that tells the app the device
// is alive.
func (a *App) runClockBroadcast(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if a.hub == nil || a.hub.Clients() == 0 {
				continue // nobody is listening; do no work at all
			}
			a.publish(state.NewEvent(state.EventTick, map[string]any{
				"now": a.clock.Now(),
			}))
		}
	}
}

// runLibraryRefresh rescans the sounds directory periodically so an MP3 copied
// onto the device shows up without a restart.
func (a *App) runLibraryRefresh(ctx context.Context) error {
	const interval = 5 * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := a.library.Refresh(); err != nil {
				a.log.Debug("periodic sound library refresh failed", "error", err)
			}
		}
	}
}

func (a *App) onPlayerConnect(ctx context.Context, c *mpv.Client) {
	// Observing properties beats polling them: the daemon stays idle while a
	// channel plays.
	if err := c.ObserveProperty(ctx, 1, "idle-active"); err != nil {
		a.log.Debug("could not observe mpv idle state", "error", err)
	}
	if a.media != nil {
		a.media.RestorePlayback(ctx)
	}
}

func (a *App) onPlayerEvent(ev mpv.Event) {
	if a.media != nil {
		a.media.HandlePlayerEvent(ev)
	}
}

func (a *App) hardwareStatus() web.HardwareService {
	if a.link != nil {
		return a.link
	}
	return staticHardware{nano: a.nano}
}

// staticHardware reports a substituted Nano's state when there is no real link,
// which is what lets the daemon run on a development machine.
type staticHardware struct{ nano hardware.Nano }

func (s staticHardware) Status() hardware.Status {
	return hardware.Status{Connected: s.nano != nil && s.nano.Connected(), Device: "(substituted)"}
}

func (s staticHardware) Connected() bool { return s.nano != nil && s.nano.Connected() }

// unavailableMedia stands in when the television subsystem could not be built,
// so the API returns empty results rather than nil-dereferencing.
type unavailableMedia struct{}

func (unavailableMedia) Channels() []ersatztv.Channel        { return nil }
func (unavailableMedia) Current() *ersatztv.Channel          { return nil }
func (unavailableMedia) ShowNoChannel(context.Context) error { return nil }
func (unavailableMedia) RefreshSoon()                        {}
func (unavailableMedia) Status() media.Status {
	return media.Status{LastError: "the television subsystem is not configured"}
}
func (unavailableMedia) SelectNumber(context.Context, string) error {
	return errors.New("the television subsystem is not configured")
}

// ensure the runtime directory exists early, since several subsystems write there.
func ensureRuntimeDir(path string) error {
	if path == "" {
		return nil
	}
	return os.MkdirAll(path, 0o750)
}
