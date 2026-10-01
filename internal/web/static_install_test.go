package web

import (
	"regexp"
	"strings"
	"testing"
)

// The companion app refuses to run in a browser: opened as a web page it shows
// installation instructions and nothing else. That makes the gate the only way
// in, so a mistake in it does not degrade the app -- it removes it. These guards
// cover the ways that could happen silently.

var (
	// Every element the script looks up by id.
	idLookupRe = regexp.MustCompile(`(?:\$\(|getElementById\()'([a-zA-Z][-\w]*)'\)`)
	// Every id defined in the markup.
	idAttrRe = regexp.MustCompile(`id="([^"]+)"`)
)

func TestEveryElementTheScriptLooksUpExistsInTheMarkup(t *testing.T) {
	// A typo'd id is a runtime TypeError on a null element. In the gate that
	// means a blank screen with no way past it, so this is worth catching here
	// rather than on a phone.
	html := readStatic(t, "index.html")
	js := readStatic(t, "app.js")

	defined := map[string]bool{}
	for _, m := range idAttrRe.FindAllStringSubmatch(html, -1) {
		defined[m[1]] = true
	}
	if len(defined) == 0 {
		t.Fatal("found no ids in index.html; has the markup changed?")
	}

	var missing []string
	for _, m := range idLookupRe.FindAllStringSubmatch(js, -1) {
		if !defined[m[1]] {
			missing = append(missing, m[1])
		}
	}
	if len(missing) > 0 {
		t.Errorf("app.js looks up ids that index.html does not define: %v", missing)
	}
}

func TestInstallGateIsHiddenInTheMarkup(t *testing.T) {
	// It must not flash up for an installed app before the script runs.
	html := readStatic(t, "index.html")
	gate := regexp.MustCompile(`<section[^>]*id="install-gate"[^>]*>`).FindString(html)
	if gate == "" {
		t.Fatal("index.html has no install-gate section")
	}
	if !strings.Contains(gate, "hidden") {
		t.Errorf("the install gate is not hidden in the markup: %s", gate)
	}
}

func TestGatedBodyHidesEverythingElse(t *testing.T) {
	// "Instructions only" is the requirement. Hiding the panels but leaving the
	// tab bar or the connection dot would leave a half-app on screen.
	css := readStatic(t, "app.css")
	rule := regexp.MustCompile(`body\.gated[^{]*\{[^}]*display\s*:\s*none`).FindString(css)
	if rule == "" {
		t.Fatal("app.css has no body.gated rule hiding the app")
	}
	for _, sel := range []string{"main > *:not(#install-gate)", ".tabs", ".conn"} {
		if !strings.Contains(rule, sel) {
			t.Errorf("the body.gated rule does not cover %q: %s", sel, rule)
		}
	}
}

func TestGateDoesNotStartTheApp(t *testing.T) {
	// The app must not connect, poll, or call the API while gated. If start()
	// reached those directly, a browser would still be talking to the device.
	js := readStatic(t, "app.js")

	start := funcBody(t, js, "start")
	for _, forbidden := range []string{"connect()", "wire()", "setInterval", "refresh()"} {
		if strings.Contains(start, forbidden) {
			t.Errorf("start() calls %s directly; it must only branch to startApp or the gate", forbidden)
		}
	}
	if !strings.Contains(start, "isInstalled()") {
		t.Error("start() does not check isInstalled()")
	}
	if !strings.Contains(start, "showInstallGate()") {
		t.Error("start() never shows the gate")
	}

	// And the gate itself must not reach into the app.
	gate := funcBody(t, js, "showInstallGate")
	for _, forbidden := range []string{"connect()", "wire()", "startApp()"} {
		if strings.Contains(gate, forbidden) {
			t.Errorf("showInstallGate() calls %s, which starts the app it is meant to replace", forbidden)
		}
	}
}

func TestInstalledDetectionUsesBothMechanisms(t *testing.T) {
	// display-mode alone misses older iOS, and navigator.standalone alone misses
	// Android and desktop. Dropping either silently gates users who did install.
	js := funcBody(t, readStatic(t, "app.js"), "isInstalled")
	if !strings.Contains(js, "navigator.standalone") {
		t.Error("isInstalled does not check navigator.standalone, so an older iOS home-screen app is treated as a browser")
	}
	if !strings.Contains(js, "display-mode") {
		t.Error("isInstalled does not check display-mode, so an installed Android or desktop app is treated as a browser")
	}
}

