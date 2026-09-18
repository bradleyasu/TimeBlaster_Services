package wifi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

// Server is the privileged half of the Wi-Fi subsystem. It runs as root in the
// timeblaster-wifi service, owns every nmcli invocation, and is the only thing
// that can change the network.
//
// Its socket is group-owned by the timeblaster user and mode 0660, so the main
// daemon can reach it and nothing else on the system can.
type Server struct {
	cfg    config.WiFi
	runner system.CommandRunner
	clock  system.Clock
	log    *slog.Logger
	portal *Portal

	mu sync.Mutex
	// mode is the current networking mode.
	mode Mode
	// setupSince and setupExpires bound setup mode.
	setupSince   time.Time
	setupExpires time.Time
	// previousConnection is the profile that was active before setup mode, so a
	// failed attempt can be rolled back to a known-good network.
	previousConnection string
	lastErr            string
	// setupCancel stops the setup-mode timeout goroutine.
	setupCancel context.CancelFunc
}

// NewServer builds the helper.
func NewServer(cfg config.WiFi, runner system.CommandRunner, clock system.Clock, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, runner: runner, clock: clock, log: log, mode: ModeNormal}
	s.portal = NewPortal(cfg, s, log)
	return s
}

// Listen serves the helper RPC until the context is cancelled.
func (s *Server) Listen(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.HelperSocket), 0o755); err != nil {
		return fmt.Errorf("wifi: creating the socket directory: %w", err)
	}
	// A socket left behind by a killed helper would otherwise block the bind.
	_ = os.Remove(s.cfg.HelperSocket)

	ln, err := net.Listen("unix", s.cfg.HelperSocket)
	if err != nil {
		return fmt.Errorf("wifi: listening on %s: %w", s.cfg.HelperSocket, err)
	}
	defer func() {
		ln.Close()
		_ = os.Remove(s.cfg.HelperSocket)
	}()

	if err := s.secureSocket(); err != nil {
		s.log.Warn("could not restrict the helper socket's ownership", "error", err)
	}
	s.log.Info("wifi helper listening", "socket", s.cfg.HelperSocket, "interface", s.cfg.Interface)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.shutdown()
				return ctx.Err()
			}
			s.log.Warn("accept failed on the helper socket", "error", err)
			continue
		}
		go s.serve(ctx, conn)
	}
}

// secureSocket makes the socket group-readable by the timeblaster user only.
func (s *Server) secureSocket() error {
	if err := os.Chmod(s.cfg.HelperSocket, 0o660); err != nil {
		return err
	}
	u, err := user.Lookup("timeblaster")
	if err != nil {
		// On a development machine the user does not exist; 0660 plus root
		// ownership is still a safe default.
		return nil
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	return os.Chown(s.cfg.HelperSocket, 0, gid)
}

func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(s.maxOperationTime()))

	// One request per connection keeps the protocol trivially stateless and means
	// a stuck client cannot hold a session open.
	r := bufio.NewReaderSize(conn, 64*1024)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}

	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(conn, errorResponse(fmt.Errorf("wifi: malformed request: %w", err)))
		return
	}
	if err := req.Validate(); err != nil {
		s.log.Warn("rejecting an invalid helper request", "request", req.Redacted(), "error", err)
		s.reply(conn, errorResponse(err))
		return
	}

	s.log.Debug("helper request", "op", req.Op)
	s.reply(conn, s.handle(ctx, req))
}

func (s *Server) reply(conn net.Conn, resp Response) {
	resp.Version = ProtocolVersion
	payload, err := encode(resp)
	if err != nil {
		s.log.Error("could not encode a helper response", "error", err)
		return
	}
	if _, err := conn.Write(payload); err != nil {
		s.log.Debug("could not write a helper response", "error", err)
	}
}

