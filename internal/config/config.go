// Package config defines Timeblaster's configuration file, its defaults and its
// validation rules.
//
// Every tunable in the system lives here rather than as a constant scattered
// through the code, so that behaviour can be adjusted on a running appliance by
// editing /etc/timeblaster/timeblaster.toml and restarting the service.
//
// The rules the rest of the codebase relies on:
//
//   - Default() returns a complete, valid configuration. A missing file is not an
//     error; an unparsable one is.
//   - Validate() is total: if it returns nil, no consumer needs to re-check ranges.
//   - Unknown keys are reported rather than silently ignored, because a typo in an
//     appliance's config file is otherwise invisible until something misbehaves.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where setup.sh installs the configuration file.
const DefaultPath = "/etc/timeblaster/timeblaster.toml"

// Duration wraps time.Duration so it can be written in TOML as a string such as
// "5s" or "250ms" instead of a raw nanosecond count.
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("config: %q is not a duration (try \"5s\" or \"250ms\"): %w", text, err)
	}
	d.Duration = v
	return nil
}

// MarshalText renders the duration back to its string form.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Dur is a convenience constructor for defaults.
func Dur(d time.Duration) Duration { return Duration{d} }

// Config is the complete Timeblaster configuration.
type Config struct {
	General  General  `toml:"general"`
	Logging  Logging  `toml:"logging"`
	Storage  Storage  `toml:"storage"`
	Serial   Serial   `toml:"serial"`
	Input    Input    `toml:"input"`
	Audio    Audio    `toml:"audio"`
	ErsatzTV ErsatzTV `toml:"ersatztv"`
	MPV      MPV      `toml:"mpv"`
	Overlay  Overlay  `toml:"overlay"`
	Web      Web      `toml:"web"`
	WiFi     WiFi     `toml:"wifi"`
}

// General holds device-wide settings.
type General struct {
	// Hostname is the mDNS name the device advertises. Changing it here does not
	// rename the host; setup.sh does that. It is used for logging and for the URL
	// printed in the UI.
	Hostname string `toml:"hostname"`
	// Timezone is an IANA name such as "America/New_York". Empty means the system
	// timezone. The value stored in the database overrides this once a user picks
	// one in the companion app.
	Timezone string `toml:"timezone"`
	// Clock24h selects 24-hour time on the 7-segment display and in the UI.
	//
	// The display formats the time itself from a unix timestamp, so this is
	// pushed to the Nano over CONFIG and re-sent on every reconnect.
	Clock24h bool `toml:"clock_24h"`
	// DisplayOn turns the 7-segment display on or off.
	//
	// It is a switch rather than a brightness level because the display has no
	// dimmer: the 74HC595s' ~OE is tied low on the board, so the hardware can
	// only be lit or dark. Exposing a 0-100 slider that did nothing except at
	// zero would be a lie. If ~OE is ever wired to a PWM pin, this is where a
	// level would come back.
	DisplayOn bool `toml:"display_on"`
	// RestoreChannelOnBoot re-selects the last channel at startup. It is off by
	// default because the channel knob is an absolute position control: the knob's
	// physical position is the truth, and the first reading from the Nano will
	// select the right channel within a second anyway.
	RestoreChannelOnBoot bool `toml:"restore_channel_on_boot"`
	// ChannelBanner is how long the seven-segment display shows the channel
	// number ("Ch.02") after a channel change before returning to the clock.
	//
	// Zero turns the banner off and leaves the clock up throughout.
	ChannelBanner Duration `toml:"channel_banner_duration"`
}

// Logging controls journald output.
type Logging struct {
	// Level is debug, info, warn or error.
	Level string `toml:"level"`
	// Format is "text" (journald-friendly logfmt) or "json".
	Format string `toml:"format"`
	// LogPotReadings enables per-sample potentiometer logging. It is separate from
	// the debug level because it is a firehose that drowns everything else.
	LogPotReadings bool `toml:"log_pot_readings"`
}

// Storage configures durable state.
type Storage struct {
	// DatabasePath is the SQLite database holding alarms and settings.
	DatabasePath string `toml:"database_path"`
	// AlarmSoundsDir holds the MP3 files offered as alarm sounds.
	AlarmSoundsDir string `toml:"alarm_sounds_dir"`
	// RuntimeDir holds sockets and generated files. systemd provides it via
	// RuntimeDirectory=timeblaster.
	RuntimeDir string `toml:"runtime_dir"`
}

