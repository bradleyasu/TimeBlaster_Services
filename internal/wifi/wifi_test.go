package wifi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/system"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Real `nmcli -t -f SSID,SIGNAL,SECURITY,IN-USE device wifi list` output,
// including a duplicate SSID from a mesh network, a hidden network and an SSID
// containing an escaped colon.
const sampleScan = `Home Network:82:WPA2:*
Home Network:54:WPA2:
:31:WPA2:
Neighbour:41:WPA1 WPA2:
CoffeeShop:22:--:
Weird\:Name:15:WPA2:
`

func TestParseScan(t *testing.T) {
	nets := ParseScan(sampleScan)

	if len(nets) != 4 {
		t.Fatalf("got %d networks: %+v", len(nets), nets)
	}
	// Strongest first.
	if nets[0].SSID != "Home Network" || nets[0].Signal != 82 {
		t.Errorf("first: %+v", nets[0])
	}
	if !nets[0].InUse {
		t.Error("the in-use marker was lost when collapsing duplicates")
	}
	// A hidden network with no SSID is not actionable and must be dropped.
	for _, n := range nets {
		if n.SSID == "" {
			t.Error("an empty SSID survived")
		}
	}
	// An escaped colon inside an SSID must not split the field.
	var found bool
	for _, n := range nets {
		if n.SSID == "Weird:Name" {
			found = true
		}
	}
	if !found {
		t.Errorf("escaped colon mishandled: %+v", nets)
	}
	// An open network reports no security.
	for _, n := range nets {
		if n.SSID == "CoffeeShop" {
			if n.Secured() {
				t.Error("an open network should not be marked secured")
			}
		}
		if n.SSID == "Neighbour" && !n.Secured() {
			t.Error("a WPA network should be marked secured")
		}
	}
}

