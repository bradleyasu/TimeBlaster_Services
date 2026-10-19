// Package deploy holds the files setup.sh installs. It has no Go code; this
// test exists so that `go test ./...` checks the deploy tree is complete.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The first install from a clean checkout failed because a file setup.sh
// installs had never been committed: an unanchored `bin/` in .gitignore also
// matched deploy/bin/. The running Pi only had it because it was copied over by
// hand. Every working machine had the file on disk, so nothing noticed -- only a
// fresh clone could, and that is exactly the situation a dead SD card puts you
// in.

var deployRefRe = regexp.MustCompile(`deploy/[A-Za-z0-9_./-]+`)

// intentionallyUntracked are referenced by setup.sh but deliberately kept out of
// git: they hold the user's own files.
var intentionallyUntracked = map[string]bool{
	"deploy/sounds": true,
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// referencedFiles returns every file under deploy/ that setup.sh names.
// Directories are skipped: what matters is the files inside them.
func referencedFiles(t *testing.T, root string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "setup.sh"))
	if err != nil {
		t.Fatalf("reading setup.sh: %v", err)
	}
	seen := map[string]bool{}
	for _, ref := range deployRefRe.FindAllString(string(body), -1) {
		ref = strings.TrimSuffix(ref, "/")
		if intentionallyUntracked[ref] || seen[ref] {
			continue
		}
		info, err := os.Stat(filepath.Join(root, ref))
		if err == nil && info.IsDir() {
			continue
		}
		seen[ref] = true
	}
	var out []string
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("found no deploy/ references in setup.sh; has the script changed?")
	}
	return out
}

func TestEveryFileSetupInstallsIsCommitted(t *testing.T) {
	root := repoRoot(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	if err := exec.Command("git", "-C", root, "rev-parse", "--git-dir").Run(); err != nil {
		t.Skip("not a git checkout")
	}

	for _, ref := range referencedFiles(t, root) {
		if _, err := os.Stat(filepath.Join(root, ref)); err != nil {
			t.Errorf("setup.sh installs %s, which does not exist", ref)
			continue
		}
		// On disk is not enough: the file that broke the fresh install was on
		// disk on every machine that had ever deployed by hand.
		cmd := exec.Command("git", "-C", root, "ls-files", "--error-unmatch", ref)
		if err := cmd.Run(); err != nil {
			why := ""
			if out, _ := exec.Command("git", "-C", root, "check-ignore", "-v", ref).Output(); len(out) > 0 {
				why = " -- ignored by " + strings.TrimSpace(string(out))
			}
			t.Errorf("setup.sh installs %s, but it is not committed, so a fresh clone does not have it%s", ref, why)
		}
	}
}

func TestDeployedScriptsAreCommittedExecutable(t *testing.T) {
	// setup.sh installs these with an explicit mode, but a clone with the bit
	// missing would still trip anyone running them by hand from the checkout.
	root := repoRoot(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-s", "deploy/bin").Output()
	if err != nil {
		t.Skip("not a git checkout")
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("nothing under deploy/bin is committed")
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[0] != "100755" {
			t.Errorf("%s is committed with mode %s, want 100755", fields[3], fields[0])
		}
	}
}

// The Pi offers a firmware update when the shipped image's version differs from
// what the Nano reports. Committing firmware changes without running
// `make firmware-release` would leave the Pi offering the old image as though
// it were current -- and a release built from other source would be flashed
// under a version that names the wrong code.
func TestShippedFirmwareMatchesTheFirmwareSource(t *testing.T) {
	root := repoRoot(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	out, err := exec.Command("git", "-C", root, "log", "-1", "--format=%H", "--",
		"firmware/timeblaster-nano/src",
		"firmware/timeblaster-nano/platformio.ini",
		"firmware/timeblaster-nano/version.py").Output()
	want := strings.TrimSpace(string(out))
	if err != nil || want == "" {
		t.Skip("no firmware history in this checkout (shallow clone?)")
	}

	raw, err := os.ReadFile(filepath.Join(root, "deploy/firmware/nano-firmware.version"))
	if err != nil {
		t.Fatalf("no shipped firmware version: %v (run make firmware-release)", err)
	}
	version := strings.TrimSpace(string(raw))
	if len(version) < 7 || !strings.HasPrefix(want, version) {
		t.Errorf("deploy/firmware is version %q, but the firmware source was last changed in %.12s.\n"+
			"Run `make firmware-release` and commit deploy/firmware/.", version, want)
	}

	image, err := os.ReadFile(filepath.Join(root, "deploy/firmware/nano-firmware.bin"))
	if err != nil {
		t.Fatalf("no shipped firmware image: %v", err)
	}
	// The version is compiled in as a string, so an image and version file
	// from different builds disagree here.
	if !strings.Contains(string(image), version) {
		t.Errorf("nano-firmware.bin does not contain its own version %q; the two files are from different builds", version)
	}
}
