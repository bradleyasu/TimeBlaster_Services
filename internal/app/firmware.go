package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/hardware"
	"github.com/bradsheets/timeblaster/internal/state"
	"github.com/bradsheets/timeblaster/internal/web"
)

// firmwarePort is the part of the hardware link a firmware update needs: give
// up the serial port while dfu-util has the Nano, take it back afterwards, and
// see what the Nano reports once it is running again.
type firmwarePort interface {
	Pause(ctx context.Context) error
	Resume()
	Status() hardware.Status
}

// How long each stage of an update may take. Variables so tests can shorten
// them; they are measured in real time, because they wait on USB and on other
// processes rather than on anything the daemon schedules.
var (
	firmwarePauseTimeout = 10 * time.Second
	// firmwareComeBack covers the Nano restarting, re-enumerating, the link
	// reconnecting, and the first PING carrying the new version -- normally a
	// few seconds.
	firmwareComeBack = 45 * time.Second
	firmwarePoll     = 250 * time.Millisecond
)

// firmwareVersionPattern is what version.py produces: an abbreviated commit
// hash. Anything else in the version file is treated as no image at all, so a
// truncated or garbled file can never enable the button.
var firmwareVersionPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// firmwareUpdater holds the state of the one update that may be running.
type firmwareUpdater struct {
	mu       sync.Mutex
	updating bool
	result   string
	lastErr  string
	finished time.Time
}

// availableFirmware reads the version of the shipped image, or "" when there
// is no usable one.
func (a *App) availableFirmware() string {
	f, err := os.Open(a.cfg.Firmware.VersionFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if !firmwareVersionPattern.MatchString(v) {
		return ""
	}
	if _, err := os.Stat(a.cfg.Firmware.Image); err != nil {
		return ""
	}
	return v
}

// FirmwareStatus reports the running and available firmware.
func (a *App) FirmwareStatus() web.FirmwareStatus {
	a.fw.mu.Lock()
	st := web.FirmwareStatus{
		Updating:     a.fw.updating,
		LastResult:   a.fw.result,
		LastError:    a.fw.lastErr,
		LastFinished: a.fw.finished,
	}
	a.fw.mu.Unlock()

	st.Available = a.availableFirmware()
	if a.fwPort == nil {
		return st
	}
	hw := a.fwPort.Status()
	st.Running = hw.Firmware
	// Live, not merely connected: an update needs a Nano that is running and
	// answering, and comparing against a version not yet reported would offer
	// an update for the first moment of every reconnect.
	st.UpdateAvailable = st.Available != "" && hw.Live && st.Running != st.Available &&
		!st.Updating && a.wifi != nil
	return st
}

// firmwareUpdating reports whether an update is in progress.
func (a *App) firmwareUpdating() bool {
	a.fw.mu.Lock()
	defer a.fw.mu.Unlock()
	return a.fw.updating
}

// UpdateFirmware flashes the shipped image onto the Nano. It is only ever
// called from the companion app's button; nothing in the daemon starts one.
func (a *App) UpdateFirmware() error {
	st := a.FirmwareStatus()
	switch {
	case st.Updating:
		return fmt.Errorf("%w: an update is already running", web.ErrFirmwareRefused)
	case a.scheduler != nil && a.scheduler.IsRinging():
		// The Nano is half of how an alarm is stopped.
		return fmt.Errorf("%w: an alarm is ringing", web.ErrFirmwareRefused)
	case a.wifi == nil || a.fwPort == nil:
		return fmt.Errorf("%w: this Timeblaster cannot flash its controller", web.ErrFirmwareRefused)
	case !st.UpdateAvailable:
		return fmt.Errorf("%w: no update is available", web.ErrFirmwareRefused)
	}

	a.fw.mu.Lock()
	if a.fw.updating {
		a.fw.mu.Unlock()
		return fmt.Errorf("%w: an update is already running", web.ErrFirmwareRefused)
	}
	a.fw.updating = true
	a.fw.result, a.fw.lastErr = "", ""
	a.fw.mu.Unlock()

	a.log.Warn("updating the Nano's firmware at the user's request",
		"running", st.Running, "installing", st.Available)
	a.publishFirmware()
	go a.runFirmwareUpdate(st.Available)
	return nil
}

func (a *App) runFirmwareUpdate(want string) {
	err := a.flashAndVerify(want)

	a.fw.mu.Lock()
	a.fw.updating = false
	a.fw.finished = a.clock.Now()
	if err != nil {
		a.fw.result, a.fw.lastErr = "failed", err.Error()
	} else {
		a.fw.result, a.fw.lastErr = "updated", ""
	}
	a.fw.mu.Unlock()

	if err != nil {
		a.log.Error("Nano firmware update failed", "error", err)
	} else {
		a.log.Info("Nano firmware updated", "version", want)
	}
	a.publishFirmware()
}

func (a *App) flashAndVerify(want string) error {
	pauseCtx, cancel := context.WithTimeout(context.Background(), firmwarePauseTimeout)
	err := a.fwPort.Pause(pauseCtx)
	cancel()
	// Whatever happens from here, the link must come back: a Nano left
	// disconnected is a dead display and dead knobs.
	defer a.fwPort.Resume()
	if err != nil {
		return err
	}

	flashCtx, cancel := context.WithTimeout(context.Background(),
		a.cfg.Firmware.FlashTimeout.Duration+30*time.Second)
	err = a.wifi.FlashNano(flashCtx)
	cancel()
	if err != nil {
		return err
	}
	a.fwPort.Resume()

	// dfu-util saying it wrote the image is not the same as the Nano running
	// it. Only the Nano reporting the new version counts.
	deadline := time.Now().Add(firmwareComeBack)
	for {
		hw := a.fwPort.Status()
		if hw.Live && hw.Firmware == want {
			return nil
		}
		if time.Now().After(deadline) {
			if hw.Firmware == "" {
				return errors.New("the controller did not come back after the update")
			}
			return fmt.Errorf("the controller came back running %s, not %s", hw.Firmware, want)
		}
		time.Sleep(firmwarePoll)
	}
}

func (a *App) publishFirmware() {
	a.publish(state.NewEvent(state.EventFirmwareChanged, a.FirmwareStatus()))
}