func TestThereIsAWayPastAWrongDetection(t *testing.T) {
	// The gate is the only interface to the device. If detection is wrong on
	// some browser, without an override the Timeblaster becomes unreachable from
	// it, so the escape hatch is load-bearing rather than a convenience.
	js := readStatic(t, "app.js")
	// The assignment, not the string anywhere in the file: the first version of
	// this check matched the explanatory comment and passed with the override
	// broken.
	if !regexp.MustCompile(`BROWSER_OVERRIDE\s*=\s*'browser=1'`).MatchString(js) {
		t.Error("app.js has no ?browser=1 override; a detection bug would lock the user out entirely")
	}
	if !strings.Contains(funcBody(t, js, "start"), "overrideActive()") {
		t.Error("start() does not honour the override")
	}
}

func TestGateOffersAWayOutWhenInstallingIsImpossible(t *testing.T) {
	// Chromium will not install from a plain-HTTP origin, which is how the
	// Timeblaster is served. Showing steps that cannot work, with no way on,
	// would be a dead end.
	js := readStatic(t, "app.js")
	advice := funcBody(t, js, "installAdvice")
	if !strings.Contains(advice, "possible") {
		t.Fatal("installAdvice does not report whether installing is possible")
	}
	if !strings.Contains(advice, "isSecureContext") && !strings.Contains(funcBody(t, js, "describePlatform"), "isSecureContext") {
		t.Error("nothing checks isSecureContext, so Chromium over HTTP is told to install when it cannot")
	}
	gate := funcBody(t, js, "showInstallGate")
	if !strings.Contains(gate, "advice.possible") {
		t.Error("showInstallGate ignores whether installing is possible, so the continue button never appears")
	}
}

// funcBody returns the source of a top-level `function name(...) { ... }` by
// brace matching, so a test can assert on one function rather than the file.
func funcBody(t *testing.T, js, name string) string {
	t.Helper()
	re := regexp.MustCompile(`function\s+` + regexp.QuoteMeta(name) + `\s*\([^)]*\)\s*\{`)
	loc := re.FindStringIndex(js)
	if loc == nil {
		t.Fatalf("app.js has no function %s", name)
	}
	depth, start := 0, loc[1]-1
	for i := start; i < len(js); i++ {
		switch js[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[start : i+1]
			}
		}
	}
	t.Fatalf("function %s is not brace-balanced", name)
	return ""
}

func TestServiceWorkerPrecachesOnlyFilesThatExist(t *testing.T) {
	// caches.addAll rejects the whole install if any single entry 404s, so one
	// stale path stops the worker activating at all -- silently, because the
	// registration failure is caught and ignored by design.
	sw := readStatic(t, "sw.js")
	m := regexp.MustCompile(`(?s)var SHELL = \[(.*?)\]`).FindStringSubmatch(sw)
	if m == nil {
		t.Fatal("sw.js has no SHELL list")
	}

	paths := regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1)
	if len(paths) == 0 {
		t.Fatal("the SHELL list is empty; has sw.js changed?")
	}

	for _, p := range paths {
		name := p[1]
		if name == "./" {
			continue // the page itself, served by the router rather than a file
		}
		if _, err := embeddedStatic.ReadFile("static/" + name); err != nil {
			t.Errorf("sw.js precaches %q, which is not in static/: %v", name, err)
		}
	}
}

func TestMarkupAndManifestReferenceIconsThatExist(t *testing.T) {
	// A missing icon is a broken install prompt and a blank home-screen tile,
	// neither of which errors anywhere a user would see.
	for _, file := range []string{"index.html", "manifest.webmanifest"} {
		body := readStatic(t, file)
		refs := regexp.MustCompile(`assets/[-\w.]+\.(?:png|svg|webp)`).FindAllString(body, -1)
		if len(refs) == 0 {
			t.Errorf("%s references no icons at all", file)
			continue
		}
		for _, ref := range refs {
			if _, err := embeddedStatic.ReadFile("static/" + ref); err != nil {
				t.Errorf("%s references %q, which is not in static/", file, ref)
			}
		}
	}
}

