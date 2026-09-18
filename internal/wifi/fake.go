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
}

// NewFakeManager returns a fake in the normal (connected) state.
func NewFakeManager() *FakeManager {
	return &FakeManager{
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
