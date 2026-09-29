package system

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultDRMRoot is where the kernel exposes display connectors.
const DefaultDRMRoot = "/sys/class/drm"

// HDMICards are the ALSA card names for the Pi's two HDMI outputs, in port
// order. Card *numbers* are useless here: they are assigned in enumeration
// order, so plugging in a USB speaker renumbers everything. On this device the
// speaker took card 0 and pushed both HDMI outputs up by one, which is exactly
// how the television's audio ended up coming out of the speaker.
var HDMICards = []string{"vc4hdmi0", "vc4hdmi1"}

// ConnectedHDMICard returns the ALSA card name of the HDMI port with something
// plugged into it.
//
// Connector HDMI-A-1 is the first port and pairs with vc4hdmi0, HDMI-A-2 with
// vc4hdmi1. With both connected the first wins; with neither, the first is
// still returned, because a television plugged in later should find audio
// already pointed at it rather than silently at the other port.
func ConnectedHDMICard(drmRoot string) string {
	if drmRoot == "" {
		drmRoot = DefaultDRMRoot
	}
	matches, err := filepath.Glob(filepath.Join(drmRoot, "*-HDMI-A-*"))
	if err != nil || len(matches) == 0 {
		return HDMICards[0]
	}
	sort.Strings(matches)

	for _, dir := range matches {
		status, err := os.ReadFile(filepath.Join(dir, "status"))
		if err != nil || strings.TrimSpace(string(status)) != "connected" {
			continue
		}
		// ".../card1-HDMI-A-2" -> port 2 -> HDMICards[1]
		name := filepath.Base(dir)
		switch {
		case strings.HasSuffix(name, "-HDMI-A-1"):
			return HDMICards[0]
		case strings.HasSuffix(name, "-HDMI-A-2"):
			return HDMICards[1]
		}
	}
	return HDMICards[0]
}

// ALSADeviceForCard renders an mpv audio-device string for a card name.
//
// plughw rather than hw so ALSA resamples when the sink cannot take the
// stream's rate directly -- an HDMI sink advertising only 48 kHz would
// otherwise refuse a 44.1 kHz track outright.
func ALSADeviceForCard(card string) string {
	if card == "" {
		return "auto"
	}
	return "alsa/plughw:CARD=" + card
}