func TestParseScanEmpty(t *testing.T) {
	if got := ParseScan(""); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
	if got := ParseScan("\n\n  \n"); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestParseDeviceShow(t *testing.T) {
	const connected = `GENERAL.STATE:100 (connected)
GENERAL.CONNECTION:Home Network
IP4.ADDRESS[1]:192.168.1.42/24
IP4.ADDRESS[2]:192.168.1.99/24
`
	st := ParseDeviceShow(connected)
	if !st.Connected {
		t.Error("should be connected")
	}
	if st.Connection != "Home Network" {
		t.Errorf("connection: %q", st.Connection)
	}
	if st.IPv4 != "192.168.1.42/24" {
		t.Errorf("the first address should win: %q", st.IPv4)
	}

	const disconnected = `GENERAL.STATE:30 (disconnected)
GENERAL.CONNECTION:--
IP4.ADDRESS:--
`
	st = ParseDeviceShow(disconnected)
	if st.Connected || st.Connection != "" || st.IPv4 != "" {
		t.Errorf("disconnected: %+v", st)
	}

	// A transitional state must not read as connected: that is exactly the case
	// where we would wrongly declare a Wi-Fi change successful.
	st = ParseDeviceShow("GENERAL.STATE:70 (config)\nGENERAL.CONNECTION:New\n")
	if st.Connected {
		t.Error("state 70 must not count as connected")
	}
}

func TestParseActiveConnectionsAndRollbackTarget(t *testing.T) {
	const out = `Home Network:wlan0:802-11-wireless
Wired connection 1:eth0:802-3-ethernet
timeblaster-setup:wlan0:802-11-wireless
`
	conns := ParseActiveConnections(out)
	if len(conns) != 3 {
		t.Fatalf("got %+v", conns)
	}

	name, ok := WirelessConnectionFor(conns, "wlan0")
	if !ok || name != "Home Network" {
		t.Errorf("rollback target: %q, %v", name, ok)
	}
	// Our own access point must never be chosen as the network to go back to.
	if name == SetupConnectionName {
		t.Error("the setup profile was chosen as the rollback target")
	}
	if _, ok := WirelessConnectionFor(conns, "wlan9"); ok {
		t.Error("unknown interface should have no rollback target")
	}
}

func TestCommandBuilders(t *testing.T) {
	if got := ScanArgs("wlan0"); !contains(got, "--rescan") || !contains(got, "wlan0") {
		t.Errorf("ScanArgs: %v", got)
	}

	got := ConnectArgs("wlan0", "Home", "hunter22", false)
	want := []string{"device", "wifi", "connect", "Home", "ifname", "wlan0", "password", "hunter22"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ConnectArgs:\n got %v\nwant %v", got, want)
	}

	open := ConnectArgs("wlan0", "CoffeeShop", "", false)
	if contains(open, "password") {
		t.Errorf("an open network must not pass a password: %v", open)
	}
	if hidden := ConnectArgs("wlan0", "Secret", "", true); !contains(hidden, "hidden") {
		t.Errorf("hidden flag missing: %v", hidden)
	}

	cmds := APUpArgs("wlan0", "TIMEBLASTER-SETUP", "", "10.42.0.1/24")
	joined := flatten(cmds)
	for _, want := range []string{"ipv4.method", "shared", "802-11-wireless.mode", "ap", "10.42.0.1/24", "autoconnect", "no"} {
		if !strings.Contains(joined, want) {
			t.Errorf("APUpArgs is missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "wifi-sec") {
		t.Errorf("an open access point must not set security: %s", joined)
	}
	secured := flatten(APUpArgs("wlan0", "TB", "supersecret", "10.42.0.1/24"))
	if !strings.Contains(secured, "wpa-psk") || !strings.Contains(secured, "supersecret") {
		t.Errorf("a secured access point must set WPA: %s", secured)
	}
}

func TestValidateCredentials(t *testing.T) {
	if err := ValidateCredentials("Home", "hunter22"); err != nil {
		t.Errorf("valid credentials rejected: %v", err)
	}
	if err := ValidateCredentials("CoffeeShop", ""); err != nil {
		t.Errorf("an open network should be allowed: %v", err)
	}

	tests := []struct {
		name, ssid, psk string
	}{
		{"empty ssid", "", "hunter22"},
		{"ssid too long", strings.Repeat("x", 33), ""},
		{"password too short", "Home", "short"},
		{"password too long", "Home", strings.Repeat("x", 64)},
		// These are the ones that matter: the values reach an nmcli command line
		// from a form on an open access point.
		{"newline in ssid", "Home\nrm -rf /", ""},
		{"nul in ssid", "Home\x00", ""},
		{"newline in password", "Home", "pass\nword1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCredentials(tc.ssid, tc.psk); err == nil {
				t.Errorf("%s should be rejected", tc.name)
			}
		})
	}
}

func TestRequestValidate(t *testing.T) {
	if err := (Request{Version: ProtocolVersion, Op: OpStatus}).Validate(); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
	if err := (Request{Version: 99, Op: OpStatus}).Validate(); err == nil {
		t.Error("a version mismatch must be rejected")
	}
	if err := (Request{Version: ProtocolVersion, Op: "reboot"}).Validate(); err == nil {
		t.Error("an unknown operation must be rejected")
	}
	if err := (Request{Version: ProtocolVersion, Op: OpConnect, SSID: ""}).Validate(); err == nil {
		t.Error("connect without an SSID must be rejected")
	}
}

func TestRequestRedactedHidesPasswords(t *testing.T) {
	r := Request{Op: OpConnect, SSID: "Home", Passphrase: "hunter22"}.Redacted()
	if strings.Contains(r.Passphrase, "hunter") {
		t.Fatalf("the password survived redaction: %+v", r)
	}
	if r.SSID != "Home" {
		t.Errorf("the SSID should be preserved: %+v", r)
	}
}

func TestFriendlyConnectError(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Error: Connection activation failed: (7) Secrets were required", "password"},
		{"Error: No network with SSID 'Nope' found.", "could not be found"},
		{"context deadline exceeded", "timed out"},
	} {
		got := friendlyConnectError(errors.New(tc.in))
		if !strings.Contains(strings.ToLower(got), tc.want) {
			t.Errorf("friendlyConnectError(%q) = %q, want it to mention %q", tc.in, got, tc.want)
		}
	}
}

func TestAddressHost(t *testing.T) {
	if got := addressHost("10.42.0.1/24"); got != "10.42.0.1" {
		t.Errorf("got %q", got)
	}
	if got := addressHost("10.42.0.1"); got != "10.42.0.1" {
		t.Errorf("got %q", got)
	}
}

// --- server tests ------------------------------------------------------------

func newTestServer(t *testing.T) (*Server, *system.FakeRunner, *system.FakeClock) {
	t.Helper()
	cfg := config.Default().WiFi
	// A path short enough for the platform's unix-socket limit.
	dir, err := os.MkdirTemp("", "tbwifi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg.HelperSocket = filepath.Join(dir, "w.sock")

	runner := system.NewFakeRunner()
	clk := system.NewFakeClock(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC))
	return NewServer(cfg, runner, clk, testLogger()), runner, clk
}