// Serial configures the link to the Arduino Nano.
type Serial struct {
	// Device is an explicit device path. Leave it empty to use DeviceGlobs, which
	// is strongly preferred: /dev/ttyACM0 is not stable across reboots.
	Device string `toml:"device"`
	// DeviceGlobs are tried in order until one matches. The udev rule installed by
	// setup.sh creates /dev/timeblaster-nano; /dev/serial/by-id/* is the fallback
	// that works without the rule.
	DeviceGlobs []string `toml:"device_globs"`
	// BaudRate must match the firmware. USB CDC ignores it, but a real UART would
	// not, and keeping it explicit documents the intent.
	BaudRate int `toml:"baud_rate"`
	// ReconnectMinBackoff and ReconnectMaxBackoff bound the reconnect loop.
	ReconnectMinBackoff Duration `toml:"reconnect_min_backoff"`
	ReconnectMaxBackoff Duration `toml:"reconnect_max_backoff"`
	// ReadTimeout bounds a single read so a wedged port is noticed.
	ReadTimeout Duration `toml:"read_timeout"`
	// WaitForTimeSync holds the Nano's loading animation until the Pi's clock
	// has been set from a time server.
	//
	// A Pi 5 with no coin cell on its RTC connector boots knowing nothing, and
	// systemd-timesyncd winds the clock forward to roughly when the Pi was last
	// powered rather than leave it in 1970. That stale time looks completely
	// plausible, so without this the display shows yesterday's time for the
	// half-minute until NTP corrects it.
	//
	// Turn it off for an installation that deliberately runs with no time
	// server, where the clock would otherwise never appear at all.
	WaitForTimeSync bool `toml:"wait_for_time_sync"`
	// TimeSyncInterval is how often the Pi pushes the current time to the Nano.
	// The Nano free-runs its display between syncs, so this can be generous.
	TimeSyncInterval Duration `toml:"time_sync_interval"`
	// HeartbeatTimeout is how long without a PING before the link is considered
	// dead and reopened.
	HeartbeatTimeout Duration `toml:"heartbeat_timeout"`
	// WriteQueueSize bounds buffered outbound messages. When it is full the oldest
	// non-critical message is dropped rather than blocking a caller.
	WriteQueueSize int `toml:"write_queue_size"`
}

// Input configures potentiometer and button processing.
type Input struct {
	// ADCMin and ADCMax are the raw readings that map to 0 % and 100 %.
	ADCMin int `toml:"adc_min"`
	ADCMax int `toml:"adc_max"`
	// EndMarginPercent snaps the outer band of pot travel to the extremes, so a
	// knob that cannot quite reach the rail still reaches 0 % and 100 %.
	EndMarginPercent float64 `toml:"end_margin_percent"`
	// InvertChannelPot and InvertVolumePot handle pots wired backwards.
	InvertChannelPot bool `toml:"invert_channel_pot"`
	InvertVolumePot  bool `toml:"invert_volume_pot"`
	// FilterAlpha is the EMA weight for new samples: lower smooths harder.
	FilterAlpha float64 `toml:"filter_alpha"`
	// FilterSnapThreshold is the normalised jump above which the filter follows
	// immediately, so deliberate fast turns feel responsive.
	FilterSnapThreshold float64 `toml:"filter_snap_threshold"`
	// ChannelHysteresis is the extra travel, as a fraction of one channel band,
	// required to move between channels. It is what stops ADC noise on a boundary
	// from flipping channels repeatedly.
	ChannelHysteresis float64 `toml:"channel_hysteresis"`
	// VolumeDeadbandPercent is the minimum volume change worth acting on.
	VolumeDeadbandPercent int `toml:"volume_deadband_percent"`
	// WiFiHoldDuration is how long the Wi-Fi button must be held to enter setup
	// mode. This is the one and only entry path to Wi-Fi setup.
	WiFiHoldDuration Duration `toml:"wifi_hold_duration"`
	// MinPressDuration rejects contact bounce that survived firmware debouncing.
	MinPressDuration Duration `toml:"min_press_duration"`
	// HoldPollInterval is how often a held button is checked. The poll loop only
	// runs while a button is actually down.
	HoldPollInterval Duration `toml:"hold_poll_interval"`
}

