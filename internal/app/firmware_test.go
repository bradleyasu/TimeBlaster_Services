package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/web"
)

// fakeFirmwarePort stands in for the hardware link.
type fakeFirmwarePort struct {
	mu       sync.Mutex
	status   hardware.Status
	paused   bool
	pauses   int
	pauseErr error
}

func (p *fakeFirmwarePort) Pause(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pauses++
	if p.pauseErr != nil {
		return p.pauseErr
	}
	p.paused = true
	p.status.Live = false
	return nil
}

func (p *fakeFirmwarePort) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
}

func (p *fakeFirmwarePort) Status() hardware.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *fakeFirmwarePort) isPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// comeBackRunning plays the Nano restarting into a version.
func (p *fakeFirmwarePort) comeBackRunning(version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.Live = true
	p.status.Firmware = version
}

const shippedVersion = "abc1234"

// firmwareHarness has a shipped image of shippedVersion and a live Nano running
// running.
func firmwareHarness(t *testing.T, running string) (*harness, *fakeFirmwarePort) {
	t.Helper()
	dir := t.TempDir()
	h := newHarness(t, func(c *config.Config) {
		c.Firmware.Image = filepath.Join(dir, "nano-firmware.bin")
		c.Firmware.VersionFile = filepath.Join(dir, "nano-firmware.version")
	})
	writeFile(t, h.app.cfg.Firmware.Image, "image")
	writeFile(t, h.app.cfg.Firmware.VersionFile, shippedVersion+"\n")

	port := &fakeFirmwarePort{status: hardware.Status{Live: true, Firmware: running}}
	h.app.fwPort = port

	old := [3]time.Duration{firmwarePauseTimeout, firmwareComeBack, firmwarePoll}
	firmwareComeBack, firmwarePoll = 300*time.Millisecond, time.Millisecond
	t.Cleanup(func() { firmwarePauseTimeout, firmwareComeBack, firmwarePoll = old[0], old[1], old[2] })
	return h, port
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func flashed(h *harness) int {
	n := 0
	for _, c := range h.wifi.Calls() {
		if c == "flash_nano" {
			n++
		}
	}
	return n
}

func waitForUpdateToFinish(t *testing.T, h *harness) web.FirmwareStatus {
	t.Helper()
	waitFor(t, "the update to finish", func() bool { return !h.app.FirmwareStatus().Updating })
	return h.app.FirmwareStatus()
}

func TestFirmwareUpdateIsOfferedOnlyWhenItWouldChangeSomething(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running string
		live    bool
		want    bool
	}{
		{"older firmware", "0123abc", true, true},
		// Everything flashed before this feature reports no version at all.
		{"firmware too old to say", "", true, true},
		{"already current", shippedVersion, true, false},
		// A Nano that is not talking may not be there to flash.
		{"silent Nano", "0123abc", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, port := firmwareHarness(t, tc.running)
			port.status.Live = tc.live
			st := h.app.FirmwareStatus()
			if st.UpdateAvailable != tc.want {
				t.Errorf("update_available = %v, want %v (%+v)", st.UpdateAvailable, tc.want, st)
			}
			if st.Available != shippedVersion || st.Running != tc.running {
				t.Errorf("versions misreported: %+v", st)
			}
		})
	}
}

func TestFirmwareUpdateIsNotOfferedWithoutAUsableImage(t *testing.T) {
	h, _ := firmwareHarness(t, "0123abc")
	// A garbled version file must not enable the button: it would flash an
	// image nobody can identify.
	writeFile(t, h.app.cfg.Firmware.VersionFile, "<html>oops</html>")
	if st := h.app.FirmwareStatus(); st.UpdateAvailable || st.Available != "" {
		t.Errorf("a garbled version file was trusted: %+v", st)
	}

	writeFile(t, h.app.cfg.Firmware.VersionFile, shippedVersion)
	_ = os.Remove(h.app.cfg.Firmware.Image)
	if st := h.app.FirmwareStatus(); st.UpdateAvailable {
		t.Errorf("an update was offered with no image to flash: %+v", st)
	}
}

func TestFirmwareUpdateFlashesAndConfirmsTheNewVersion(t *testing.T) {
	h, port := firmwareHarness(t, "0123abc")
	h.wifi.OnFlash = func() {
		if !port.isPaused() {
			t.Error("flashed while the link still held the port")
		}
		port.comeBackRunning(shippedVersion)
	}

	if err := h.app.UpdateFirmware(); err != nil {
		t.Fatalf("UpdateFirmware: %v", err)
	}
	st := waitForUpdateToFinish(t, h)
	if st.LastResult != "updated" || st.LastError != "" {
		t.Errorf("result = %q (%q), want updated", st.LastResult, st.LastError)
	}
	if st.UpdateAvailable {
		t.Error("the button is still offered after a successful update")
	}
	if port.isPaused() {
		t.Error("the link was left paused")
	}
	if n := flashed(h); n != 1 {
		t.Errorf("flashed %d times, want once", n)
	}
}

