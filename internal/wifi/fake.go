package wifi

import (
	"context"
	"errors"
	"sync"
	"time"
)

// FakeManager is an in-memory Manager for tests and for running the daemon on a
// development machine with no helper installed.
type FakeManager struct {
	mu sync.Mutex

	status   Status
	networks []Network
	calls    []string

	// Err, when set, makes every operation fail.
	Err error
	// ConnectErr makes only Connect fail, which is how a wrong-password attempt
	// is simulated.
	ConnectErr error
	// Unavailable makes Available report false.
	Unavailable bool

	// remote is the fake's Raspberry Pi Connect state. Signed in and off is the
	// shipped posture: the device is linked to an account, and the service only
	// runs when somebody asks for it.
	remote RemoteStatus
	// RemoteErr makes only the remote access operations fail, which is how a
	// device without Connect installed is simulated.
	RemoteErr error
	// PowerOffErr makes only PowerOff fail.
	PowerOffErr error
}

// RemoteStatus reports the fake's remote access state.
func (f *FakeManager) RemoteStatus(ctx context.Context) (RemoteStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "remote_status")
	if f.RemoteErr != nil {
		return RemoteStatus{}, f.RemoteErr
	}
	if f.Err != nil {
		return RemoteStatus{}, f.Err
	}
	return f.remote, nil
}

// SetRemoteAccess starts or stops the fake's remote access.
func (f *FakeManager) SetRemoteAccess(ctx context.Context, on bool) (RemoteStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if on {
		f.calls = append(f.calls, "remote_on")
	} else {
		f.calls = append(f.calls, "remote_off")
	}
	if f.RemoteErr != nil {
		return RemoteStatus{}, f.RemoteErr
	}
	if f.Err != nil {
		return RemoteStatus{}, f.Err
	}
	f.remote.Enabled = on
	return f.remote, nil
}

// PowerOff records the request. It never actually powers anything off.
func (f *FakeManager) PowerOff(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "power_off")
	if f.PowerOffErr != nil {
		return f.PowerOffErr
	}
	return f.Err
}

// NewFakeManager returns a fake in the normal (connected) state.
func NewFakeManager() *FakeManager {
	return &FakeManager{
		remote: RemoteStatus{SignedIn: true, Enabled: false},
		status: Status{
			Mode: ModeNormal, Interface: "wlan0", Connected: true,
			SSID: "Home", IPv4: "192.168.1.42/24", Hostname: "timeblaster.local",
		},
		networks: []Network{
			{SSID: "Home", Signal: 82, Security: "WPA2", InUse: true},
			{SSID: "Neighbour", Signal: 41, Security: "WPA2"},
			{SSID: "CoffeeShop", Signal: 22},
		},
	}
}

func (f *FakeManager) record(op string) error {
	f.calls = append(f.calls, op)
	return f.Err
}

// Status reports the simulated state.
func (f *FakeManager) Status(context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("status"); err != nil {
		return Status{}, err
	}
	return f.status, nil
}

// EnterSetupMode simulates raising the access point.
func (f *FakeManager) EnterSetupMode(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("enter_setup"); err != nil {
		return err
	}
	f.status.Mode = ModeSetup
	f.status.SetupSSID = "TIMEBLASTER-SETUP"
	f.status.SetupSince = time.Now()
	return nil
}

// ExitSetupMode simulates tearing it down.
func (f *FakeManager) ExitSetupMode(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("exit_setup"); err != nil {
		return err
	}
	f.status.Mode = ModeNormal
	f.status.SetupSSID = ""
	return nil
}

// Scan returns the simulated network list.
func (f *FakeManager) Scan(context.Context) ([]Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("scan"); err != nil {
		return nil, err
	}
	return append([]Network(nil), f.networks...), nil
}

// Connect simulates joining a network.
func (f *FakeManager) Connect(_ context.Context, ssid, passphrase string, _ bool) (ConnectResult, error) {
	if err := ValidateCredentials(ssid, passphrase); err != nil {
		return ConnectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("connect"); err != nil {
		return ConnectResult{}, err
	}
	if f.ConnectErr != nil {
		return ConnectResult{
			Success: false, SSID: ssid,
			Message:    f.ConnectErr.Error(),
			RolledBack: true,
		}, f.ConnectErr
	}
	f.status.Mode = ModeNormal
	f.status.Connected = true
	f.status.SSID = ssid
	f.status.IPv4 = "192.168.1.42/24"
	return ConnectResult{Success: true, SSID: ssid, IPv4: f.status.IPv4}, nil
}

// Available reports helper reachability.
func (f *FakeManager) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.Unavailable
}

// Calls returns the operations performed.
func (f *FakeManager) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// SetStatus replaces the simulated status.
func (f *FakeManager) SetStatus(s Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

// SetConnectError makes the next connection attempt fail.
func (f *FakeManager) SetConnectError(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg == "" {
		f.ConnectErr = nil
		return
	}
	f.ConnectErr = errors.New(msg)
}

var _ Manager = (*FakeManager)(nil)