func (s *Server) handle(ctx context.Context, req Request) Response {
	switch req.Op {
	case OpPing:
		return Response{OK: true}

	case OpStatus:
		st := s.Status(ctx)
		return Response{OK: true, Status: &st}

	case OpScan:
		nets, err := s.Scan(ctx)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Networks: nets}

	case OpEnterSetup:
		if err := s.EnterSetup(ctx); err != nil {
			return errorResponse(err)
		}
		st := s.Status(ctx)
		return Response{OK: true, Status: &st}

	case OpExitSetup:
		if err := s.ExitSetup(ctx); err != nil {
			return errorResponse(err)
		}
		st := s.Status(ctx)
		return Response{OK: true, Status: &st}

	case OpConnect:
		result, err := s.Connect(ctx, req.SSID, req.Passphrase, req.Hidden)
		if err != nil {
			resp := errorResponse(err)
			resp.Connect = &result
			return resp
		}
		return Response{OK: true, Connect: &result}

	default:
		return errorResponse(fmt.Errorf("wifi: unhandled operation %q", req.Op))
	}
}

// Status reports the current network state.
func (s *Server) Status(ctx context.Context) Status {
	s.mu.Lock()
	st := Status{
		Mode:           s.mode,
		Interface:      s.cfg.Interface,
		Hostname:       hostnameOrEmpty(),
		SetupSince:     s.setupSince,
		SetupExpiresAt: s.setupExpires,
		LastError:      s.lastErr,
	}
	if s.mode == ModeSetup {
		st.SetupSSID = s.cfg.SetupSSID
	}
	s.mu.Unlock()

	out, err := s.runner.Run(ctx, "nmcli", DeviceShowArgs(s.cfg.Interface)...)
	if err != nil {
		st.Mode = ModeUnknown
		if st.LastError == "" {
			st.LastError = err.Error()
		}
		return st
	}
	dev := ParseDeviceShow(string(out))
	st.Connected = dev.Connected
	st.IPv4 = dev.IPv4
	if dev.Connection != SetupConnectionName {
		st.SSID = dev.Connection
	}
	return st
}

// Scan lists visible networks.
func (s *Server) Scan(ctx context.Context) ([]Network, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ScanTimeout.Duration)
	defer cancel()

	out, err := s.runner.Run(ctx, "nmcli", ScanArgs(s.cfg.Interface)...)
	if err != nil {
		s.setError(err.Error())
		return nil, fmt.Errorf("wifi: scanning for networks: %w", err)
	}
	nets := ParseScan(string(out))
	s.log.Info("wifi scan complete", "networks", len(nets))
	return nets, nil
}

// EnterSetup raises the access point and starts the captive portal.
func (s *Server) EnterSetup(ctx context.Context) error {
	s.mu.Lock()
	if s.mode == ModeSetup {
		s.mu.Unlock()
		s.log.Info("already in Wi-Fi setup mode")
		return nil
	}
	s.mu.Unlock()

	s.log.Info("entering Wi-Fi setup mode",
		"ssid", s.cfg.SetupSSID, "interface", s.cfg.Interface,
		"address", s.cfg.SetupAddress, "secured", s.cfg.SetupPassphrase != "")

	// Remember the current network so a failed attempt can be rolled back.
	previous := ""
	if out, err := s.runner.Run(ctx, "nmcli", ActiveConnectionArgs()...); err == nil {
		if name, ok := WirelessConnectionFor(ParseActiveConnections(string(out)), s.cfg.Interface); ok {
			previous = name
			s.log.Info("remembering the current network for rollback", "connection", previous)
		}
	}

	// The delete step is expected to fail when there is nothing to clean up.
	cmds := APUpArgs(s.cfg.Interface, s.cfg.SetupSSID, s.cfg.SetupPassphrase, s.cfg.SetupAddress)
	for i, args := range cmds {
		if _, err := s.runner.Run(ctx, "nmcli", args...); err != nil {
			if i == 0 {
				continue // no stale profile to delete
			}
			s.setError(err.Error())
			// Leave the interface in a usable state rather than half-configured.
			s.teardownAP(ctx)
			return fmt.Errorf("wifi: raising the setup access point: %w", err)
		}
	}

	now := s.clock.Now()
	setupCtx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	s.mode = ModeSetup
	s.setupSince = now
	s.setupExpires = now.Add(s.cfg.SetupTimeout.Duration)
	s.previousConnection = previous
	s.lastErr = ""
	s.setupCancel = cancel
	s.mu.Unlock()

	if err := s.portal.Start(setupCtx); err != nil {
		s.log.Error("could not start the captive portal; the access point is up but unusable", "error", err)
	}

	// Setup mode ends by itself so a forgotten access point does not stay up.
	go s.expireSetup(setupCtx, s.cfg.SetupTimeout.Duration)

	s.log.Info("Wi-Fi setup mode is active",
		"ssid", s.cfg.SetupSSID, "portal", "http://"+addressHost(s.cfg.SetupAddress),
		"expires_at", s.setupExpires.Format(time.RFC3339))
	return nil
}