func TestServerEnterAndExitSetup(t *testing.T) {
	s, runner, _ := newTestServer(t)
	ctx := context.Background()

	runner.OnRun = func(c system.RecordedCommand) ([]byte, error) {
		if contains(c.Args, "--active") {
			return []byte("Home Network:wlan0:802-11-wireless\n"), nil
		}
		return nil, nil
	}

	if err := s.EnterSetup(ctx); err != nil {
		t.Fatalf("EnterSetup: %v", err)
	}
	st := s.Status(ctx)
	if st.Mode != ModeSetup || st.SetupSSID != "TIMEBLASTER-SETUP" {
		t.Errorf("status: %+v", st)
	}
	if st.SetupExpiresAt.IsZero() {
		t.Error("setup mode must have an expiry so a forgotten access point does not stay up")
	}

	joined := flattenCalls(runner.Calls())
	for _, want := range []string{"802-11-wireless.mode ap", "ipv4.method shared", "connection up " + SetupConnectionName} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}

	// Entering twice is idempotent.
	before := len(runner.Calls())
	if err := s.EnterSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls()) != before {
		t.Error("a second EnterSetup should be a no-op")
	}

	runner.Reset()
	if err := s.ExitSetup(ctx); err != nil {
		t.Fatalf("ExitSetup: %v", err)
	}
	if s.Status(ctx).Mode == ModeSetup {
		t.Error("still in setup mode")
	}
	joined = flattenCalls(runner.Calls())
	if !strings.Contains(joined, "connection down "+SetupConnectionName) {
		t.Errorf("the access point was not torn down:\n%s", joined)
	}
	// The previous network must be restored.
	if !strings.Contains(joined, "connection up Home Network") {
		t.Errorf("the previous network was not restored:\n%s", joined)
	}
	if s.portal.Running() {
		t.Error("the captive portal is still running")
	}
}

func TestServerEnterSetupCleansUpOnFailure(t *testing.T) {
	s, runner, _ := newTestServer(t)
	runner.OnRun = func(c system.RecordedCommand) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "connection" && c.Args[1] == "up" {
			return nil, errors.New("Error: Connection activation failed")
		}
		return nil, nil
	}
	if err := s.EnterSetup(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if s.Status(context.Background()).Mode == ModeSetup {
		t.Error("a failed EnterSetup must not leave the device in setup mode")
	}
}

func TestServerConnectSuccess(t *testing.T) {
	s, runner, _ := newTestServer(t)
	ctx := context.Background()

	runner.OnRun = func(c system.RecordedCommand) ([]byte, error) {
		if isDeviceShow(c) {
			return []byte("GENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:Home\nIP4.ADDRESS[1]:192.168.1.42/24\n"), nil
		}
		return nil, nil
	}

	result, err := s.Connect(ctx, "Home", "hunter22", false)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !result.Success || result.IPv4 != "192.168.1.42/24" {
		t.Fatalf("result: %+v", result)
	}

	joined := flattenCalls(runner.Calls())
	if !strings.Contains(joined, "device wifi connect Home ifname wlan0 password hunter22") {
		t.Errorf("connect command:\n%s", joined)
	}
	if s.Status(ctx).Mode == ModeSetup {
		t.Error("a successful connection should leave setup mode")
	}
}

func TestServerConnectRollsBackOnWrongPassword(t *testing.T) {
	s, runner, _ := newTestServer(t)
	ctx := context.Background()

	runner.OnRun = func(c system.RecordedCommand) ([]byte, error) {
		switch {
		case contains(c.Args, "--active"):
			return []byte("Home Network:wlan0:802-11-wireless\n"), nil
		case len(c.Args) >= 3 && c.Args[0] == "device" && c.Args[1] == "wifi" && c.Args[2] == "connect":
			return nil, errors.New("Error: Connection activation failed: (7) Secrets were required")
		}
		return nil, nil
	}

	if err := s.EnterSetup(ctx); err != nil {
		t.Fatal(err)
	}
	runner.Reset()

	result, err := s.Connect(ctx, "Neighbour", "wrongpassword", false)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if result.Success {
		t.Fatal("result should not report success")
	}
	if !strings.Contains(strings.ToLower(result.Message), "password") {
		t.Errorf("message should be actionable: %q", result.Message)
	}
	if !result.RolledBack {
		t.Error("the previous network should have been restored")
	}

	joined := flattenCalls(runner.Calls())
	if !strings.Contains(joined, "connection up Home Network") {
		t.Errorf("no rollback attempt:\n%s", joined)
	}
	// Crucially, nothing deleted the known-good profile.
	if strings.Contains(joined, "connection delete Home Network") {
		t.Errorf("the known-good network was deleted:\n%s", joined)
	}
}

