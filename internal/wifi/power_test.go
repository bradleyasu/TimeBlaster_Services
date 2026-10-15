package wifi

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

func TestPowerOffRunsExactlySystemctlPoweroff(t *testing.T) {
	// The helper runs as root. The request carries no fields, so the argv is
	// fixed: there is nothing a caller could send to make it run anything else.
	runner := system.NewFakeRunner()
	s := NewServer(config.Default().WiFi, runner, system.NewFakeClock(time.Now()), testLogger())

	if err := s.PowerOff(context.Background()); err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("ran %d commands, want exactly one: %+v", len(calls), calls)
	}
	if calls[0].Name != "systemctl" || !slices.Equal(calls[0].Args, []string{"poweroff"}) {
		t.Errorf("ran %s %v, want systemctl poweroff", calls[0].Name, calls[0].Args)
	}
}

func TestPowerOffReportsFailure(t *testing.T) {
	runner := system.NewFakeRunner()
	runner.Errors["systemctl"] = errors.New("Access denied")
	s := NewServer(config.Default().WiFi, runner, system.NewFakeClock(time.Now()), testLogger())

	resp := s.handle(context.Background(), Request{Version: ProtocolVersion, Op: OpPowerOff})
	if resp.OK || resp.Error == "" {
		t.Errorf("a failed power-off was reported as success: %+v", resp)
	}
}

func TestPowerOffIsAValidOperation(t *testing.T) {
	if err := (Request{Version: ProtocolVersion, Op: OpPowerOff}).Validate(); err != nil {
		t.Errorf("the helper would reject its own power-off: %v", err)
	}
}