func (s *Server) expireSetup(ctx context.Context, d time.Duration) {
	t := s.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C():
	}
	s.log.Warn("Wi-Fi setup mode timed out; returning to normal networking", "after", d)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.ExitSetup(shutdownCtx); err != nil {
		s.log.Error("could not leave setup mode after the timeout", "error", err)
	}
}

// ExitSetup tears the access point down and restores normal networking.
func (s *Server) ExitSetup(ctx context.Context) error {
	s.mu.Lock()
	if s.mode != ModeSetup {
		s.mu.Unlock()
		return nil
	}
	cancel := s.setupCancel
	previous := s.previousConnection
	s.mode = ModeNormal
	s.setupSince = time.Time{}
	s.setupExpires = time.Time{}
	s.setupCancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.portal.Stop()
	s.teardownAP(ctx)

	// Put the interface back to automatic behaviour, preferring the network that
	// was active before setup mode.
	if previous != "" {
		if _, err := s.runner.Run(ctx, "nmcli", "connection", "up", previous); err == nil {
			s.log.Info("restored the previous network", "connection", previous)
			return nil
		}
		s.log.Warn("could not restore the previous network; letting NetworkManager choose",
			"connection", previous)
	}
	if _, err := s.runner.Run(ctx, "nmcli", ReconnectArgs(s.cfg.Interface)...); err != nil {
		s.log.Warn("could not reconnect the wireless interface", "error", err)
	}
	s.log.Info("left Wi-Fi setup mode")
	return nil
}

func (s *Server) teardownAP(ctx context.Context) {
	for _, args := range APDownArgs() {
		// Both steps routinely fail when the profile is already gone.
		if _, err := s.runner.Run(ctx, "nmcli", args...); err != nil {
			s.log.Debug("access point teardown step failed", "args", args, "error", err)
		}
	}
}

// Connect joins a network, validating it before committing.
//
// The ordering here is the part that matters: nmcli creates a profile for the
// new SSID without touching the existing one, so if validation fails the old
// credentials are still on disk and we simply bring them back up. A failed
// attempt can therefore never strand the device on a network it cannot reach.
func (s *Server) Connect(ctx context.Context, ssid, passphrase string, hidden bool) (ConnectResult, error) {
	if err := ValidateCredentials(ssid, passphrase); err != nil {
		return ConnectResult{Success: false, SSID: ssid, Message: err.Error()}, err
	}

	s.mu.Lock()
	previous := s.previousConnection
	wasSetup := s.mode == ModeSetup
	s.mu.Unlock()

	s.log.Info("attempting to join a network", "ssid", ssid, "secured", passphrase != "", "hidden", hidden)

	// The access point and a client connection cannot share one radio, so setup
	// mode must come down before the attempt. The portal has already delivered
	// its "connecting…" page by this point.
	if wasSetup {
		s.portal.Stop()
		s.teardownAP(ctx)
	}

	connectCtx, cancel := context.WithTimeout(ctx, s.cfg.ConnectTimeout.Duration)
	defer cancel()

	_, err := s.runner.Run(connectCtx, "nmcli", ConnectArgs(s.cfg.Interface, ssid, passphrase, hidden)...)
	if err != nil {
		msg := friendlyConnectError(err)
		s.log.Warn("could not join the network", "ssid", ssid, "error", err)
		result := ConnectResult{Success: false, SSID: ssid, Message: msg}
		result.RolledBack = s.rollback(ctx, previous, wasSetup)
		s.setError(msg)
		return result, errors.New(msg)
	}

	// Connecting is not the same as working: wait for an actual IPv4 address
	// before declaring success and discarding setup mode.
	state, ok := s.waitForAddress(ctx, s.cfg.ValidateTimeout.Duration)
	if !ok {
		msg := "Joined the network but no IP address was assigned. Check the router's DHCP settings."
		s.log.Warn("network joined but validation failed", "ssid", ssid, "state", state.State)
		result := ConnectResult{Success: false, SSID: ssid, Message: msg}
		result.RolledBack = s.rollback(ctx, previous, wasSetup)
		s.setError(msg)
		return result, errors.New(msg)
	}

	s.mu.Lock()
	s.mode = ModeNormal
	s.setupSince = time.Time{}
	s.setupExpires = time.Time{}
	s.previousConnection = ""
	s.lastErr = ""
	cancelSetup := s.setupCancel
	s.setupCancel = nil
	s.mu.Unlock()
	if cancelSetup != nil {
		cancelSetup()
	}

	s.log.Info("joined network successfully", "ssid", ssid, "ipv4", state.IPv4)
	return ConnectResult{Success: true, SSID: ssid, IPv4: state.IPv4}, nil
}

