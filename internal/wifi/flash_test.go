package wifi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

// dfuSuccess is what dfu-util 0.11 printed flashing the real Nano with -R.
const dfuSuccess = `Opening DFU capable USB device...
Downloading element to address = 0x00000000, size = 309344
Download	[=========================] 100%       309344 bytes
Download done.
DFU state(7) = dfuMANIFEST, status(0) = No error condition is present
DFU state(2) = dfuIDLE, status(0) = No error condition is present
Done!
`

func flashServer(t *testing.T) (*Server, *system.FakeRunner, string) {
	t.Helper()
	image := filepath.Join(t.TempDir(), "nano-firmware.bin")
	if err := os.WriteFile(image, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Firmware
	cfg.Image = image
	runner := system.NewFakeRunner()
	s := NewServer(config.Default().WiFi, system.NewFakeRunner(), system.NewFakeClock(time.Now()), testLogger())
	s.EnableFirmwareFlashing(cfg, runner)
	return s, runner, image
}

func TestFlashNanoRunsExactlyDfuUtil(t *testing.T) {
	// Root, raw USB. The argv is fixed by the helper's own configuration: the
	// request has no fields, so the daemon cannot choose the image or device.
	s, runner, image := flashServer(t)
	runner.Outputs["dfu-util"] = []byte(dfuSuccess)

	if err := s.FlashNano(context.Background()); err != nil {
		t.Fatalf("FlashNano: %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("ran %d commands, want exactly one: %+v", len(calls), calls)
	}
	want := []string{"-d", "2341:0070", "-R", "-D", image}
	if calls[0].Name != "dfu-util" || !slices.Equal(calls[0].Args, want) {
		t.Errorf("ran %s, want dfu-util %v", calls[0], want)
	}
}

func TestFlashNanoTrustsDfuUtilsReportOverItsExitStatus(t *testing.T) {
	// -R resets the Nano into the new firmware, the device disappears under
	// dfu-util, and it exits 251 after a flash that worked.
	s, runner, _ := flashServer(t)
	runner.Outputs["dfu-util"] = []byte(dfuSuccess)
	runner.Errors["dfu-util"] = errors.New("dfu-util -d 2341:0070 -R -D x: exit status 251: can't detach")
	if err := s.FlashNano(context.Background()); err != nil {
		t.Errorf("a completed download was reported as a failure: %v", err)
	}

	// And the converse: a clean exit that never wrote anything is a failure.
	s, runner, _ = flashServer(t)
	runner.Outputs["dfu-util"] = []byte("Opening DFU capable USB device...\n")
	if err := s.FlashNano(context.Background()); err == nil {
		t.Error("a flash with no download was reported as success")
	}
}

func TestFlashNanoExplainsAMissingNano(t *testing.T) {
	s, runner, _ := flashServer(t)
	runner.Errors["dfu-util"] = errors.New("dfu-util: exit status 74: No DFU capable USB device available")
	err := s.FlashNano(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not found on USB") {
		t.Errorf("got %v, want it to say the Nano was not found", err)
	}
}

func TestFlashNanoRefusesWithoutAnImage(t *testing.T) {
	s, runner, image := flashServer(t)
	_ = os.Remove(image)
	if err := s.FlashNano(context.Background()); err == nil {
		t.Error("flashed an image that does not exist")
	}
	if n := len(runner.Calls()); n != 0 {
		t.Errorf("ran dfu-util with no image: %d calls", n)
	}

	// An empty file is a failed build, not a firmware.
	s, runner, image = flashServer(t)
	_ = os.WriteFile(image, nil, 0o644)
	if err := s.FlashNano(context.Background()); err == nil || len(runner.Calls()) != 0 {
		t.Errorf("flashed an empty image: err=%v calls=%d", err, len(runner.Calls()))
	}
}

func TestFlashNanoIsRefusedUnlessEnabled(t *testing.T) {
	s := NewServer(config.Default().WiFi, system.NewFakeRunner(), system.NewFakeClock(time.Now()), testLogger())
	resp := s.handle(context.Background(), Request{Version: ProtocolVersion, Op: OpFlashNano})
	if resp.OK {
		t.Error("a helper without flashing configured claimed to have flashed")
	}
}

func TestFlashNanoRefusesASecondFlashWhileOneRuns(t *testing.T) {
	s, runner, _ := flashServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	runner.OnRun = func(system.RecordedCommand) ([]byte, error) {
		close(started)
		<-release
		return []byte(dfuSuccess), nil
	}
	first := make(chan error, 1)
	go func() { first <- s.FlashNano(context.Background()) }()
	<-started

	if err := s.FlashNano(context.Background()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a second flash was not refused: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the first flash failed: %v", err)
	}
	if n := len(runner.Calls()); n != 1 {
		t.Errorf("dfu-util ran %d times, want once", n)
	}
}

func TestFlashNanoFitsInsideTheConnectionDeadline(t *testing.T) {
	// The helper drops a connection at its deadline. If that came before the
	// flash finished, the daemon would see a failure for a flash that worked.
	s, _, _ := flashServer(t)
	if got, flash := s.maxOperationTime(), config.Default().Firmware.FlashTimeout.Duration; got <= flash {
		t.Errorf("connections close after %v, but a flash may take %v", got, flash)
	}
	if err := (Request{Version: ProtocolVersion, Op: OpFlashNano}).Validate(); err != nil {
		t.Errorf("the helper would reject its own flash request: %v", err)
	}
}