// Audio configures the dedicated alarm speaker path.
type Audio struct {
	// AlarmDevice selects the USB audio card. Prefer a stable identifier:
	// a card id ("Device"), an ALSA string ("hw:CARD=Device,DEV=0") or a substring
	// of the card description ("USB"). An empty value picks the first USB audio
	// card, falling back to the first card present.
	AlarmDevice string `toml:"alarm_device"`
	// MixerControl overrides the auto-detected hardware volume control. Empty
	// means "detect"; "none" forces software volume.
	MixerControl string `toml:"mixer_control"`
	// StartupVolumePercent is used until the volume knob reports its position.
	// Persisted volume is deliberately not replayed: the knob is authoritative.
	StartupVolumePercent int `toml:"startup_volume_percent"`
	// AllowSoftwareVolume lets the companion app set volume. Even when enabled,
	// the next physical knob movement wins.
	AllowSoftwareVolume bool `toml:"allow_software_volume"`
	// PlayerBinary is the audio-only player. mpv is the default because it is
	// already installed for video and exposes the same JSON IPC.
	PlayerBinary string `toml:"player_binary"`
	// ExtraPlayerArgs are appended to the generated command line.
	ExtraPlayerArgs []string `toml:"extra_player_args"`
	// DefaultSoundID is the sound used by alarms that name none.
	DefaultSoundID string `toml:"default_sound_id"`
	// DeviceRecheckInterval is how often an unavailable USB speaker is re-probed.
	DeviceRecheckInterval Duration `toml:"device_recheck_interval"`
	// TestDuration bounds a "test this sound" playback from the companion app.
	TestDuration Duration `toml:"test_duration"`
}

// ErsatzTV configures the channel source.
type ErsatzTV struct {
	// BaseURL is where ErsatzTV listens. It runs on the same Pi by default.
	BaseURL string `toml:"base_url"`
	// RequestTimeout bounds a single API call.
	RequestTimeout Duration `toml:"request_timeout"`
	// RefreshInterval is how often the channel list is re-read.
	RefreshInterval Duration `toml:"refresh_interval"`
	// RetryMinBackoff and RetryMaxBackoff bound reconnection while ErsatzTV is
	// starting up or restarting.
	RetryMinBackoff Duration `toml:"retry_min_backoff"`
	RetryMaxBackoff Duration `toml:"retry_max_backoff"`
	// StreamMode is appended to stream URLs. "mixed" lets ErsatzTV decide;
	// "hls-direct" avoids transcoding when the media is already H.264/AAC in MP4,
	// which is the intended library format.
	StreamMode string `toml:"stream_mode"`
	// StreamFormat is "m3u8" (HLS) or "ts" (MPEG-TS).
	StreamFormat string `toml:"stream_format"`
}

// MPV configures the television playback instance.
type MPV struct {
	// Binary is the mpv executable.
	Binary string `toml:"binary"`
	// IPCSocket is the JSON IPC socket path. The daemon talks to mpv over this
	// rather than restarting it on every channel change.
	IPCSocket string `toml:"ipc_socket"`
	// Args are the mpv options used for HDMI output. The defaults target
	// Raspberry Pi OS Lite on a Pi 5: DRM/KMS output with no X11 or Wayland
	// session. See docs/troubleshooting.md for alternatives if your TV or HDMI
	// mode misbehaves.
	Args []string `toml:"args"`
	// NoChannelImage is shown fullscreen whenever no channel is selected, which is
	// what keeps a Linux console off the television.
	NoChannelImage string `toml:"no_channel_image"`
	// BootingImage is shown instead, from startup until the channel list has
	// been read for the first time. It is the same screen the boot splash
	// paints, so the handover from framebuffer to mpv is invisible -- and it
	// avoids telling the user to turn the channel knob before any channels
	// exist. Empty falls back to NoChannelImage.
	BootingImage string `toml:"booting_image"`
	// BootingTimeout caps how long the booting screen may stay up. Past it the
	// no-channel screen is shown even if the channel list never loaded, because
	// at that point the device is not booting, something is wrong.
	BootingTimeout Duration `toml:"booting_timeout"`
	// RestartMinBackoff and RestartMaxBackoff bound the mpv restart loop.
	RestartMinBackoff Duration `toml:"restart_min_backoff"`
	RestartMaxBackoff Duration `toml:"restart_max_backoff"`
	// StartupTimeout is how long to wait for mpv's IPC socket to appear.
	StartupTimeout Duration `toml:"startup_timeout"`
	// CommandTimeout bounds a single IPC request.
	CommandTimeout Duration `toml:"command_timeout"`
}

