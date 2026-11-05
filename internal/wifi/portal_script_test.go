package wifi

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The setup page's script decides, with no help from the Timeblaster, whether
// an attempt worked: the setup network is gone by the time there is an answer.
// So its behaviour is pinned here, by running it against a fake browser for
// each outcome: joined, wrong password, fell back to the old network, refused
// outright, and never heard from.
func TestSetupPageScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	page := portalHTML("TIMEBLASTER-SETUP", "http://timeblaster.local:8080")
	m := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the page has no script")
	}
	script := filepath.Join(t.TempDir(), "portal.js")
	if err := os.WriteFile(script, []byte(m[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "testdata/portal_harness.js", script).CombinedOutput()
	if err != nil {
		t.Errorf("the setup page script misbehaved:\n%s", out)
	}
	if !strings.Contains(string(out), "PASS joined the chosen network") {
		t.Errorf("the harness did not run its scenarios:\n%s", out)
	}
}

func TestSetupPageSendsPeopleToTheRealAppAddress(t *testing.T) {
	// It used to say http://timeblaster.local, where nothing answers: the app
	// is on 8080.
	page := portalHTML("TIMEBLASTER-SETUP", "http://timeblaster.local:8080")
	if n := strings.Count(page, "timeblaster.local"); n == 0 || n != strings.Count(page, "timeblaster.local:8080") {
		t.Errorf("the page names timeblaster.local without the app's port")
	}
	// And its policy lets it ask the app whether setup is over, but nothing else.
	csp := portalCSP("http://timeblaster.local:8080")
	if !strings.Contains(csp, "connect-src 'self' http://timeblaster.local:8080;") {
		t.Errorf("the page may not reach the app: %s", csp)
	}
	if strings.Contains(csp, "*") {
		t.Errorf("the policy is wider than the app: %s", csp)
	}
}

func TestSetupPageEscapesWhatItIsGiven(t *testing.T) {
	page := portalHTML(`</script><b>`, `http://x.local:8080/"</script>`)
	if strings.Contains(page, "</script><b>") || strings.Count(page, "</script>") != 1 {
		t.Error("a configured name or address could break out of the page's script")
	}
}
