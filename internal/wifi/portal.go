package wifi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
)

// portalOps is what the portal needs from the helper. Declaring it as an
// interface rather than taking *Server keeps the portal testable and makes the
// (deliberately tiny) surface it can reach explicit.
type portalOps interface {
	Scan(ctx context.Context) ([]Network, error)
	Connect(ctx context.Context, ssid, passphrase string, hidden bool) (ConnectResult, error)
	Status(ctx context.Context) Status
}

// Portal is the captive-portal web server shown while setup mode is active.
//
// It exists only between entering and leaving setup mode, is served by the
// privileged helper (which can bind port 80), and exposes exactly three
// operations: list networks, join one, and report status. There is deliberately
// no way to reach a shell, a file, or any other part of the system from it.
type Portal struct {
	cfg config.WiFi
	ops portalOps
	log *slog.Logger

	// port is always "80" in production: the captive-portal probe URLs phones
	// use are only consulted on port 80, so the page cannot pop up by itself
	// anywhere else. Tests override it, because binding 80 needs root.
	port string

	mu   sync.Mutex
	srv  *http.Server
	done chan struct{}
}

// NewPortal creates the portal.
func NewPortal(cfg config.WiFi, ops portalOps, log *slog.Logger) *Portal {
	return &Portal{cfg: cfg, ops: ops, log: log, port: "80"}
}

// Start begins serving. It binds port 80 on the setup address so that a phone
// joining the access point is taken straight to the page.
func (p *Portal) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.srv != nil {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handleRoot)
	mux.HandleFunc("/api/networks", p.handleNetworks)
	mux.HandleFunc("/api/connect", p.handleConnect)
	mux.HandleFunc("/api/status", p.handleStatus)
	// The probe URLs iOS, Android and Windows use to detect a captive portal.
	// Answering them with a redirect is what makes the setup page pop up by
	// itself instead of the user having to find an IP address.
	for _, probe := range []string{
		"/hotspot-detect.html", "/library/test/success.html",
		"/generate_204", "/gen_204", "/connecttest.txt", "/ncsi.txt", "/redirect",
	} {
		mux.HandleFunc(probe, p.handleCaptiveProbe)
	}

	port := p.port
	if port == "" {
		port = "80"
	}
	addr := net.JoinHostPort(addressHost(p.cfg.SetupAddress), port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      2 * time.Minute, // a connection attempt is slow by nature
		IdleTimeout:       60 * time.Second,
		// The portal serves phones on an isolated network; a bounded header size
		// keeps a malformed request from costing anything.
		MaxHeaderBytes: 16 << 10,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("wifi: binding the captive portal to %s: %w", addr, err)
	}

	p.srv = srv
	p.done = make(chan struct{})
	done := p.done

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.log.Error("captive portal stopped unexpectedly", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		p.Stop()
	}()

	p.log.Info("captive portal started", "address", "http://"+addr)
	return nil
}

// Stop shuts the portal down. It is safe to call more than once.
func (p *Portal) Stop() {
	p.mu.Lock()
	srv, done := p.srv, p.done
	p.srv, p.done = nil, nil
	p.mu.Unlock()

	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	p.log.Info("captive portal stopped")
}

// Running reports whether the portal is serving.
func (p *Portal) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.srv != nil
}

func (p *Portal) handleCaptiveProbe(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "http://"+addressHost(p.cfg.SetupAddress)+"/", http.StatusFound)
}

func (p *Portal) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		p.handleCaptiveProbe(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(portalHTML(p.cfg.SetupSSID)))
}

func (p *Portal) handleNetworks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	nets, err := p.ops.Scan(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Could not scan for networks. Please try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": nets})
}

func (p *Portal) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.ops.Status(r.Context()))
}

func (p *Portal) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		SSID       string `json:"ssid"`
		Passphrase string `json:"passphrase"`
		Hidden     bool   `json:"hidden"`
	}
	// Bound the body: this endpoint is reachable by anyone within radio range of
	// the setup access point.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Malformed request.")
		return
	}
	if err := ValidateCredentials(body.SSID, body.Passphrase); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	p.log.Info("captive portal connection request", "ssid", body.SSID, "hidden", body.Hidden)

	result, err := p.ops.Connect(r.Context(), body.SSID, body.Passphrase, body.Hidden)
	status := http.StatusOK
	if err != nil {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, result)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// The page is entirely self-contained; forbidding external resources means
		// a phone with no internet access still renders it correctly.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// portalHTML is the setup page. It is a single self-contained document with no