// Overlay configures the channel-change banner drawn over the video.
type Overlay struct {
	// Enabled turns the banner on.
	Enabled bool `toml:"channel_overlay_enabled"`
	// Duration is how long the banner stays on screen once the picture has
	// actually arrived.
	Duration Duration `toml:"channel_overlay_duration"`
	// MaxHold caps how long the banner may wait for that picture. Tuning a
	// channel takes a second or two warm and considerably longer when ErsatzTV
	// has to cold-start it, so the banner holds until playback begins rather
	// than expiring into a blank screen — but not forever, or a stream that
	// never starts would pin it there.
	MaxHold Duration `toml:"channel_overlay_max_hold"`
	// TextFormat is a Go format string receiving the channel number, then the
	// channel name. "CH %s" produces "CH 3".
	TextFormat string `toml:"channel_overlay_text_format"`
	// Color is a hex RGB colour. The default is a bright retro green.
	Color string `toml:"channel_overlay_color"`
	// FontSize is the glyph height in overlay units (the overlay canvas is
	// 1280x720 regardless of the TV's real resolution, so this is resolution
	// independent).
	FontSize int `toml:"channel_overlay_font_size"`
	// Position is one of top-left, top-right, bottom-left, bottom-right, center.
	Position string `toml:"channel_overlay_position"`
	// MarginX and MarginY inset the banner from the screen edge, which matters on
	// televisions that overscan.
	MarginX int `toml:"channel_overlay_margin_x"`
	MarginY int `toml:"channel_overlay_margin_y"`
	// Outline is the border thickness that keeps the text readable over bright
	// video.
	Outline int `toml:"channel_overlay_outline"`
	// Tuning turns on the full-screen card shown while a channel is coming up.
	//
	// ErsatzTV has to cold-start a transcode for any channel it is not already
	// streaming — measured at around nine seconds on a Pi 5, nearly all of it
	// spent encoding the first HLS segment. Meanwhile mpv holds the outgoing
	// channel's last frame on screen until the new one decodes, so the wait
	// looks exactly like a picture that has frozen; when there is no frame to
	// hold, as after a stream dies, it is black instead. Neither says anything
	// is happening, which is what the card is for.
	Tuning bool `toml:"channel_overlay_tuning_enabled"`
	// TuningText is the word shown beneath the channel number while waiting.
	TuningText string `toml:"channel_overlay_tuning_text"`
	// TuningInterval is how often the animated dots advance.
	TuningInterval Duration `toml:"channel_overlay_tuning_interval"`
	// TuningFontSize is the glyph height of the tuning line. It defaults to half
	// the banner font size, which keeps the channel number dominant.
	TuningFontSize int `toml:"channel_overlay_tuning_font_size"`
	// TuningBackground is the colour painted over the whole screen behind the
	// tuning card.
	//
	// mpv holds the outgoing channel's last frame until the new one decodes, so
	// without this the card sits on a frozen picture — which reads as a crash.
	// Painting over it reads as having left the channel, which is what actually
	// happened. Empty leaves the frozen frame showing.
	TuningBackground string `toml:"channel_overlay_tuning_background"`
}

// Web configures the companion app's HTTP server.
type Web struct {
	// ListenAddress is host:port. Port 80 requires a capability, which the systemd
	// unit grants; the default of 8080 works unprivileged and is what setup.sh
	// redirects port 80 to when the capability is unavailable.
	ListenAddress string `toml:"listen_address"`
	// ReadTimeout and WriteTimeout bound HTTP requests.
	ReadTimeout  Duration `toml:"read_timeout"`
	WriteTimeout Duration `toml:"write_timeout"`
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
	// StaticDir serves the PWA from disk instead of the binary's embedded copy.
	// Used during development; empty means "use the embedded assets".
	StaticDir string `toml:"static_dir"`
	// WebSocketPingInterval keeps idle connections alive through the phone's
	// power management.
	WebSocketPingInterval Duration `toml:"websocket_ping_interval"`
}