func TestServerConnectFailsValidationWithoutAnAddress(t *testing.T) {
	// nmcli reports success but DHCP never completes: this must not count as a
	// working network, or the user is left with an unreachable Timeblaster.
	s, runner, clk := newTestServer(t)
	ctx := context.Background()

	runner.OnRun = func(c system.RecordedCommand) ([]byte, error) {
		if isDeviceShow(c) {
			return []byte("GENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:Home\nIP4.ADDRESS:--\n"), nil
		}
		return nil, nil
	}

	done := make(chan ConnectResult, 1)
	go func() {
		r, _ := s.Connect(ctx, "Home", "hunter22", false)
		done <- r
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if clk.Waiters() > 0 {
			clk.Advance(2 * time.Minute)
		}
		select {
		case r := <-done:
			if r.Success {
				t.Fatal("a connection with no IP address must not count as success")
			}
			if !strings.Contains(r.Message, "IP address") {
				t.Errorf("message: %q", r.Message)
			}
			return
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Connect did not return")
}

func TestServerConnectRejectsInvalidCredentials(t *testing.T) {
	s, runner, _ := newTestServer(t)
	if _, err := s.Connect(context.Background(), "Home\nevil", "hunter22", false); err == nil {
		t.Fatal("expected a validation failure")
	}
	if len(runner.Calls()) != 0 {
		t.Errorf("an invalid request reached nmcli: %+v", runner.Calls())
	}
}

func TestServerScan(t *testing.T) {
	s, runner, _ := newTestServer(t)
	runner.Outputs["nmcli"] = []byte(sampleScan)

	nets, err := s.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(nets) != 4 {
		t.Errorf("networks: %+v", nets)
	}

	runner.Outputs["nmcli"] = nil
	runner.Errors["nmcli"] = errors.New("device not ready")
	if _, err := s.Scan(context.Background()); err == nil {
		t.Error("expected an error")
	}
}

// --- client/server integration over a real socket ----------------------------

func TestClientServerRoundTrip(t *testing.T) {
	s, runner, _ := newTestServer(t)
	runner.Outputs["nmcli"] = []byte(sampleScan)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Listen(ctx) }()

	c := NewClient(s.cfg.HelperSocket, 5*time.Second, testLogger())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.Ping(context.Background()); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("helper never became reachable: %v", err)
	}
	if !c.Available() {
		t.Error("Available should be true after a successful call")
	}

	nets, err := c.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(nets) != 4 {
		t.Errorf("networks: %+v", nets)
	}

	if _, err := c.Status(context.Background()); err != nil {
		t.Errorf("Status: %v", err)
	}
	// Invalid credentials are rejected client-side, before they cross the socket.
	if _, err := c.Connect(context.Background(), "", "x", false); err == nil {
		t.Error("expected a validation error")
	}
}

func TestClientReportsAnAbsentHelper(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "absent.sock"), time.Second, testLogger())
	_, err := c.Status(context.Background())
	if !errors.Is(err, ErrHelperUnavailable) {
		t.Fatalf("got %v, want ErrHelperUnavailable", err)
	}
	if c.Available() {
		t.Error("Available should be false")
	}
	if c.LastError() == "" {
		t.Error("LastError should be populated")
	}
}

// --- captive portal ----------------------------------------------------------

type stubOps struct {
	nets       []Network
	connectErr error
	lastSSID   string
	lastPSK    string
}

func (s *stubOps) Scan(context.Context) ([]Network, error) {
	if s.nets == nil {
		return nil, errors.New("scan failed")
	}
	return s.nets, nil
}

func (s *stubOps) Connect(_ context.Context, ssid, psk string, _ bool) (ConnectResult, error) {
	s.lastSSID, s.lastPSK = ssid, psk
	if s.connectErr != nil {
		return ConnectResult{Success: false, SSID: ssid, Message: s.connectErr.Error(), RolledBack: true}, s.connectErr
	}
	return ConnectResult{Success: true, SSID: ssid, IPv4: "192.168.1.42/24"}, nil
}

func (s *stubOps) Status(context.Context) Status {
	return Status{Mode: ModeSetup, Interface: "wlan0"}
}

func newPortalHandler(ops portalOps) http.Handler {
	p := NewPortal(config.Default().WiFi, ops, testLogger())
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handleRoot)
	mux.HandleFunc("/api/networks", p.handleNetworks)
	mux.HandleFunc("/api/connect", p.handleConnect)
	mux.HandleFunc("/api/status", p.handleStatus)
	mux.HandleFunc("/generate_204", p.handleCaptiveProbe)
	return securityHeaders(mux)
}

