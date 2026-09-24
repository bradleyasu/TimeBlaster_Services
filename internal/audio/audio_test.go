package audio

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeMP3 creates a file that passes the library's playability checks.
func writeMP3(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), make([]byte, 2048)...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLibraryRefreshFindsSounds(t *testing.T) {
	dir := t.TempDir()
	writeMP3(t, dir, "alarm1.mp3")
	writeMP3(t, dir, "alarm2.mp3")
	writeMP3(t, dir, "klaxon.MP3")

	// Things that must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden.mp3"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "truncated.mp3"), []byte("ID3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir.mp3"), 0o755); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(dir)
	if err := lib.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	got := lib.Sounds()
	if len(got) != 3 {
		t.Fatalf("got %d sounds: %+v", len(got), ids(got))
	}
	want := []string{"alarm1", "alarm2", "klaxon"}
	for i, w := range want {
		if got[i].ID != w {
			t.Fatalf("ids: got %v want %v", ids(got), want)
		}
	}
	if got[0].Name != "Alarm 1" {
		t.Errorf("display name: %q", got[0].Name)
	}
	if got[0].SizeBytes == 0 {
		t.Error("size not recorded")
	}
}

func TestLibraryRefreshOnMissingDirectory(t *testing.T) {
	lib := NewLibrary(filepath.Join(t.TempDir(), "absent"))
	if err := lib.Refresh(); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
	if len(lib.Sounds()) != 0 {
		t.Error("no sounds should have been loaded")
	}
}

func TestLibraryRefreshKeepsExistingListOnFailure(t *testing.T) {
	dir := t.TempDir()
	writeMP3(t, dir, "alarm1.mp3")
	lib := NewLibrary(dir)
	if err := lib.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// An SD-card hiccup must not erase the user's sound list from the daemon.
	if err := lib.Refresh(); err == nil {
		t.Fatal("expected an error")
	}
	if len(lib.Sounds()) != 1 {
		t.Errorf("previous list lost: %+v", lib.Sounds())
	}
}

func TestLibraryGet(t *testing.T) {
	dir := t.TempDir()
	writeMP3(t, dir, "alarm1.mp3")
	lib := NewLibrary(dir)
	if err := lib.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Get("alarm1"); err != nil {
		t.Errorf("Get: %v", err)
	}
	if _, err := lib.Get("nope"); !errors.Is(err, ErrSoundNotFound) {
		t.Errorf("Get missing: %v", err)
	}
}