// WiFi configures the setup-mode helper.
type WiFi struct {
	// HelperSocket is the unix socket the privileged helper listens on. The main
	// daemon never runs nmcli itself.
	HelperSocket string `toml:"helper_socket"`
	// Interface is the wireless interface, usually wlan0.
	Interface string `toml:"interface"`
	// SetupSSID is the access point name shown during setup.
	SetupSSID string `toml:"setup_ssid"`
	// SetupPassphrase secures the setup access point. At least 8 characters, or
	// empty for an open network. An open network is the friendlier default for a
	// device whose whole point is being easy to set up, and it is only live while
	// the user is standing in front of it holding a button.
	SetupPassphrase string `toml:"setup_passphrase"`
	// SetupAddress is the Pi's address on the setup network.
	SetupAddress string `toml:"setup_address"`
	// SetupTimeout automatically leaves setup mode after this long, so a forgotten
	// access point does not stay up indefinitely.
	SetupTimeout Duration `toml:"setup_timeout"`
	// ConnectTimeout bounds one attempt to join a chosen network.
	ConnectTimeout Duration `toml:"connect_timeout"`
	// ValidateTimeout is how long to wait for the new network to prove itself
	// (carrier plus an IP address) before accepting it. Until it does, the old
	// known-good credentials are kept.
	ValidateTimeout Duration `toml:"validate_timeout"`
	// ScanTimeout bounds a network scan.
	ScanTimeout Duration `toml:"scan_timeout"`
}

