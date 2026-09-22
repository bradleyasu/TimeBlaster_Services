package ersatztv

import (
	"regexp"
	"strings"
)

// The channel list comes from the IPTV playlist rather than from the JSON API.
//
// ErsatzTV v26 requires authentication on /api/, which returns 401 to an
// anonymous caller, while /iptv/channels.m3u stays open — it is the interface
// every other client (Plex, Jellyfin, Kodi) consumes. Reading the playlist means
// Timeblaster needs no credentials and no configuration to discover channels,
// and it keeps working if the API's auth rules change again.
//
// A real line from a running server:
//
//	#EXTINF:0 tvg-id="C1.145.ersatztv.org" channel-id="6Yn5Gl..." channel-number="1" \
//	  CUID="6Yn5Gl..." tvg-chno="1" tvg-name="ErsatzTV" tvg-logo="..." \
//	  group-title="ErsatzTV" tvc-stream-vcodec="h264" tvc-stream-acodec="aac", ErsatzTV
//	http://127.0.0.1:8409/iptv/channel/1.ts

// attrRe matches the key="value" attributes on an #EXTINF line.
var attrRe = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)

// ParseM3U extracts the channel list from an ErsatzTV IPTV playlist.
//
// Split out as a pure function so the format can be pinned by tests against
// real captured output, with no server involved.
func ParseM3U(body string) []Channel {
	var (
		out     []Channel
		pending *Channel
	)

	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			if ch, ok := parseEXTINF(line); ok {
				pending = &ch
			} else {
				pending = nil
			}

		case strings.HasPrefix(line, "#"):
			// Any other directive (#EXTM3U, #EXTGRP, ...) is not our business.

		default:
			// A bare line is the stream URL for the #EXTINF above it. The number
			// is taken from the URL only when the attributes did not carry one,
			// which is what lets a minimal playlist still work.
			if pending == nil {
				continue
			}
			if pending.Number == "" {
				pending.Number = numberFromURL(line)
			}
			if pending.Number != "" {
				out = append(out, *pending)
			}
			pending = nil
		}
	}

	SortChannels(out)
	return out
}

func parseEXTINF(line string) (Channel, bool) {
	attrs := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(line, -1) {
		attrs[strings.ToLower(m[1])] = m[2]
	}

	var ch Channel
	// channel-number is ErsatzTV's own; tvg-chno is the widely understood
	// equivalent. Either is the number the channel knob selects.
	for _, key := range []string{"channel-number", "tvg-chno"} {
		if v := strings.TrimSpace(attrs[key]); v != "" {
			ch.Number = v
			break
		}
	}

	ch.Name = strings.TrimSpace(attrs["tvg-name"])
	if ch.Name == "" {
		ch.Name = displayName(line)
	}
	if v := strings.TrimSpace(attrs["tvc-stream-vcodec"]); v != "" {
		// Not the FFmpeg profile, but the closest thing the playlist exposes and
		// genuinely useful when diagnosing why a channel is transcoding.
		ch.FFmpegProfile = v
	}
	return ch, true
}

// displayName returns the text after the attribute list, which is the channel
// name in a playlist that omits tvg-name.
//
// It scans from the last quote so a comma inside an attribute value cannot be
// mistaken for the separator.
func displayName(line string) string {
	tail := line
	if i := strings.LastIndex(line, `"`); i >= 0 {
		tail = line[i+1:]
	}
	if i := strings.Index(tail, ","); i >= 0 {
		return strings.TrimSpace(tail[i+1:])
	}
	return ""
}

// numberRe pulls the channel number out of a stream URL such as
// /iptv/channel/2.1.m3u8 or /iptv/channel/10.ts.
var numberRe = regexp.MustCompile(`/iptv/channel/([^/?]+?)\.(m3u8|ts)(\?|$)`)

func numberFromURL(url string) string {
	if m := numberRe.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
}