func TestValidateRejectsCorruptFiles(t *testing.T) {
	dir := t.TempDir()

	good := Sound{ID: "good", Path: writeMP3(t, dir, "good.mp3")}
	if err := Validate(good); err != nil {
		t.Errorf("a valid file was rejected: %v", err)
	}

	missing := Sound{ID: "missing", Path: filepath.Join(dir, "gone.mp3")}
	if err := Validate(missing); !errors.Is(err, ErrUnplayable) {
		t.Errorf("missing file: %v", err)
	}

	tinyPath := filepath.Join(dir, "tiny.mp3")
	if err := os.WriteFile(tinyPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Sound{ID: "tiny", Path: tinyPath}); !errors.Is(err, ErrUnplayable) {
		t.Errorf("truncated file: %v", err)
	}

	// An HTML error page saved as .wav is the realistic "corrupt file" case.
	htmlPath := filepath.Join(dir, "broken.wav")
	if err := os.WriteFile(htmlPath, append([]byte("<!DOCTYPE html>"), make([]byte, 4096)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Sound{ID: "broken", Path: htmlPath}); !errors.Is(err, ErrUnplayable) {
		t.Errorf("non-audio content: %v", err)
	}

	if err := Validate(Sound{ID: "nopath"}); !errors.Is(err, ErrUnplayable) {
		t.Errorf("empty path: %v", err)
	}
}

func TestLooksLikeAudioMagicNumbers(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		ext  string
		want bool
	}{
		{"id3", []byte("ID3\x03\x00\x00\x00\x00\x00\x00\x00\x00"), ".mp3", true},
		{"mpeg sync", []byte{0xFF, 0xFB, 0x90, 0x00, 0, 0, 0, 0, 0, 0, 0, 0}, ".mp3", true},
		{"riff wav", []byte("RIFF\x00\x00\x00\x00WAVE"), ".wav", true},
		{"ogg", []byte("OggS\x00\x00\x00\x00\x00\x00\x00\x00"), ".ogg", true},
		{"flac", []byte("fLaC\x00\x00\x00\x00\x00\x00\x00\x00"), ".flac", true},
		{"m4a", []byte("\x00\x00\x00\x20ftypM4A "), ".m4a", true},
		{"html as wav", []byte("<!DOCTYPE html>x"), ".wav", false},
		{"html as mp3 trusts extension", []byte("<!DOCTYPE html>x"), ".mp3", true},
		{"too short", []byte{1, 2}, ".mp3", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeAudio(tc.head, tc.ext); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestLibraryResolveFallbackChain(t *testing.T) {
	dir := t.TempDir()
	writeMP3(t, dir, "alarm1.mp3")
	writeMP3(t, dir, "alarm2.mp3")
	lib := NewLibrary(dir)
	if err := lib.Refresh(); err != nil {
		t.Fatal(err)
	}

	// 1. The requested sound.
	s, reason, err := lib.Resolve("alarm2", "alarm1")
	if err != nil || s.ID != "alarm2" || reason != "" {
		t.Fatalf("requested: %+v, %q, %v", s, reason, err)
	}

	// 2. The configured default when the request is missing.
	s, reason, err = lib.Resolve("deleted", "alarm1")
	if err != nil || s.ID != "alarm1" {
		t.Fatalf("default: %+v, %v", s, err)
	}
	if !strings.Contains(reason, "deleted") {
		t.Errorf("reason should name the missing sound: %q", reason)
	}

	// 3. Any other playable sound when both are missing.
	s, reason, err = lib.Resolve("deleted", "also-gone")
	if err != nil || (s.ID != "alarm1" && s.ID != "alarm2") {
		t.Fatalf("any sound: %+v, %v", s, err)
	}
	if reason == "" {
		t.Error("substitution should be explained")
	}

	// 4. The built-in tone when the library is empty.
	empty := NewLibrary(t.TempDir())
	if err := empty.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := empty.Resolve("anything", "alarm1"); !errors.Is(err, ErrNoSounds) {
		t.Fatalf("empty library without a fallback: %v", err)
	}

	tone, err := WriteFallbackTone(filepath.Join(t.TempDir(), "fallback.wav"))
	if err != nil {
		t.Fatal(err)
	}
	empty.SetFallback(tone)
	s, reason, err = empty.Resolve("anything", "alarm1")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if !s.Fallback || s.ID != FallbackToneID {
		t.Errorf("expected the built-in tone: %+v", s)
	}
	if reason == "" {
		t.Error("falling back to the built-in tone should be explained")
	}
}

func TestWriteFallbackToneProducesAPlayableWAV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "fallback.wav")
	s, err := WriteFallbackTone(path)
	if err != nil {
		t.Fatalf("WriteFallbackTone: %v", err)
	}
	if err := Validate(s); err != nil {
		t.Fatalf("generated tone is not playable: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[36:40]) != "data" {
		t.Fatalf("not a canonical WAV: %q", b[:44])
	}
	// The declared sizes must match the actual file, or mpv will refuse it.
	riffSize := int(b[4]) | int(b[5])<<8 | int(b[6])<<16 | int(b[7])<<24
	if riffSize != len(b)-8 {
		t.Errorf("RIFF size %d does not match the file length %d", riffSize, len(b))
	}
	dataSize := int(b[40]) | int(b[41])<<8 | int(b[42])<<16 | int(b[43])<<24
	if dataSize != len(b)-44 {
		t.Errorf("data size %d does not match the payload length %d", dataSize, len(b)-44)
	}
	if !s.Fallback {
		t.Error("the generated tone should be marked as the fallback")
	}
}

func TestDisplayName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"alarm1", "Alarm 1"},
		{"alarm2", "Alarm 2"},
		{"klaxon", "Klaxon"},
		{"gentle-chime", "Gentle chime"},
		{"air_raid", "Air raid"},
	} {
		if got := displayName(tc.in); got != tc.want {
			t.Errorf("displayName(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildArgsTargetsTheConfiguredDevice(t *testing.T) {
	args := BuildArgs("/var/lib/timeblaster/alarm-sounds/alarm1.mp3", PlayOptions{
		Device:        "alsa/hw:CARD=Device,DEV=0",
		Loop:          true,
		VolumePercent: 65,
	}, "/run/timeblaster/alarm.sock")

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--no-video",
		"--ao=alsa",
		"--audio-device=alsa/hw:CARD=Device,DEV=0",
		"--loop-file=inf",
		"--volume=65",
		"--no-config",
		"--input-ipc-server=/run/timeblaster/alarm.sock",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in: %v", want, args)
		}
	}
	// The file must come last so no option can swallow it.
	if args[len(args)-1] != "/var/lib/timeblaster/alarm-sounds/alarm1.mp3" {
		t.Errorf("file is not the final argument: %v", args)
	}
	// The alarm player must never touch video output.
	for _, forbidden := range []string{"--vo=", "--gpu-context", "--fullscreen"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("alarm player must not configure video output: %v", args)
		}
	}
}