// Default returns a complete, valid configuration.
func Default() Config {
	return Config{
		General: General{
			Hostname:             "timeblaster",
			Timezone:             "",
			Clock24h:             false,
			DisplayOn:            true,
			RestoreChannelOnBoot: false,
			ChannelBanner:        Dur(2 * time.Second),
		},
		Logging: Logging{
			Level:  "info",
			Format: "text",
		},
		Storage: Storage{
			DatabasePath:   "/var/lib/timeblaster/timeblaster.db",
			AlarmSoundsDir: "/var/lib/timeblaster/alarm-sounds",
			RuntimeDir:     "/run/timeblaster",
		},
		Serial: Serial{
			Device: "",
			DeviceGlobs: []string{
				"/dev/timeblaster-nano",
				"/dev/serial/by-id/*Arduino*",
				"/dev/serial/by-id/*",
			},
			BaudRate:            115200,
			ReconnectMinBackoff: Dur(500 * time.Millisecond),
			ReconnectMaxBackoff: Dur(15 * time.Second),
			ReadTimeout:         Dur(2 * time.Second),
			WaitForTimeSync:     true,
			TimeSyncInterval:    Dur(60 * time.Second),
			HeartbeatTimeout:    Dur(8 * time.Second),
			WriteQueueSize:      64,
		},
		Input: Input{
			ADCMin:                0,
			ADCMax:                4095,
			EndMarginPercent:      2,
			FilterAlpha:           0.35,
			FilterSnapThreshold:   0.08,
			ChannelHysteresis:     0.25,
			VolumeDeadbandPercent: 2,
			WiFiHoldDuration:      Dur(5 * time.Second),
			MinPressDuration:      Dur(30 * time.Millisecond),
			HoldPollInterval:      Dur(100 * time.Millisecond),
		},
		Audio: Audio{
			AlarmDevice:          "",
			MixerControl:         "",
			StartupVolumePercent: 40,
			AllowSoftwareVolume:  false,
			PlayerBinary:         "mpv",
			// An empty slice rather than nil, so the shipped configuration file's
			// `extra_player_args = []` round-trips to exactly the default.
			ExtraPlayerArgs:       []string{},
			DefaultSoundID:        "alarm1",
			DeviceRecheckInterval: Dur(30 * time.Second),
			TestDuration:          Dur(10 * time.Second),
		},
		ErsatzTV: ErsatzTV{
			BaseURL:         "http://127.0.0.1:8409",
			RequestTimeout:  Dur(5 * time.Second),
			RefreshInterval: Dur(60 * time.Second),
			RetryMinBackoff: Dur(2 * time.Second),
			RetryMaxBackoff: Dur(60 * time.Second),
			StreamMode:      "mixed",
			StreamFormat:    "m3u8",
		},
		MPV: MPV{
			Binary:    "mpv",
			IPCSocket: "/run/timeblaster/mpv-tv.sock",
			Args: []string{
				// Output straight to the kernel modesetting driver: no X11, no
				// Wayland, no desktop session on Raspberry Pi OS Lite.
				"--vo=gpu",
				"--gpu-context=drm",
				"--gpu-api=opengl",
				"--hwdec=auto-safe",
				"--fullscreen",
				// Keep mpv alive with nothing loaded so channel changes are a
				// loadfile over IPC rather than a process restart.
				"--idle=yes",
				"--force-window=yes",
				"--keep-open=no",
				// Without this an image is shown for image-display-duration
				// (5s by default) and then ends, leaving mpv idle and the
				// television black. The standby screen has to stay up for as
				// long as no channel is selected, which may be forever.
				// Video is unaffected: the option applies only to images.
				"--image-display-duration=inf",
				"--no-input-default-bindings",
				"--no-osc",
				"--no-terminal",
				"--msg-level=all=warn",
				// Live streams: prefer staying near the live edge over buffering.
				"--cache=yes",
				"--demuxer-max-bytes=32MiB",
				"--audio-device=auto",
			},
			NoChannelImage:    "/usr/share/timeblaster/assets/no-channel.png",
			BootingImage:      "/usr/share/timeblaster/assets/booting.png",
			BootingTimeout:    Dur(90 * time.Second),
			RestartMinBackoff: Dur(time.Second),
			RestartMaxBackoff: Dur(30 * time.Second),
			StartupTimeout:    Dur(15 * time.Second),
			CommandTimeout:    Dur(5 * time.Second),
		},
		Overlay: Overlay{
			Enabled:    true,
			Duration:   Dur(2500 * time.Millisecond),
			MaxHold:    Dur(20 * time.Second),
			TextFormat: "CH %s",
			Color:      "#33FF33",
			FontSize:   96,
			Position:   "top-left",
			MarginX:    64,
			MarginY:    48,
			Outline:    3,

			Tuning:           true,
			TuningText:       "TUNING",
			TuningInterval:   Dur(400 * time.Millisecond),
			TuningFontSize:   44,
			TuningBackground: "#000000",
		},
		Web: Web{
			ListenAddress:         ":8080",
			ReadTimeout:           Dur(15 * time.Second),
			WriteTimeout:          Dur(30 * time.Second),
			ShutdownTimeout:       Dur(10 * time.Second),
			WebSocketPingInterval: Dur(20 * time.Second),
		},
		WiFi: WiFi{
			HelperSocket:    "/run/timeblaster/wifi.sock",
			Interface:       "wlan0",
			SetupSSID:       "TIMEBLASTER-SETUP",
			SetupPassphrase: "",
			SetupAddress:    "10.42.0.1/24",
			SetupTimeout:    Dur(15 * time.Minute),
			ConnectTimeout:  Dur(45 * time.Second),
			ValidateTimeout: Dur(30 * time.Second),
			ScanTimeout:     Dur(20 * time.Second),
		},
	}
}

