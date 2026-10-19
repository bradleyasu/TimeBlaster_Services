package wifi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

// maxFirmwareImage is far larger than any real image -- they are about 300 KB
// -- and far smaller than anything that should be written to a microcontroller.
const maxFirmwareImage = 8 << 20

// firmwareFlasher writes the shipped Nano firmware. It is root's job because
// the DFU interface is a raw USB device, and the daemon is not trusted with raw
// USB access to anything.
type firmwareFlasher struct {
	cfg    config.Firmware
	runner system.CommandRunner
	// busy refuses a second flash while one is running. dfu-util would fail on
	// its own, but only after the first had already lost the device to it.
	busy sync.Mutex
}

// EnableFirmwareFlashing lets the helper flash the Nano. runner is separate from
// the helper's own because a flash legitimately outlasts the short cap every
// other command runs under. Without this call, flash requests are refused.
func (s *Server) EnableFirmwareFlashing(cfg config.Firmware, runner system.CommandRunner) {
	s.flasher = &firmwareFlasher{cfg: cfg, runner: runner}
}

// FlashNano writes the shipped firmware image to the Nano and restarts it.
//
// The request carries no fields. The image, the program and the device are all
// fixed by the helper's own configuration, so nothing the daemon sends can
// choose what gets written or where.
//
// Success is judged by dfu-util's own report, not its exit status: -R resets
// the Nano into the new firmware, the device vanishes mid-conversation, and
// dfu-util exits 251 after a flash that worked perfectly.
func (s *Server) FlashNano(ctx context.Context) error {
	f := s.flasher
	if f == nil {
		return errors.New("wifi: firmware flashing is not enabled")
	}
	if !f.busy.TryLock() {
		return errors.New("wifi: a firmware update is already running")
	}
	defer f.busy.Unlock()

	info, err := os.Stat(f.cfg.Image)
	if err != nil {
		return fmt.Errorf("wifi: no firmware image to flash: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxFirmwareImage {
		return fmt.Errorf("wifi: %s is not a plausible firmware image (%d bytes)", f.cfg.Image, info.Size())
	}

	ctx, cancel := context.WithTimeout(ctx, f.cfg.FlashTimeout.Duration)
	defer cancel()

	s.log.Warn("flashing Nano firmware at the daemon's request", "image", f.cfg.Image, "bytes", info.Size())
	out, runErr := f.runner.Run(ctx, f.cfg.DFUUtil, "-d", f.cfg.USBID, "-R", "-D", f.cfg.Image)
	report := string(out)
	if runErr != nil {
		report += "\n" + runErr.Error()
	}

	if strings.Contains(report, "Download done.") {
		s.log.Info("Nano firmware written; the Nano is restarting into it", "exit", errString(runErr))
		return nil
	}
	s.log.Error("Nano firmware update failed", "error", errString(runErr), "output", lastLines(report, 6))
	return fmt.Errorf("wifi: firmware update failed: %s", flashFailure(report))
}

// flashFailure says what went wrong in terms someone could act on.
func flashFailure(report string) string {
	switch {
	case strings.Contains(report, "No DFU capable USB device"):
		return "the Nano was not found on USB"
	case strings.Contains(report, "deadline exceeded"):
		return "the update took too long"
	case strings.Contains(report, "executable file not found"):
		return "dfu-util is not installed"
	}
	if line := lastLines(report, 1); line != "" {
		return line
	}
	return "dfu-util reported nothing"
}

// lastLines returns the final n non-empty lines of s, joined by " | ".
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