func TestBuildArgsClampsVolumeAndOmitsOptionalParts(t *testing.T) {
	args := BuildArgs("/x.mp3", PlayOptions{VolumePercent: 500}, "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--volume=100") {
		t.Errorf("volume not clamped: %v", args)
	}
	if strings.Contains(joined, "--input-ipc-server") {
		t.Errorf("no socket was requested: %v", args)
	}
	if strings.Contains(joined, "--loop-file") {
		t.Errorf("loop was not requested: %v", args)
	}
	if strings.Contains(joined, "--audio-device") {
		t.Errorf("no device was requested: %v", args)
	}
}

func TestBuildArgsAppendsExtraArgs(t *testing.T) {
	args := BuildArgs("/x.mp3", PlayOptions{ExtraArgs: []string{"--af=loudnorm"}}, "")
	if !strings.Contains(strings.Join(args, " "), "--af=loudnorm") {
		t.Errorf("extra args dropped: %v", args)
	}
}

// --- service tests -----------------------------------------------------------

type serviceFixture struct {
	svc    *AlarmService
	player *FakePlayer
	runner *system.FakeRunner
	clock  *system.FakeClock
	dir    string
}

func newServiceFixture(t *testing.T, mutate func(*config.Audio)) *serviceFixture {
	t.Helper()
	dir := t.TempDir()
	writeMP3(t, dir, "alarm1.mp3")
	writeMP3(t, dir, "alarm2.mp3")

	lib := NewLibrary(dir)
	if err := lib.Refresh(); err != nil {
		t.Fatal(err)
	}
	tone, err := WriteFallbackTone(filepath.Join(t.TempDir(), "fallback.wav"))
	if err != nil {
		t.Fatal(err)
	}
	lib.SetFallback(tone)

	cardsPath := filepath.Join(t.TempDir(), "cards")
	if err := os.WriteFile(cardsPath, []byte(
		" 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0\n"+
			"                      vc4-hdmi-0\n"+
			" 1 [Device         ]: USB-Audio - USB Audio Device\n"+
			"                      Generic USB Audio Device\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := system.NewFakeRunner()
	runner.Outputs["amixer"] = []byte("Simple mixer control 'PCM',0\n")

	cfg := config.Default().Audio
	cfg.AlarmDevice = "USB"
	cfg.DefaultSoundID = "alarm1"
	if mutate != nil {
		mutate(&cfg)
	}

	player := NewFakePlayer()
	clk := system.NewFakeClock(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC))
	svc := NewService(cfg, Deps{
		Library: lib,
		Player:  player,
		Probe:   system.ALSAProbe{CardsPath: cardsPath, Runner: runner},
		Runner:  runner,
		Clock:   clk,
		Logger:  testLogger(),
	})
	return &serviceFixture{svc: svc, player: player, runner: runner, clock: clk, dir: dir}
}

func TestServiceResolvesTheUSBDevice(t *testing.T) {
	f := newServiceFixture(t, nil)
	if err := f.svc.ResolveDevice(context.Background()); err != nil {
		t.Fatalf("ResolveDevice: %v", err)
	}

	h := f.svc.Health()
	if !h.DeviceAvailable {
		t.Fatal("device should be available")
	}
	if h.Device != "alsa/hw:CARD=Device,DEV=0" {
		t.Errorf("device: %q", h.Device)
	}
	if h.MixerControl != "PCM" || !h.HardwareVolume {
		t.Errorf("mixer: %+v", h)
	}
}

func TestServiceReportsAMissingDevice(t *testing.T) {
	f := newServiceFixture(t, func(c *config.Audio) { c.AlarmDevice = "bluetooth-thing" })
	if err := f.svc.ResolveDevice(context.Background()); err == nil {
		t.Fatal("expected an error for an absent device")
	}
	h := f.svc.Health()
	if h.DeviceAvailable {
		t.Error("device should be unavailable")
	}
	if h.LastError == "" {
		t.Error("the failure should be surfaced in health")
	}
}

func TestServicePlayAlarmUsesTheAlarmDeviceAndLoops(t *testing.T) {
	f := newServiceFixture(t, nil)
	if err := f.svc.ResolveDevice(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.PlayAlarm(context.Background(), "alarm2"); err != nil {
		t.Fatalf("PlayAlarm: %v", err)
	}
	play, ok := f.player.LastPlay()
	if !ok {
		t.Fatal("nothing played")
	}
	if filepath.Base(play.Path) != "alarm2.mp3" {
		t.Errorf("path: %s", play.Path)
	}
	if play.Opts.Device != "alsa/hw:CARD=Device,DEV=0" {
		t.Errorf("an alarm must target the USB speaker, got %q", play.Opts.Device)
	}
	if !play.Opts.Loop {
		t.Error("an alarm must loop until dismissed")
	}
	if !f.svc.IsPlaying() {
		t.Error("IsPlaying should be true")
	}

	if err := f.svc.StopAlarm(); err != nil {
		t.Fatalf("StopAlarm: %v", err)
	}
	if f.svc.IsPlaying() {
		t.Error("IsPlaying should be false after stopping")
	}
	if !f.player.LastHandle().Stopped() {
		t.Error("the handle was not stopped")
	}
	// Stopping twice is harmless: the big red button gets pressed a lot.
	if err := f.svc.StopAlarm(); err != nil {
		t.Errorf("second StopAlarm: %v", err)
	}
}

func TestServiceFallsBackWhenTheSoundIsMissing(t *testing.T) {
	f := newServiceFixture(t, nil)
	if err := f.svc.PlayAlarm(context.Background(), "deleted-sound"); err != nil {
		t.Fatalf("a missing sound must still ring something: %v", err)
	}
	play, _ := f.player.LastPlay()
	if filepath.Base(play.Path) != "alarm1.mp3" {
		t.Errorf("expected the default sound, played %s", play.Path)
	}
}

func TestServiceUsesTheBuiltInToneWhenEverythingIsGone(t *testing.T) {
	f := newServiceFixture(t, nil)
	// Someone emptied the sounds directory.
	for _, name := range []string{"alarm1.mp3", "alarm2.mp3"} {
		if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Library().Refresh(); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.PlayAlarm(context.Background(), "alarm1"); err != nil {
		t.Fatalf("PlayAlarm: %v", err)
	}
	play, _ := f.player.LastPlay()
	if !strings.HasSuffix(play.Path, "fallback.wav") {
		t.Errorf("expected the built-in tone, played %s", play.Path)
	}
}

func TestServiceReportsPlaybackStartFailure(t *testing.T) {
	f := newServiceFixture(t, nil)
	f.player.SetError(errors.New("no such audio device"))

	if err := f.svc.PlayAlarm(context.Background(), "alarm1"); err == nil {
		t.Fatal("expected an error")
	}
	if f.svc.IsPlaying() {
		t.Error("IsPlaying should be false after a failed start")
	}
	if f.svc.Health().LastError == "" {
		t.Error("the failure should be recorded in health")
	}
}

func TestServiceNoticesAPlayerThatDiesMidAlarm(t *testing.T) {
	f := newServiceFixture(t, nil)
	if err := f.svc.PlayAlarm(context.Background(), "alarm1"); err != nil {
		t.Fatal(err)
	}
	f.player.LastHandle().Crash(errors.New("mpv segfaulted"))

	deadline := time.Now().Add(2 * time.Second)
	for f.svc.IsPlaying() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.svc.IsPlaying() {
		t.Fatal("service still believes it is playing after the player died")
	}
	if got := f.svc.Health().LastError; got == "" {
		t.Error("the crash should be recorded in health")
	}
}

func TestServiceVolumeKnobIsAuthoritative(t *testing.T) {
	f := newServiceFixture(t, nil)
	ctx := context.Background()
	if err := f.svc.ResolveDevice(ctx); err != nil {
		t.Fatal(err)
	}

	// Before the knob reports in, volume is provisional.
	pct, auth := f.svc.Volume()
	if auth {
		t.Error("volume must not be authoritative before the knob reports")
	}
	if pct != config.Default().Audio.StartupVolumePercent {
		t.Errorf("startup volume: %d", pct)
	}

	f.runner.Reset()
	if err := f.svc.SetAlarmVolume(ctx, 65, true); err != nil {
		t.Fatalf("SetAlarmVolume: %v", err)
	}
	pct, auth = f.svc.Volume()
	if pct != 65 || !auth {
		t.Fatalf("after the knob: %d, %v", pct, auth)
	}

	// It must reach the card's hardware mixer, not just mpv.
	last, ok := f.runner.LastCall()
	if !ok {
		t.Fatal("no amixer call")
	}
	if got := last.String(); got != "amixer -c 1 -- sset PCM 65%" {
		t.Errorf("amixer command: %q", got)
	}

	// Software volume is refused by default.
	if err := f.svc.SetAlarmVolume(ctx, 10, false); err == nil {
		t.Error("software volume should be refused when the knob is authoritative")
	}
	if pct, _ := f.svc.Volume(); pct != 65 {
		t.Errorf("a refused software change must not alter the volume: %d", pct)
	}
}

func TestServiceSoftwareVolumeWhenAllowed(t *testing.T) {
	f := newServiceFixture(t, func(c *config.Audio) {
		c.AllowSoftwareVolume = true
		c.MixerControl = "none" // force the software path
	})
	ctx := context.Background()
	if err := f.svc.ResolveDevice(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.PlayAlarm(ctx, "alarm1"); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.SetAlarmVolume(ctx, 30, false); err != nil {
		t.Fatalf("SetAlarmVolume: %v", err)
	}
	if got := f.player.LastHandle().Volume(); got != 30 {
		t.Errorf("software volume not applied to the player: %d", got)
	}

	// The knob still wins afterwards.
	if err := f.svc.SetAlarmVolume(ctx, 80, true); err != nil {
		t.Fatal(err)
	}
	if pct, auth := f.svc.Volume(); pct != 80 || !auth {
		t.Errorf("knob did not override: %d, %v", pct, auth)
	}
	if f.svc.Health().HardwareVolume {
		t.Error("mixer_control=none should force software volume")
	}
}

func TestServiceVolumeIsClampedAndDeduplicated(t *testing.T) {
	f := newServiceFixture(t, nil)
	ctx := context.Background()
	if err := f.svc.ResolveDevice(ctx); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.SetAlarmVolume(ctx, 500, true); err != nil {
		t.Fatal(err)
	}
	if pct, _ := f.svc.Volume(); pct != 100 {
		t.Errorf("clamp high: %d", pct)
	}
	if err := f.svc.SetAlarmVolume(ctx, -20, true); err != nil {
		t.Fatal(err)
	}
	if pct, _ := f.svc.Volume(); pct != 0 {
		t.Errorf("clamp low: %d", pct)
	}

	// Repeating the same value must not re-issue the mixer command.
	f.runner.Reset()
	if err := f.svc.SetAlarmVolume(ctx, 0, true); err != nil {
		t.Fatal(err)
	}
	if n := len(f.runner.Calls()); n != 0 {
		t.Errorf("an unchanged volume issued %d commands", n)
	}
}

func TestServiceTestAlarmRefusesDuringARealAlarm(t *testing.T) {
	f := newServiceFixture(t, nil)
	ctx := context.Background()
	if err := f.svc.PlayAlarm(ctx, "alarm1"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.TestAlarm(ctx, "alarm2", time.Second); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}

func TestServiceTestAlarmDoesNotLoopAndIsTimeLimited(t *testing.T) {
	f := newServiceFixture(t, nil)
	ctx := context.Background()

	if err := f.svc.TestAlarm(ctx, "alarm2", 5*time.Second); err != nil {
		t.Fatalf("TestAlarm: %v", err)
	}
	play, _ := f.player.LastPlay()
	if play.Opts.Loop {
		t.Error("a test preview must not loop")
	}

	// Let the watcher register its limit timer, then expire it.
	deadline := time.Now().Add(2 * time.Second)
	for f.clock.Waiters() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	f.clock.Advance(6 * time.Second)

	h := f.player.LastHandle()
	for !h.Stopped() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !h.Stopped() {
		t.Error("test playback was not stopped at its time limit")
	}
}

func TestServicePlayReplacesExistingPlayback(t *testing.T) {
	f := newServiceFixture(t, nil)
	ctx := context.Background()
	if err := f.svc.PlayAlarm(ctx, "alarm1"); err != nil {
		t.Fatal(err)
	}
	first := f.player.LastHandle()

	if err := f.svc.PlayAlarm(ctx, "alarm2"); err != nil {
		t.Fatal(err)
	}
	if !first.Stopped() {
		t.Error("the previous playback should have been stopped")
	}
	if len(f.player.Plays()) != 2 {
		t.Errorf("plays: %d", len(f.player.Plays()))
	}
}

func TestServiceHealthSnapshot(t *testing.T) {
	f := newServiceFixture(t, nil)
	ctx := context.Background()
	if err := f.svc.ResolveDevice(ctx); err != nil {
		t.Fatal(err)
	}
	h := f.svc.Health()
	// Two files plus the registered fallback tone.
	if h.SoundCount != 3 {
		t.Errorf("sound count: %d", h.SoundCount)
	}
	if h.Playing {
		t.Error("should not be playing")
	}
	if h.CardName == "" {
		t.Error("card description missing")
	}
}

func ids(ss []Sound) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

func TestDisplayNameSeparators(t *testing.T) {
	// A separator already contributes the space, so the digit-boundary rule
	// must not add a second one. "alarm_1" is how the files are actually named,
	// and it rendered as "Alarm  1" until this was fixed.
	for _, tc := range []struct{ id, want string }{
		{"alarm1", "Alarm 1"},
		{"alarm_1", "Alarm 1"},
		{"alarm-2", "Alarm 2"},
		{"alarm_10", "Alarm 10"},
		{"klaxon", "Klaxon"},
		{"gentle_wake_up", "Gentle wake up"},
		{"track_01_intro", "Track 01 intro"},
		{"_leading", "leading"},
		{"", ""},
	} {
		if got := displayName(tc.id); got != tc.want {
			t.Errorf("displayName(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
