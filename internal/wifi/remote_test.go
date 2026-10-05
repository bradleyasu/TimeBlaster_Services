package wifi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

// Real output from the device, Lite OS with no desktop.
const realStatus = `Signed in: yes
Subscribed to events: yes
Screen sharing: unavailable
Remote shell: allowed (0 sessions active)`

func TestParseRemoteStatus(t *testing.T) {
	for _, tc := range []struct {
		name          string
		out           string
		wantEnabled   bool
		wantSignedIn  bool
		wantAvailable bool
	}{
		{
			// Screen sharing is always unavailable on a Lite install with no
			// desktop. Reading that as "off" would report a working service as
			// broken on exactly the hardware this runs on.
			"lite, shell allowed", realStatus, true, true, true,
		},
		{
			"stopped",
			"Signed in: yes\nScreen sharing: unavailable\nRemote shell: unavailable",
			false, true, true,
		},
		{
			"signed out",
			"Signed in: no\nScreen sharing: unavailable\nRemote shell: unavailable",
			false, false, true,
		},
		{
			"screen sharing alone is enough",
			"Signed in: yes\nScreen sharing: allowed\nRemote shell: disallowed",
			true, true, true,
		},
		{"empty", "", false, false, false},
		{"not key-value at all", "command not found", false, false, false},
		{
			// Stopped is the normal state once the switch exists, and Connect
			// says nothing about the account then. Treating that as unavailable
			// hid the switch in the one state it is for.
			"stopped: installed but not running",
			"\u2717 Raspberry Pi Connect is not running, run rpi-connect on",
			false, false, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRemoteStatus(tc.out)
			if got.Enabled != tc.wantEnabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tc.wantEnabled)
			}
			if got.SignedIn != tc.wantSignedIn {
				t.Errorf("SignedIn = %v, want %v", got.SignedIn, tc.wantSignedIn)
			}
			if got.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v", got.Available, tc.wantAvailable)
			}
		})
	}
}

func newRemoteServer(t *testing.T, runner *system.FakeRunner) *Server {
	t.Helper()
	cfg := config.Default().WiFi
	return NewServer(cfg, runner, system.NewFakeClock(time.Now()), testLogger())
}

func TestRemoteCommandsRunAsTheConnectUser(t *testing.T) {
	// Raspberry Pi Connect's units are user units. A plain systemctl as root
	// reaches root's own user manager, where Connect is not configured, and the
	// switch would appear to work while doing nothing at all.
	runner := system.NewFakeRunner()
	runner.Outputs["id"] = []byte("1000\n")
	runner.Outputs["runuser"] = []byte(realStatus)
	s := newRemoteServer(t, runner)

	if _, err := s.SetRemote(context.Background(), true); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	var sawOn bool
	for _, c := range runner.Calls() {
		if c.Name != "runuser" {
			continue
		}
		joined := strings.Join(c.Args, " ")
		if !strings.Contains(joined, "-u "+config.Default().WiFi.RemoteUser) {
			t.Errorf("runuser did not become the Connect user: %q", joined)
		}
		if !strings.Contains(joined, "XDG_RUNTIME_DIR=/run/user/1000") {
			t.Errorf("no user bus in the environment, so systemctl --user cannot find the manager: %q", joined)
		}
		// on and off go through the session bus and fail without this, while
		// status does not -- so omitting it ships a switch that reports
		// correctly and changes nothing.
		if !strings.Contains(joined, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus") {
			t.Errorf("no session bus address, so on/off cannot work: %q", joined)
		}
		if strings.HasSuffix(joined, " on") {
			sawOn = true
		}
	}
	if !sawOn {
		t.Errorf("never ran the on subcommand: %+v", runner.Calls())
	}
}

func TestRemoteOffUsesTheOffSubcommand(t *testing.T) {
	// `rpi-connect off` both stops the service and stops it starting at boot.
	// Stopping it alone would leave a switch that forgets itself on reboot.
	runner := system.NewFakeRunner()
	runner.Outputs["id"] = []byte("1000\n")
	runner.Outputs["runuser"] = []byte("Signed in: yes\nRemote shell: unavailable")
	s := newRemoteServer(t, runner)

	if _, err := s.SetRemote(context.Background(), false); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	var subs []string
	for _, c := range runner.Calls() {
		if c.Name == "runuser" && len(c.Args) > 0 {
			subs = append(subs, c.Args[len(c.Args)-1])
		}
	}
	if len(subs) == 0 || subs[0] != "off" {
		t.Errorf("expected the off subcommand first, got %v", subs)
	}
}

func TestRemoteIsRefusedWhenNotConfigured(t *testing.T) {
	// An installation without Connect should look absent, not broken.
	runner := system.NewFakeRunner()
	cfg := config.Default().WiFi
	cfg.RemoteCommand = ""
	s := NewServer(cfg, runner, system.NewFakeClock(time.Now()), testLogger())

	if _, err := s.RemoteStatusNow(context.Background()); err == nil {
		t.Error("status succeeded with no command configured")
	}
	if _, err := s.SetRemote(context.Background(), true); err == nil {
		t.Error("the switch worked with no command configured")
	}
	if len(runner.Calls()) != 0 {
		t.Errorf("ran something despite not being configured: %+v", runner.Calls())
	}
}

func TestStoppedServiceIsReadFromStderr(t *testing.T) {
	// Connect reports "not running" on stderr and exits non-zero, and the runner
	// folds stderr into the error. Parsing stdout alone sees an empty string,
	// calls the service unreachable, and hides the switch that would start it --
	// in the state that is now the default.
	runner := system.NewFakeRunner()
	runner.Outputs["id"] = []byte("1000\n")
	runner.Outputs["runuser"] = []byte("") // everything went to stderr
	runner.Errors["runuser"] = errors.New(
		"runuser ...: exit status 1: \u2717 Raspberry Pi Connect is not running, run rpi-connect on")
	s := newRemoteServer(t, runner)

	st, err := s.RemoteStatusNow(context.Background())
	if err != nil {
		t.Fatalf("a stopped service was reported as an error: %v", err)
	}
	if !st.Available {
		t.Error("a stopped but installed Connect was reported as unavailable, which hides the switch")
	}
	if st.Enabled {
		t.Error("a stopped service was reported as running")
	}
}

func TestRemoteStatusSurvivesANonZeroExit(t *testing.T) {
	// Connect reports "not running" by exiting non-zero. Treating that as a
	// failure would make a stopped service indistinguishable from a broken one,
	// and the switch would never show as off.
	runner := system.NewFakeRunner()
	runner.Outputs["id"] = []byte("1000\n")
	runner.Outputs["runuser"] = []byte("Signed in: yes\nRemote shell: unavailable")
	runner.Errors["runuser"] = errors.New("exit status 1")
	s := newRemoteServer(t, runner)

	st, err := s.RemoteStatusNow(context.Background())
	if err != nil {
		t.Fatalf("a stopped service was reported as an error: %v", err)
	}
	if st.Enabled {
		t.Error("a stopped service was reported as running")
	}
	if !st.SignedIn {
		t.Error("the signed-in state was lost")
	}
}
