package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("the default configuration must be valid: %v", err)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
	if cfg.Web.ListenAddress != Default().Web.ListenAddress {
		t.Errorf("defaults not applied: %+v", cfg.Web)
	}
}

func TestLoadEmptyPathUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil || cfg.General.Hostname != "timeblaster" {
		t.Fatalf("got %+v, %v", cfg.General, err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "timeblaster.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadLayersOverDefaults(t *testing.T) {
	path := writeConfig(t, `
[general]
hostname = "bedroom-blaster"
display_brightness = 30

[serial]
time_sync_interval = "2m"

[audio]
alarm_device = "hw:CARD=Device,DEV=0"
startup_volume_percent = 55

[overlay]
channel_overlay_text_format = "CHANNEL %s"
channel_overlay_duration = "4s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.General.Hostname != "bedroom-blaster" || cfg.General.DisplayBrightness != 30 {
		t.Errorf("general: %+v", cfg.General)
	}
	if cfg.Serial.TimeSyncInterval.Duration != 2*time.Minute {
		t.Errorf("duration parsing: %v", cfg.Serial.TimeSyncInterval)
	}
	if cfg.Audio.StartupVolumePercent != 55 {
		t.Errorf("audio: %+v", cfg.Audio)
	}
	if cfg.Overlay.Duration.Duration != 4*time.Second {
		t.Errorf("overlay duration: %v", cfg.Overlay.Duration)
	}
	// Untouched sections keep their defaults.
	if cfg.MPV.Binary != "mpv" || len(cfg.MPV.Args) == 0 {
		t.Errorf("mpv defaults lost: %+v", cfg.MPV)
	}
	if cfg.ErsatzTV.BaseURL != "http://127.0.0.1:8409" {
		t.Errorf("ersatztv default lost: %q", cfg.ErsatzTV.BaseURL)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	// A typo in an appliance's config file must be loud, not silent.
	path := writeConfig(t, "[general]\nhostnmae = \"typo\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "hostnmae") {
		t.Errorf("error should name the offending key: %v", err)
	}
}

func TestLoadRejectsMalformedTOML(t *testing.T) {
	path := writeConfig(t, "[general\nhostname = \"x\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	path := writeConfig(t, "[serial]\ntime_sync_interval = \"soon\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected a duration error")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	path := writeConfig(t, "[general]\ndisplay_brightness = 250\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(err.Error(), "display_brightness") {
		t.Errorf("error should name the field: %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	// Fixing config on an appliance one error at a time is miserable, so Validate
	// joins every failure.
	cfg := Default()
	cfg.General.Hostname = ""
	cfg.Logging.Level = "loud"
	cfg.Input.FilterAlpha = 3
	cfg.WiFi.SetupPassphrase = "short"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"hostname", "logging.level", "filter_alpha", "setup_passphrase"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
}

func TestValidateFieldRanges(t *testing.T) {
	tests := []struct {
		name  string
		mut   func(*Config)
		field string
	}{
		{"bad timezone", func(c *Config) { c.General.Timezone = "Mars/Olympus" }, "timezone"},
		{"empty database path", func(c *Config) { c.Storage.DatabasePath = "" }, "database_path"},
		{"no serial device", func(c *Config) { c.Serial.Device = ""; c.Serial.DeviceGlobs = nil }, "serial"},
		{"backoff inverted", func(c *Config) { c.Serial.ReconnectMaxBackoff = Dur(time.Millisecond) }, "reconnect_max_backoff"},
		{"adc range inverted", func(c *Config) { c.Input.ADCMax = 0 }, "adc_max"},
		{"hysteresis too large", func(c *Config) { c.Input.ChannelHysteresis = 0.9 }, "channel_hysteresis"},
		{"volume out of range", func(c *Config) { c.Audio.StartupVolumePercent = 101 }, "startup_volume_percent"},
		{"base url without scheme", func(c *Config) { c.ErsatzTV.BaseURL = "127.0.0.1:8409" }, "base_url"},
		{"bad stream format", func(c *Config) { c.ErsatzTV.StreamFormat = "rtsp" }, "stream_format"},
		{"empty mpv socket", func(c *Config) { c.MPV.IPCSocket = "" }, "ipc_socket"},
		{"overlay format without verb", func(c *Config) { c.Overlay.TextFormat = "CHANNEL" }, "text_format"},
		{"overlay bad colour", func(c *Config) { c.Overlay.Color = "green" }, "color"},
		{"overlay bad position", func(c *Config) { c.Overlay.Position = "middle-ish" }, "position"},
		{"ssid too long", func(c *Config) { c.WiFi.SetupSSID = strings.Repeat("x", 33) }, "setup_ssid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s should be invalid", tc.name)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error should mention %q: %v", tc.field, err)
			}
		})
	}
}

func TestOverlayValidationSkippedWhenDisabled(t *testing.T) {
	cfg := Default()
	cfg.Overlay.Enabled = false
	cfg.Overlay.Color = "not a colour"
	cfg.Overlay.FontSize = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled overlay should not be validated: %v", err)
	}
}

func TestSoundPathRejectsTraversal(t *testing.T) {
	cfg := Default()
	cfg.Storage.AlarmSoundsDir = "/var/lib/timeblaster/alarm-sounds"

	got, err := cfg.SoundPath("alarm1")
	if err != nil {
		t.Fatalf("SoundPath: %v", err)
	}
	if got != "/var/lib/timeblaster/alarm-sounds/alarm1.mp3" {
		t.Errorf("got %q", got)
	}
	// An explicit extension is respected rather than doubled.
	if got, _ := cfg.SoundPath("alarm2.mp3"); got != "/var/lib/timeblaster/alarm-sounds/alarm2.mp3" {
		t.Errorf("got %q", got)
	}

	// Sound ids arrive over HTTP, so traversal must be impossible.
	for _, bad := range []string{"", "../../etc/shadow", "/etc/passwd", "sub/dir.mp3", `..\windows`} {
		if _, err := cfg.SoundPath(bad); err == nil {
			t.Errorf("SoundPath(%q) should have been rejected", bad)
		}
	}
}

func TestParseHexColor(t *testing.T) {
	for _, tc := range []struct {
		in      string
		r, g, b uint8
	}{
		{"#33FF33", 0x33, 0xFF, 0x33},
		{"33ff33", 0x33, 0xFF, 0x33},
		{"  #000000 ", 0, 0, 0},
		{"#FFFFFF", 255, 255, 255},
	} {
		r, g, b, err := ParseHexColor(tc.in)
		if err != nil {
			t.Fatalf("ParseHexColor(%q): %v", tc.in, err)
		}
		if r != tc.r || g != tc.g || b != tc.b {
			t.Errorf("ParseHexColor(%q) = %d,%d,%d", tc.in, r, g, b)
		}
	}
	for _, bad := range []string{"", "#FFF", "green", "#GGGGGG", "#1234567"} {
		if _, _, _, err := ParseHexColor(bad); err == nil {
			t.Errorf("ParseHexColor(%q) should fail", bad)
		}
	}
}

func TestDurationRoundTrip(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("2m30s")); err != nil {
		t.Fatal(err)
	}
	if d.Duration != 150*time.Second {
		t.Fatalf("got %v", d.Duration)
	}
	b, err := d.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "2m30s" {
		t.Errorf("got %q", b)
	}
}

// TestShippedConfigMatchesDefaults guards the installed configuration file
// against drift. Every value in deploy/config/timeblaster.toml is documented as
// being the built-in default, so a commented-out line and a deleted line behave
// identically — and that promise is only true if the file really does match.
//
// It also proves the file parses with no unknown keys, which catches a renamed
// field before it ships rather than on first boot.
func TestShippedConfigMatchesDefaults(t *testing.T) {
	const path = "../../deploy/config/timeblaster.toml"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shipped config not present: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped configuration does not load: %v", err)
	}
	want := Default()

	// Compare section by section so a failure names the offending block.
	checks := []struct {
		name      string
		got, want any
	}{
		{"general", got.General, want.General},
		{"logging", got.Logging, want.Logging},
		{"storage", got.Storage, want.Storage},
		{"serial", got.Serial, want.Serial},
		{"input", got.Input, want.Input},
		{"audio", got.Audio, want.Audio},
		{"ersatztv", got.ErsatzTV, want.ErsatzTV},
		{"mpv", got.MPV, want.MPV},
		{"overlay", got.Overlay, want.Overlay},
		{"web", got.Web, want.Web},
		{"wifi", got.WiFi, want.WiFi},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("[%s] in the shipped config differs from the built-in default:\n got  %+v\n want %+v",
				c.name, c.got, c.want)
		}
	}
}