// Load reads a configuration file, layering it over the defaults.
//
// A missing file is not an error: a freshly imaged Pi that has not run setup.sh
// still boots into a working alarm clock. Anything else — unreadable, unparsable,
// or containing keys we do not recognise — is reported, because silent misconfig
// on an appliance is how you find out six months later that your alarm never
// used the speaker you configured.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("config: reading %s: %w", path, err)
	}

	md, err := toml.Decode(string(b), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return cfg, fmt.Errorf("config: %s contains unknown keys: %s", path, strings.Join(keys, ", "))
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks every field that a consumer would otherwise have to re-check.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	// general
	if c.General.Hostname == "" {
		add("general.hostname must not be empty")
	}
	if c.General.Timezone != "" {
		if _, err := time.LoadLocation(c.General.Timezone); err != nil {
			add("general.timezone %q is not a known IANA timezone", c.General.Timezone)
		}
	}

	// logging
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		add("logging.level must be debug, info, warn or error, got %q", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "text", "json":
	default:
		add("logging.format must be text or json, got %q", c.Logging.Format)
	}

	// storage
	if c.Storage.DatabasePath == "" {
		add("storage.database_path must not be empty")
	}
	if c.Storage.AlarmSoundsDir == "" {
		add("storage.alarm_sounds_dir must not be empty")
	}

	// serial
	if c.Serial.Device == "" && len(c.Serial.DeviceGlobs) == 0 {
		add("serial: set either device or device_globs")
	}
	if c.Serial.BaudRate <= 0 {
		add("serial.baud_rate must be positive, got %d", c.Serial.BaudRate)
	}
	if c.Serial.ReconnectMinBackoff.Duration <= 0 {
		add("serial.reconnect_min_backoff must be positive")
	}
	if c.Serial.ReconnectMaxBackoff.Duration < c.Serial.ReconnectMinBackoff.Duration {
		add("serial.reconnect_max_backoff must be at least reconnect_min_backoff")
	}
	if c.Serial.TimeSyncInterval.Duration <= 0 {
		add("serial.time_sync_interval must be positive")
	}
	if c.Serial.HeartbeatTimeout.Duration <= 0 {
		add("serial.heartbeat_timeout must be positive")
	}
	if c.Serial.WriteQueueSize <= 0 {
		add("serial.write_queue_size must be positive, got %d", c.Serial.WriteQueueSize)
	}

	// input
	if c.Input.ADCMax <= c.Input.ADCMin {
		add("input.adc_max (%d) must exceed input.adc_min (%d)", c.Input.ADCMax, c.Input.ADCMin)
	}
	if c.Input.EndMarginPercent < 0 || c.Input.EndMarginPercent >= 50 {
		add("input.end_margin_percent must be in [0,50), got %v", c.Input.EndMarginPercent)
	}
	if c.Input.FilterAlpha <= 0 || c.Input.FilterAlpha > 1 {
		add("input.filter_alpha must be in (0,1], got %v", c.Input.FilterAlpha)
	}
	if c.Input.FilterSnapThreshold < 0 || c.Input.FilterSnapThreshold > 1 {
		add("input.filter_snap_threshold must be in [0,1], got %v", c.Input.FilterSnapThreshold)
	}
	if c.Input.ChannelHysteresis < 0 || c.Input.ChannelHysteresis >= 0.5 {
		add("input.channel_hysteresis must be in [0,0.5), got %v", c.Input.ChannelHysteresis)
	}
	if c.Input.VolumeDeadbandPercent < 0 || c.Input.VolumeDeadbandPercent > 50 {
		add("input.volume_deadband_percent must be 0-50, got %d", c.Input.VolumeDeadbandPercent)
	}
	if c.Input.WiFiHoldDuration.Duration <= 0 {
		add("input.wifi_hold_duration must be positive")
	}
	if c.Input.HoldPollInterval.Duration <= 0 {
		add("input.hold_poll_interval must be positive")
	}

	// audio
	if c.Audio.StartupVolumePercent < 0 || c.Audio.StartupVolumePercent > 100 {
		add("audio.startup_volume_percent must be 0-100, got %d", c.Audio.StartupVolumePercent)
	}
	if c.Audio.PlayerBinary == "" {
		add("audio.player_binary must not be empty")
	}
	if c.Audio.TestDuration.Duration <= 0 {
		add("audio.test_duration must be positive")
	}

	// ersatztv
	if c.ErsatzTV.BaseURL == "" {
		add("ersatztv.base_url must not be empty")
	} else if !strings.HasPrefix(c.ErsatzTV.BaseURL, "http://") && !strings.HasPrefix(c.ErsatzTV.BaseURL, "https://") {
		add("ersatztv.base_url must start with http:// or https://, got %q", c.ErsatzTV.BaseURL)
	}
	if c.ErsatzTV.RequestTimeout.Duration <= 0 {
		add("ersatztv.request_timeout must be positive")
	}
	if c.ErsatzTV.RefreshInterval.Duration <= 0 {
		add("ersatztv.refresh_interval must be positive")
	}
	switch c.ErsatzTV.StreamFormat {
	case "m3u8", "ts":
	default:
		add("ersatztv.stream_format must be m3u8 or ts, got %q", c.ErsatzTV.StreamFormat)
	}

	// mpv
	if c.MPV.Binary == "" {
		add("mpv.binary must not be empty")
	}
	if c.MPV.IPCSocket == "" {
		add("mpv.ipc_socket must not be empty")
	}
	if c.MPV.CommandTimeout.Duration <= 0 {
		add("mpv.command_timeout must be positive")
	}
	if c.MPV.StartupTimeout.Duration <= 0 {
		add("mpv.startup_timeout must be positive")
	}

	// overlay
	if c.Overlay.Enabled {
		if c.Overlay.Duration.Duration <= 0 {
			add("overlay.channel_overlay_duration must be positive when the overlay is enabled")
		}
		if c.Overlay.MaxHold.Duration < c.Overlay.Duration.Duration {
			add("overlay.channel_overlay_max_hold must be at least channel_overlay_duration")
		}
		if c.Overlay.FontSize <= 0 {
			add("overlay.channel_overlay_font_size must be positive, got %d", c.Overlay.FontSize)
		}
		if !strings.Contains(c.Overlay.TextFormat, "%") {
			add("overlay.channel_overlay_text_format must contain a %% verb for the channel number, got %q", c.Overlay.TextFormat)
		}
		if _, _, _, err := ParseHexColor(c.Overlay.Color); err != nil {
			add("overlay.channel_overlay_color: %v", err)
		}
		if c.Overlay.Tuning {
			if c.Overlay.TuningInterval.Duration <= 0 {
				add("overlay.channel_overlay_tuning_interval must be positive when the tuning card is enabled, got %s", c.Overlay.TuningInterval.Duration)
			}
			if c.Overlay.TuningFontSize < 0 {
				add("overlay.channel_overlay_tuning_font_size cannot be negative, got %d", c.Overlay.TuningFontSize)
			}
			if c.Overlay.TuningBackground != "" {
				if _, _, _, err := ParseHexColor(c.Overlay.TuningBackground); err != nil {
					add("overlay.channel_overlay_tuning_background: %v", err)
				}
			}
		}
		switch c.Overlay.Position {
		case "top-left", "top-right", "bottom-left", "bottom-right", "center":
		default:
			add("overlay.channel_overlay_position must be top-left, top-right, bottom-left, bottom-right or center, got %q", c.Overlay.Position)
		}
	}

	// web
	if c.Web.ListenAddress == "" {
		add("web.listen_address must not be empty")
	}

	// wifi
	if c.WiFi.HelperSocket == "" {
		add("wifi.helper_socket must not be empty")
	}
	if c.WiFi.Interface == "" {
		add("wifi.interface must not be empty")
	}
	if c.WiFi.SetupSSID == "" || len(c.WiFi.SetupSSID) > 32 {
		add("wifi.setup_ssid must be 1-32 characters, got %d", len(c.WiFi.SetupSSID))
	}
	if n := len(c.WiFi.SetupPassphrase); n != 0 && (n < 8 || n > 63) {
		add("wifi.setup_passphrase must be empty (open network) or 8-63 characters, got %d", n)
	}
	if c.WiFi.ConnectTimeout.Duration <= 0 {
		add("wifi.connect_timeout must be positive")
	}

	return errors.Join(errs...)
}