// external resources, because a phone joined to the setup access point has no
// route to the internet and would otherwise render an unstyled, scriptless page.
func portalHTML(setupSSID string) string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Timeblaster Wi-Fi Setup</title>
<style>
  :root {
    --bg: #0b0f0b; --panel: #121812; --edge: #1f2a1f;
    --green: #33ff33; --dim: #7a9a7a; --text: #d8f5d8; --error: #ff6b6b;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 24px 16px 48px;
    background: var(--bg); color: var(--text);
    font: 16px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  }
  .wrap { max-width: 480px; margin: 0 auto; }
  h1 { color: var(--green); font-size: 22px; letter-spacing: .12em; margin: 0 0 4px; }
  .sub { color: var(--dim); font-size: 13px; margin: 0 0 24px; }
  .panel { background: var(--panel); border: 1px solid var(--edge); border-radius: 10px; padding: 16px; margin-bottom: 16px; }
  label { display: block; font-size: 13px; color: var(--dim); margin-bottom: 6px; letter-spacing: .06em; }
  select, input, button {
    width: 100%; font: inherit; padding: 12px;
    background: #0d130d; color: var(--text);
    border: 1px solid var(--edge); border-radius: 8px; margin-bottom: 14px;
  }
  button {
    background: var(--green); color: #061206; font-weight: 700;
    letter-spacing: .08em; border: none; margin-bottom: 0; cursor: pointer;
  }
  button[disabled] { opacity: .5; cursor: progress; }
  .row { display: flex; gap: 10px; align-items: center; margin-bottom: 14px; }
  .row input[type=checkbox] { width: auto; margin: 0; }
  .row label { margin: 0; }
  .msg { padding: 12px; border-radius: 8px; font-size: 14px; margin-top: 14px; display: none; }
  .msg.show { display: block; }
  .msg.ok { background: #0f2a0f; border: 1px solid var(--green); color: var(--green); }
  .msg.err { background: #2a0f0f; border: 1px solid var(--error); color: var(--error); }
  .msg.busy { background: #17210f; border: 1px solid var(--dim); color: var(--dim); }
  .hint { color: var(--dim); font-size: 12px; margin-top: 18px; }
</style>
</head>
<body>
<div class="wrap">
  <h1>TIMEBLASTER</h1>
  <p class="sub">Wi-Fi setup &middot; ` + html.EscapeString(setupSSID) + `</p>

  <div class="panel">
    <label for="ssid">Network</label>
    <select id="ssid"><option value="">Scanning&hellip;</option></select>

    <div class="row">
      <input type="checkbox" id="manual">
      <label for="manual">Enter a hidden network name</label>
    </div>
    <input type="text" id="ssid-manual" placeholder="Network name" style="display:none" autocapitalize="none" autocorrect="off">

    <label for="psk">Password</label>
    <input type="password" id="psk" placeholder="Leave blank for an open network" autocapitalize="none" autocorrect="off">

    <button id="go">CONNECT</button>
    <div id="msg" class="msg"></div>
  </div>

  <p class="hint">
    After a successful connection this access point disappears and the Timeblaster
    rejoins your home network. Open <strong>http://timeblaster.local</strong> from a
    device on that network to reach the companion app.
  </p>
</div>
<script>
(function () {
  var sel = document.getElementById('ssid');
  var manual = document.getElementById('manual');
  var manualInput = document.getElementById('ssid-manual');
  var psk = document.getElementById('psk');
  var go = document.getElementById('go');
  var msg = document.getElementById('msg');

  function show(kind, text) {
    msg.className = 'msg show ' + kind;
    msg.textContent = text;
  }

  manual.addEventListener('change', function () {
    manualInput.style.display = manual.checked ? 'block' : 'none';
    sel.style.display = manual.checked ? 'none' : 'block';
  });

  function scan() {
    fetch('/api/networks')
      .then(function (r) { return r.json(); })
      .then(function (d) {
        sel.innerHTML = '';
        var nets = d.networks || [];
        if (!nets.length) {
          sel.innerHTML = '<option value="">No networks found</option>';
          return;
        }
        nets.forEach(function (n) {
          var o = document.createElement('option');
          o.value = n.ssid;
          o.textContent = n.ssid + '  (' + (n.signal || 0) + '%)' + (n.security ? ' \u{1F512}' : '');
          sel.appendChild(o);
        });
      })
      .catch(function () {
        sel.innerHTML = '<option value="">Scan failed</option>';
      });
  }

  go.addEventListener('click', function () {
    var ssid = manual.checked ? manualInput.value.trim() : sel.value;
    if (!ssid) { show('err', 'Choose a network first.'); return; }

    go.disabled = true;
    show('busy', 'Connecting to "' + ssid + '". This can take up to a minute; this page will stop responding once the setup network shuts down.');

    fetch('/api/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ssid: ssid, passphrase: psk.value, hidden: manual.checked })
    })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d.success) {
          show('ok', 'Connected to ' + d.ssid + ' (' + (d.ipv4 || 'address pending') +
            '). Rejoin your home Wi-Fi, then open http://timeblaster.local');
        } else {
          go.disabled = false;
          show('err', (d.message || d.error || 'Connection failed.') +
            (d.rolled_back ? ' The previous network was restored.' : ''));
          scan();
        }
      })
      .catch(function () {
        show('ok', 'The setup network has shut down. If the Timeblaster joined your network, ' +
          'open http://timeblaster.local from a device on it. Otherwise hold the Wi-Fi button for five seconds again.');
      });
  });

  scan();
})();
</script>
</body>
</html>`
}

// ensure the helper satisfies what the portal needs.
var _ portalOps = (*Server)(nil)