func TestFirmwareUpdateFailsIfTheNanoDoesNotComeBackRunningIt(t *testing.T) {
	// dfu-util reporting success is not the Nano running the image. Only the
	// Nano saying so counts.
	h, port := firmwareHarness(t, "0123abc")
	h.wifi.OnFlash = func() { port.comeBackRunning("0123abc") }

	if err := h.app.UpdateFirmware(); err != nil {
		t.Fatal(err)
	}
	st := waitForUpdateToFinish(t, h)
	if st.LastResult != "failed" || !strings.Contains(st.LastError, "0123abc") {
		t.Errorf("got %q (%q), want a failure naming the version it found", st.LastResult, st.LastError)
	}
	if port.isPaused() {
		t.Error("the link was left paused")
	}
}

func TestFirmwareUpdateResumesTheLinkWhenTheFlashFails(t *testing.T) {
	h, port := firmwareHarness(t, "0123abc")
	h.wifi.FlashErr = errors.New("wifi: firmware update failed: the Nano was not found on USB")

	if err := h.app.UpdateFirmware(); err != nil {
		t.Fatal(err)
	}
	st := waitForUpdateToFinish(t, h)
	if st.LastResult != "failed" || !strings.Contains(st.LastError, "not found on USB") {
		t.Errorf("got %q (%q)", st.LastResult, st.LastError)
	}
	// A Nano left disconnected is a dead display and dead knobs.
	if port.isPaused() {
		t.Error("a failed flash left the link paused")
	}
}

func TestFirmwareUpdateDoesNotFlashIfThePortWasNotReleased(t *testing.T) {
	h, port := firmwareHarness(t, "0123abc")
	port.pauseErr = errors.New("hardware: the serial port was not released")

	if err := h.app.UpdateFirmware(); err != nil {
		t.Fatal(err)
	}
	if st := waitForUpdateToFinish(t, h); st.LastResult != "failed" {
		t.Errorf("result = %q, want failed", st.LastResult)
	}
	if n := flashed(h); n != 0 {
		t.Errorf("flashed %d times with the port still held", n)
	}
}

func TestFirmwareUpdateRefusals(t *testing.T) {
	t.Run("already current", func(t *testing.T) {
		h, _ := firmwareHarness(t, shippedVersion)
		if err := h.app.UpdateFirmware(); !errors.Is(err, web.ErrFirmwareRefused) {
			t.Errorf("got %v, want a refusal", err)
		}
		if flashed(h) != 0 {
			t.Error("flashed firmware that was already current")
		}
	})

	t.Run("alarm ringing", func(t *testing.T) {
		// The Nano is half of how an alarm is stopped.
		h, _ := firmwareHarness(t, "0123abc")
		ringAnAlarm(t, h)
		if err := h.app.UpdateFirmware(); !errors.Is(err, web.ErrFirmwareRefused) {
			t.Errorf("got %v, want a refusal", err)
		}
		if flashed(h) != 0 {
			t.Error("flashed while an alarm was ringing")
		}
	})

	t.Run("already updating", func(t *testing.T) {
		h, port := firmwareHarness(t, "0123abc")
		release := make(chan struct{})
		h.wifi.OnFlash = func() { <-release; port.comeBackRunning(shippedVersion) }
		if err := h.app.UpdateFirmware(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the flash to start", func() bool { return flashed(h) == 1 })
		if err := h.app.UpdateFirmware(); !errors.Is(err, web.ErrFirmwareRefused) {
			t.Errorf("a second update was not refused: %v", err)
		}
		if !h.app.FirmwareStatus().Updating {
			t.Error("status does not say an update is running")
		}
		close(release)
		waitForUpdateToFinish(t, h)
		if n := flashed(h); n != 1 {
			t.Errorf("flashed %d times, want once", n)
		}
	})
}

func TestFirmwareIsNeverUpdatedAutomatically(t *testing.T) {
	// The user's rule: only the button flashes. An update being available, the
	// Nano reconnecting and time passing must not start one.
	h, port := firmwareHarness(t, "0123abc")
	port.comeBackRunning("0123abc")
	h.app.LinkUp("pipe")
	h.app.LinkDown(errors.New("gone"))
	h.app.LinkUp("pipe")
	for range 10 {
		h.clock.Advance(time.Hour)
	}
	time.Sleep(50 * time.Millisecond)
	if n := flashed(h); n != 0 {
		t.Errorf("the firmware was flashed %d times without anyone asking", n)
	}
	if !h.app.FirmwareStatus().UpdateAvailable {
		t.Error("the update should still be waiting for the button")
	}
}

func TestNanoHealthIsDegradedNotDownDuringAnUpdate(t *testing.T) {
	h, port := firmwareHarness(t, "0123abc")
	release := make(chan struct{})
	h.wifi.OnFlash = func() { <-release; port.comeBackRunning(shippedVersion) }
	if err := h.app.UpdateFirmware(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the flash to start", func() bool { return flashed(h) == 1 })
	if got := h.app.Health().Components["nano"]; got != state.StatusDegraded {
		t.Errorf("nano health during an update = %q, want degraded", got)
	}
	close(release)
	waitForUpdateToFinish(t, h)
}
