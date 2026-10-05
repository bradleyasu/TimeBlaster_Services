package wifi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Raspberry Pi Connect runs as a systemd *user* service belonging to the
// account that signed in, not as a system service. Root cannot drive it with a
// plain systemctl call: that reaches root's own user manager, where Connect is
// not configured, and the switch would appear to work while doing nothing.
//
// So the helper becomes that user. runuser gives us their identity, and
// XDG_RUNTIME_DIR points at their user bus, which is what systemctl --user
// needs to find the manager. Lingering keeps that manager running with nobody
// logged in, which is why this works at all on a headless device.
const remoteRuntimeDirFmt = "/run/user/%d"

// remoteArgs builds the argv for one Connect subcommand run as the Connect user.
func (s *Server) remoteArgs(uid int, sub string) (string, []string) {
	runtimeDir := fmt.Sprintf(remoteRuntimeDirFmt, uid)
	return "runuser", []string{
		"-u", s.cfg.RemoteUser, "--",
		"env",
		"XDG_RUNTIME_DIR=" + runtimeDir,
		// `on` and `off` go through the session bus and fail with "no D-Bus
		// session bus is set" without this. `status` happens not to need it,
		// which is a good way to end up shipping a switch that reports
		// correctly and changes nothing.
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus",
		s.cfg.RemoteCommand, sub,
	}
}

// remoteConfigured reports whether a remote access switch makes any sense here.
func (s *Server) remoteConfigured() bool {
	return s.cfg.RemoteCommand != "" && s.cfg.RemoteUser != ""
}

// remoteUID looks up the Connect user's numeric id, which the runtime directory
// path is built from.
func (s *Server) remoteUID(ctx context.Context) (int, error) {
	out, err := s.runner.Run(ctx, "id", "-u", s.cfg.RemoteUser)
	if err != nil {
		return 0, fmt.Errorf("wifi: looking up %q: %w", s.cfg.RemoteUser, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("wifi: %q has no numeric uid: %w", s.cfg.RemoteUser, err)
	}
	return uid, nil
}

// RemoteStatusNow reports whether remote access is running.
func (s *Server) RemoteStatusNow(ctx context.Context) (RemoteStatus, error) {
	if !s.remoteConfigured() {
		return RemoteStatus{}, fmt.Errorf("wifi: remote access is not configured")
	}
	uid, err := s.remoteUID(ctx)
	if err != nil {
		return RemoteStatus{}, err
	}
	name, args := s.remoteArgs(uid, "status")
	out, err := s.runner.Run(ctx, name, args...)

	// A stopped service is a state, not a failure: Connect says so on stderr and
	// exits non-zero. The runner folds stderr into the error, so the error text
	// has to be parsed too -- reading stdout alone sees nothing and reports a
	// stopped service as unreachable, which hides the switch that would start it.
	text := string(out)
	if err != nil {
		text += "\n" + err.Error()
	}
	st := parseRemoteStatus(text)
	st.Detail = strings.TrimSpace(string(out))
	if !st.Available {
		if err != nil {
			return st, fmt.Errorf("wifi: reading remote access state: %w", err)
		}
		return st, fmt.Errorf("wifi: could not make sense of %q status output", s.cfg.RemoteCommand)
	}
	return st, nil
}

// parseRemoteStatus reads `rpi-connect status`.
//
// Running, the output is a few "Key: value" lines. Stopped, it is a single line
// saying so and nothing else -- no account information at all, which is why
// Available is derived from recognising either shape rather than from any field
// inside them. Screen sharing is always unavailable on a Lite install with no
// desktop, so its absence is not a fault and is not treated as one.
func parseRemoteStatus(out string) RemoteStatus {
	var st RemoteStatus
	st.Detail = strings.TrimSpace(out)

	// Connect's own way of saying it is installed but stopped.
	if strings.Contains(strings.ToLower(out), "not running") {
		st.Available = true
		return st
	}

	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.TrimSpace(value))
		switch key {
		case "signed in":
			// Recognising this line is what proves Connect answered.
			st.Available = true
			st.SignedIn = strings.HasPrefix(value, "yes")
		case "remote shell", "screen sharing":
			// "allowed" means the subsystem is up. Anything else -- "unavailable",
			// "disallowed" -- means it is not, and one working subsystem is
			// enough to call the service running.
			if strings.HasPrefix(value, "allowed") {
				st.Enabled = true
			}
		}
	}
	return st
}

// SetRemote starts or stops remote access.
//
// `rpi-connect on` and `off` both start/stop the service and set whether it
// starts at boot, so one call covers the running state and what happens after a
// restart. That is deliberate: a switch that forgets itself on reboot is a
// worse lie than one that does not exist.
func (s *Server) SetRemote(ctx context.Context, on bool) (RemoteStatus, error) {
	if !s.remoteConfigured() {
		return RemoteStatus{}, fmt.Errorf("wifi: remote access is not configured")
	}
	uid, err := s.remoteUID(ctx)
	if err != nil {
		return RemoteStatus{}, err
	}
	sub := "off"
	if on {
		sub = "on"
	}
	name, args := s.remoteArgs(uid, sub)
	if _, err := s.runner.Run(ctx, name, args...); err != nil {
		return RemoteStatus{}, fmt.Errorf("wifi: turning remote access %s: %w", sub, err)
	}
	return s.RemoteStatusNow(ctx)
}
