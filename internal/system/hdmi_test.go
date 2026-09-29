package system

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConnector(t *testing.T, root, name, status string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConnectedHDMICard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup map[string]string
		want  string
	}{
		{
			"second port connected",
			map[string]string{"card1-HDMI-A-1": "disconnected", "card1-HDMI-A-2": "connected"},
			"vc4hdmi1",
		},
		{
			"first port connected",
			map[string]string{"card1-HDMI-A-1": "connected", "card1-HDMI-A-2": "disconnected"},
			"vc4hdmi0",
		},
		{
			// A television plugged in later should find audio already pointed
			// at the first port rather than silently at the other one.
			"neither connected falls back to the first",
			map[string]string{"card1-HDMI-A-1": "disconnected", "card1-HDMI-A-2": "disconnected"},
			"vc4hdmi0",
		},
		{
			"both connected prefers the first",
			map[string]string{"card1-HDMI-A-1": "connected", "card1-HDMI-A-2": "connected"},
			"vc4hdmi0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for conn, status := range tc.setup {
				writeConnector(t, root, conn, status)
			}
			// Connectors that are not HDMI must be ignored.
			writeConnector(t, root, "card1-DP-1", "connected")

			if got := ConnectedHDMICard(root); got != tc.want {
				t.Errorf("ConnectedHDMICard = %q, want %q", got, tc.want)
			}
		})
	}

	// No DRM at all, as on a development machine.
	if got := ConnectedHDMICard(t.TempDir()); got != "vc4hdmi0" {
		t.Errorf("with no connectors, got %q", got)
	}
}

func TestALSADeviceForCard(t *testing.T) {
	// Named, never numbered: card numbers shift with enumeration order, which
	// is how a USB speaker became card 0 and stole the television's audio.
	if got := ALSADeviceForCard("vc4hdmi0"); got != "alsa/plughw:CARD=vc4hdmi0" {
		t.Errorf("got %q", got)
	}
	if got := ALSADeviceForCard(""); got != "auto" {
		t.Errorf("an empty card should fall back to auto, got %q", got)
	}
}