// The offline overlay is what the app shows when the Timeblaster cannot be
// reached. It is the only screen a user sees in that state, so the ways it could
// fail -- never appearing, never leaving, or appearing over a working app -- are
// all invisible until someone is standing in the wrong place with a phone.

func TestOfflineOverlayIsHiddenInTheMarkup(t *testing.T) {
	html := readStatic(t, "index.html")
	tag := regexp.MustCompile(`<section[^>]*id="offline"[^>]*>`).FindString(html)
	if tag == "" {
		t.Fatal("index.html has no offline section")
	}
	if !strings.Contains(tag, "hidden") {
		t.Errorf("the offline overlay is not hidden in the markup, so it covers a working app: %s", tag)
	}
}

func TestOfflineOverlayCoversTheApp(t *testing.T) {
	// Every control underneath needs the device, so a half-covering overlay
	// would leave buttons reachable that can only fail.
	css := readStatic(t, "app.css")
	rule := regexp.MustCompile(`\.offline\s*\{[^}]*\}`).FindString(css)
	if rule == "" {
		t.Fatal("app.css has no .offline rule")
	}
	for _, want := range []string{"position", "fixed", "inset", "z-index"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".offline does not set %q, so it may not cover the app: %s", want, rule)
		}
	}
}

func TestConnectionLossArmsAndClearsTheOverlay(t *testing.T) {
	// setConnected is the single place the socket reports its state, so both
	// directions have to be handled there or the overlay sticks.
	js := readStatic(t, "app.js")
	body := funcBody(t, js, "setConnected")
	if !strings.Contains(body, "hideOffline()") {
		t.Error("setConnected never hides the overlay, so it would stay up after reconnecting")
	}
	if !strings.Contains(body, "scheduleOffline()") {
		t.Error("setConnected never arms the overlay, so losing the device shows nothing")
	}

	// A failed WebSocket constructor must report the loss too: onclose cannot
	// fire for a socket that never opened.
	//
	// Checked against the catch block rather than the whole of connect(): the
	// first version of this searched the function and matched the
	// setConnected(false) inside the onclose handler, so it passed with the
	// catch reporting nothing.
	conn := funcBody(t, js, "connect")
	catch := regexp.MustCompile(`(?s)catch\s*\([^)]*\)\s*\{(.*?)\n    \}`).FindStringSubmatch(conn)
	if catch == nil {
		t.Fatal("connect() has no catch block around the WebSocket constructor")
	}
	if !strings.Contains(catch[1], "setConnected(false)") {
		t.Error("connect()'s catch does not report the failure, so a constructor that throws never shows the overlay")
	}
}

func TestOfflineOverlayWaitsBeforeAppearing(t *testing.T) {
	// Without a grace period an ordinary one-second reconnect would flash a
	// full-screen failure over a working app.
	js := readStatic(t, "app.js")
	if !regexp.MustCompile(`OFFLINE_GRACE_MS\s*=\s*(\d+)`).MatchString(js) {
		t.Fatal("no OFFLINE_GRACE_MS")
	}
	body := funcBody(t, js, "scheduleOffline")
	if !strings.Contains(body, "setTimeout") {
		t.Error("scheduleOffline shows the overlay immediately; a brief reconnect would flash it")
	}
	if !strings.Contains(body, "navigator.onLine") {
		t.Error("scheduleOffline ignores navigator.onLine, so a phone with no network waits out the full grace period for nothing")
	}
}

func TestRetryIsOfferedAndResetsTheBackoff(t *testing.T) {
	// The reconnect backoff grows to twenty seconds, so without a retry the user
	// can be left staring at the overlay long after walking back in range.
	js := readStatic(t, "app.js")
	body := funcBody(t, js, "retryNow")
	if !strings.Contains(body, "wsBackoff = 1000") {
		t.Error("retryNow does not reset the backoff, so the retry may do nothing for twenty seconds")
	}
	if !strings.Contains(body, "connect()") {
		t.Error("retryNow does not reconnect")
	}
	if !strings.Contains(js, "btn-offline-retry") {
		t.Error("nothing wires the retry button")
	}
}