// SoundPath resolves a sound identifier to a path inside the alarm sounds
// directory, rejecting anything that would escape it. Sound ids come from the
// HTTP API, so this is a security boundary, not a convenience.
func (c Config) SoundPath(id string) (string, error) {
	if id == "" {
		return "", errors.New("config: empty sound id")
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("config: sound id %q must be a bare file name", id)
	}
	if !strings.HasSuffix(strings.ToLower(id), ".mp3") {
		id += ".mp3"
	}
	full := filepath.Join(c.Storage.AlarmSoundsDir, id)
	// Belt and braces: confirm the joined path really is inside the directory.
	base, err := filepath.Abs(c.Storage.AlarmSoundsDir)
	if err != nil {
		return "", fmt.Errorf("config: resolving sounds directory: %w", err)
	}
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("config: resolving sound path: %w", err)
	}
	if !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", fmt.Errorf("config: sound id %q resolves outside the sounds directory", id)
	}
	return full, nil
}

// ParseHexColor parses "#RRGGBB" or "RRGGBB" into its components.
func ParseHexColor(s string) (r, g, b uint8, err error) {
	v := strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(v) != 6 {
		return 0, 0, 0, fmt.Errorf("%q is not a #RRGGBB colour", s)
	}
	var n uint64
	for _, c := range v {
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint64(c-'A') + 10
		default:
			return 0, 0, 0, fmt.Errorf("%q is not a #RRGGBB colour", s)
		}
		n = n<<4 | d
	}
	return uint8(n >> 16), uint8(n >> 8), uint8(n), nil
}
