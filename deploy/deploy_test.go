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