func TestPortalServesTheSetupPage(t *testing.T) {
	h := newPortalHandler(&stubOps{nets: ParseScan(sampleScan)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"TIMEBLASTER", "TIMEBLASTER-SETUP", "timeblaster.local", "/api/connect"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// A phone on the setup network has no internet, so nothing may be external.
	for _, forbidden := range []string{"http://cdn", "https://", "<link rel=\"stylesheet\" href=\"http"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the page references an external resource: %q", forbidden)
		}
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("security headers missing: %q", got)
	}
}

func TestPortalCaptiveProbeRedirects(t *testing.T) {
	h := newPortalHandler(&stubOps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/generate_204", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status: %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "10.42.0.1") {
		t.Errorf("redirect target: %q", loc)
	}
}

func TestPortalListsNetworks(t *testing.T) {
	h := newPortalHandler(&stubOps{nets: ParseScan(sampleScan)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/networks", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var body struct{ Networks []Network }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Networks) != 4 {
		t.Errorf("networks: %+v", body.Networks)
	}
}

func TestPortalScanFailure(t *testing.T) {
	h := newPortalHandler(&stubOps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/networks", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d", rec.Code)
	}
}

func TestPortalConnect(t *testing.T) {
	ops := &stubOps{}
	h := newPortalHandler(ops)

	req := httptest.NewRequest(http.MethodPost, "/api/connect",
		strings.NewReader(`{"ssid":"Home","passphrase":"hunter22"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body %s", rec.Code, rec.Body)
	}
	if ops.lastSSID != "Home" || ops.lastPSK != "hunter22" {
		t.Errorf("credentials not passed through: %q/%q", ops.lastSSID, ops.lastPSK)
	}
	var result ConnectResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("result: %+v", result)
	}
}

func TestPortalConnectRejectsBadInput(t *testing.T) {
	h := newPortalHandler(&stubOps{})
	tests := []struct {
		name, body string
		wantStatus int
	}{
		{"malformed json", `{`, http.StatusBadRequest},
		{"unknown field", `{"ssid":"Home","command":"rm -rf /"}`, http.StatusBadRequest},
		{"empty ssid", `{"ssid":""}`, http.StatusBadRequest},
		{"control characters", `{"ssid":"Home\nevil"}`, http.StatusBadRequest},
		{"short password", `{"ssid":"Home","passphrase":"abc"}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(tc.body)))
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
		})
	}
}

func TestPortalConnectMethodNotAllowed(t *testing.T) {
	h := newPortalHandler(&stubOps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/connect", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: %d", rec.Code)
	}
}

func TestPortalConnectFailureIsReported(t *testing.T) {
	ops := &stubOps{connectErr: errors.New("The password was not accepted.")}
	h := newPortalHandler(ops)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/connect",
		strings.NewReader(`{"ssid":"Home","passphrase":"wrongpass"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: %d", rec.Code)
	}
	var result ConnectResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success || !result.RolledBack {
		t.Errorf("result: %+v", result)
	}
}

func TestFakeManager(t *testing.T) {
	f := NewFakeManager()
	ctx := context.Background()

	if st, err := f.Status(ctx); err != nil || st.Mode != ModeNormal {
		t.Fatalf("status: %+v, %v", st, err)
	}
	if err := f.EnterSetupMode(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Status(ctx); st.Mode != ModeSetup {
		t.Errorf("mode: %v", st.Mode)
	}

	f.SetConnectError("nope")
	if r, err := f.Connect(ctx, "Home", "hunter22", false); err == nil || r.Success {
		t.Errorf("expected a failure: %+v, %v", r, err)
	}
	f.SetConnectError("")
	if r, err := f.Connect(ctx, "Home", "hunter22", false); err != nil || !r.Success {
		t.Errorf("expected success: %+v, %v", r, err)
	}
	if st, _ := f.Status(ctx); st.Mode != ModeNormal {
		t.Error("a successful connection should leave setup mode")
	}
	if !f.Available() {
		t.Error("should be available")
	}
	if len(f.Calls()) == 0 {
		t.Error("calls not recorded")
	}
}

// isDeviceShow recognises `nmcli -t -f ... device show <iface>`.
func isDeviceShow(c system.RecordedCommand) bool {
	return contains(c.Args, "device") && contains(c.Args, "show")
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func flatten(cmds [][]string) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(strings.Join(c, " "))
		b.WriteString("\n")
	}
	return b.String()
}

func flattenCalls(calls []system.RecordedCommand) string {
	var b strings.Builder
	for _, c := range calls {
		b.WriteString(c.String())
		b.WriteString("\n")
	}
	return b.String()
}