// rollback restores the previous network, or setup mode, after a failure. It
// reports whether the previous network was restored.
func (s *Server) rollback(ctx context.Context, previous string, wasSetup bool) bool {
	restored := false
	if previous != "" {
		if _, err := s.runner.Run(ctx, "nmcli", "connection", "up", previous); err == nil {
			s.log.Info("rolled back to the previous network", "connection", previous)
			restored = true
		} else {
			s.log.Warn("could not roll back to the previous network",
				"connection", previous, "error", err)
		}
	}
	if wasSetup && !restored {
		// Keep setup mode available so the user can try a different password
		// without having to hold the button again.
		s.mu.Lock()
		s.mode = ModeNormal // EnterSetup returns early otherwise
		s.mu.Unlock()
		if err := s.EnterSetup(ctx); err != nil {
			s.log.Error("could not restore setup mode after a failed attempt", "error", err)
		}
	}
	return restored
}

// waitForAddress polls until the interface has an IPv4 address or the timeout
// elapses.
func (s *Server) waitForAddress(ctx context.Context, timeout time.Duration) (DeviceState, bool) {
	deadline := s.clock.Now().Add(timeout)
	var last DeviceState

	for {
		out, err := s.runner.Run(ctx, "nmcli", DeviceShowArgs(s.cfg.Interface)...)
		if err == nil {
			last = ParseDeviceShow(string(out))
			if last.Connected && last.IPv4 != "" {
				return last, true
			}
		}
		if !s.clock.Now().Before(deadline) {
			return last, false
		}
		t := s.clock.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return last, false
		case <-t.C():
		}
	}
}

func (s *Server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.ExitSetup(ctx); err != nil {
		s.log.Warn("could not leave setup mode during shutdown", "error", err)
	}
}

func (s *Server) setError(msg string) {
	s.mu.Lock()
	s.lastErr = msg
	s.mu.Unlock()
}

func (s *Server) maxOperationTime() time.Duration {
	// The longest operation is a connection attempt plus its validation window.
	d := s.cfg.ConnectTimeout.Duration + s.cfg.ValidateTimeout.Duration + 30*time.Second
	if scan := s.cfg.ScanTimeout.Duration + 10*time.Second; scan > d {
		d = scan
	}
	return d
}

// friendlyConnectError turns nmcli's output into something a person standing in
// front of the device can act on.
func friendlyConnectError(err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "secrets were required") ||
		strings.Contains(lower, "invalid password") ||
		strings.Contains(lower, "802-11-wireless-security"):
		return "The password was not accepted. Check it and try again."
	case strings.Contains(lower, "no network with ssid") ||
		strings.Contains(lower, "not found"):
		return "That network could not be found. Move closer, or check that it is broadcasting."
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "The connection attempt timed out. The network may be out of range."
	default:
		return "Could not join the network: " + firstLine(msg)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// addressHost strips the prefix length from a CIDR address.
func addressHost(cidr string) string {
	if host, _, ok := strings.Cut(cidr, "/"); ok {
		return host
	}
	return cidr
}

func hostnameOrEmpty() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if !strings.HasSuffix(h, ".local") {
		h += ".local"
	}
	return h
}
