// Package wifi handles network configuration, split across a privilege boundary.
//
// The main daemon runs unprivileged and speaks only the small RPC defined here
// over a unix socket. The timeblaster-wifi helper runs as root, owns every nmcli
// invocation, and serves the captive-portal page while setup mode is active.
// Nothing in timeblasterd can run an arbitrary command, change a network, or bind
// a privileged port.
//
// Wi-Fi setup has exactly one entry path: holding the dedicated button for five
// seconds. The daemon never starts an access point on its own because a network
// is unreachable — a Timeblaster that spawns an open access point every time the
// router reboots is worse than one that simply waits.
package wifi

import (
	"encoding/json"
	"fmt"
	"time"
)

// ProtocolVersion is the helper RPC version. The helper rejects a mismatched
// client so a half-upgraded install fails loudly rather than mysteriously.
const ProtocolVersion = 1

// Op identifies an RPC operation.
type Op string

// Operations understood by the helper.
const (
	// OpStatus reports the current network state.
	OpStatus Op = "status"
	// OpEnterSetup starts the access point and captive portal.
	OpEnterSetup Op = "enter_setup"
	// OpExitSetup tears them down and restores normal networking.
	OpExitSetup Op = "exit_setup"
	// OpScan lists visible networks.
	OpScan Op = "scan"
	// OpConnect joins a network, validating before committing.
	OpConnect Op = "connect"
	// OpPing checks that the helper is alive.
	OpPing Op = "ping"
)

// Request is one RPC call.
type Request struct {
	Version int    `json:"version"`
	Op      Op     `json:"op"`
	SSID    string `json:"ssid,omitempty"`
	// Passphrase is write-only: it is never echoed in a response or a log line.
	Passphrase string `json:"passphrase,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"`
}

// Response is one RPC reply.
type Response struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`

	Status   *Status        `json:"status,omitempty"`
	Networks []Network      `json:"networks,omitempty"`
	Connect  *ConnectResult `json:"connect,omitempty"`
}

// Mode is the device's current networking mode.
type Mode string

const (
	// ModeNormal means ordinary client operation.
	ModeNormal Mode = "normal"
	// ModeSetup means the setup access point is up.
	ModeSetup Mode = "setup"
	// ModeUnknown means the helper could not determine the state.
	ModeUnknown Mode = "unknown"
)

// Status describes the device's network state.
type Status struct {
	Mode Mode `json:"mode"`
	// Interface is the wireless device, usually wlan0.
	Interface string `json:"interface"`
	// Connected reports whether the interface has an active connection.
	Connected bool `json:"connected"`
	// SSID is the network currently joined.
	SSID string `json:"ssid,omitempty"`
	// IPv4 is the current address, e.g. 192.168.1.42/24.
	IPv4 string `json:"ipv4,omitempty"`
	// Signal is the current signal strength, 0-100.
	Signal int `json:"signal,omitempty"`
	// Hostname is the mDNS name the UI is reachable at.
	Hostname string `json:"hostname,omitempty"`
	// SetupSSID is the access point name, populated while in setup mode.
	SetupSSID string `json:"setup_ssid,omitempty"`
	// SetupSince is when setup mode began.
	SetupSince time.Time `json:"setup_since,omitzero"`
	// SetupExpiresAt is when setup mode will end by itself.
	SetupExpiresAt time.Time `json:"setup_expires_at,omitzero"`
	// LastError describes the most recent networking failure.
	LastError string `json:"last_error,omitempty"`
}

// Network is a visible Wi-Fi network.
type Network struct {
	SSID string `json:"ssid"`
	// Signal is strength as a percentage.
	Signal int `json:"signal"`
	// Security is the security type, e.g. "WPA2"; empty means an open network.
	Security string `json:"security,omitempty"`
	// InUse marks the network currently joined.
	InUse bool `json:"in_use,omitempty"`
}

// Secured reports whether the network needs a passphrase.
func (n Network) Secured() bool { return n.Security != "" && n.Security != "--" }

// ConnectResult describes the outcome of a connection attempt.
type ConnectResult struct {
	Success bool   `json:"success"`
	SSID    string `json:"ssid"`
	IPv4    string `json:"ipv4,omitempty"`
	// Message is a user-facing explanation, safe to display in the portal.
	Message string `json:"message,omitempty"`
	// RolledBack reports whether the previous network was restored after a
	// failure, which is what guarantees a failed attempt cannot strand the device.
	RolledBack bool `json:"rolled_back,omitempty"`
}

// Validate checks an inbound request. The helper runs as root, so every field
// that reaches a command line is checked here first.
func (r Request) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("wifi: unsupported protocol version %d (helper speaks %d)", r.Version, ProtocolVersion)
	}
	switch r.Op {
	case OpStatus, OpEnterSetup, OpExitSetup, OpScan, OpPing:
		return nil
	case OpConnect:
		return ValidateCredentials(r.SSID, r.Passphrase)
	default:
		return fmt.Errorf("wifi: unknown operation %q", r.Op)
	}
}

// ValidateCredentials checks an SSID and passphrase against the 802.11 limits.
//
// This is a security boundary, not a nicety: these values come from a web form
// served on an open access point and are passed to nmcli. Control characters are
// rejected outright so nothing can smuggle a newline or a NUL into an argument.
func ValidateCredentials(ssid, passphrase string) error {
	if ssid == "" {
		return fmt.Errorf("wifi: the network name must not be empty")
	}
	if len(ssid) > 32 {
		return fmt.Errorf("wifi: the network name must be 32 bytes or fewer, got %d", len(ssid))
	}
	if hasControlChars(ssid) {
		return fmt.Errorf("wifi: the network name contains invalid characters")
	}
	if passphrase != "" {
		if n := len(passphrase); n < 8 || n > 63 {
			return fmt.Errorf("wifi: the password must be 8 to 63 characters, got %d", n)
		}
		if hasControlChars(passphrase) {
			return fmt.Errorf("wifi: the password contains invalid characters")
		}
	}
	return nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Redacted returns a copy of the request safe to log: the passphrase is replaced
// with a fixed marker so a password can never reach the journal.
func (r Request) Redacted() Request {
	if r.Passphrase != "" {
		r.Passphrase = "[redacted]"
	}
	return r
}

// errorResponse builds a failed reply.
func errorResponse(err error) Response {
	return Response{Version: ProtocolVersion, OK: false, Error: err.Error()}
}

// encode writes a message as one JSON line, which keeps the wire format
// debuggable with socat.
func encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
