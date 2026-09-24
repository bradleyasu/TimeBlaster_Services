package ersatztv

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"
)

// GuidePath is the XMLTV guide ErsatzTV publishes. Like the playlist it needs
// no authentication, unlike the /api endpoints.
const GuidePath = "/iptv/xmltv.xml"

// Programme is one scheduled item.
type Programme struct {
	Start    time.Time `json:"start"`
	Stop     time.Time `json:"stop"`
	Title    string    `json:"title"`
	SubTitle string    `json:"subTitle,omitempty"`
	Category string    `json:"category,omitempty"`
}

// Duration is how long the programme runs.
func (p Programme) Duration() time.Duration { return p.Stop.Sub(p.Start) }

// XMLTV is a parsed guide document, before it is paired with a channel lineup.
type XMLTV struct {
	// Programmes are grouped by the XMLTV channel id that carries them, in
	// start order.
	Programmes map[string][]Programme
	// Numbers maps an XMLTV channel id to the bare channel number ErsatzTV
	// publishes as one of its display names. It is the fallback join key for a
	// playlist entry with no tvg-id.
	Numbers map[string]string
}

// GuideChannel is one channel of the lineup together with its schedule.
type GuideChannel struct {
	Number string `json:"number"`
	Name   string `json:"name"`
	// Programmes may be empty: a channel with no playout has nothing scheduled,
	// and saying so is more useful than omitting the channel.
	Programmes []Programme `json:"programmes"`
}

// Guide is the channel lineup paired with its schedule.
type Guide struct {
	// From and Until bound the window the programmes were selected for.
	From     time.Time      `json:"from"`
	Until    time.Time      `json:"until"`
	Channels []GuideChannel `json:"channels"`
}

// xmltv document shapes. Only the fields Timeblaster shows are decoded.
type xmltvDoc struct {
	Channels   []xmltvChannel   `xml:"channel"`
	Programmes []xmltvProgramme `xml:"programme"`
}

type xmltvChannel struct {
	ID           string   `xml:"id,attr"`
	DisplayNames []string `xml:"display-name"`
}

type xmltvProgramme struct {
	Start    string `xml:"start,attr"`
	Stop     string `xml:"stop,attr"`
	Channel  string `xml:"channel,attr"`
	Title    string `xml:"title"`
	SubTitle string `xml:"sub-title"`
	Category string `xml:"category"`
}

// xmltvLayouts are the timestamp forms XMLTV allows. ErsatzTV emits the first;
// the offset is optional in the specification, so the second is accepted too
// and read as local time.
var xmltvLayouts = []string{"20060102150405 -0700", "20060102150405"}

// ParseXMLTV decodes an XMLTV guide document.
//
// A programme with an unparseable or reversed time range is skipped rather than
// failing the whole guide: one malformed entry should cost that entry, not the
// television listing.
func ParseXMLTV(body []byte) (XMLTV, error) {
	var doc xmltvDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return XMLTV{}, fmt.Errorf("ersatztv: parsing the XMLTV guide: %w", err)
	}

	out := XMLTV{
		Programmes: make(map[string][]Programme, len(doc.Channels)),
		Numbers:    make(map[string]string, len(doc.Channels)),
	}
	for _, c := range doc.Channels {
		if n := bareNumber(c.DisplayNames); n != "" {
			out.Numbers[c.ID] = n
		}
	}
	for _, p := range doc.Programmes {
		start, ok := parseXMLTVTime(p.Start)
		if !ok {
			continue
		}
		stop, ok := parseXMLTVTime(p.Stop)
		if !ok || !stop.After(start) {
			continue
		}
		out.Programmes[p.Channel] = append(out.Programmes[p.Channel], Programme{
			Start:    start,
			Stop:     stop,
			Title:    strings.TrimSpace(p.Title),
			SubTitle: strings.TrimSpace(p.SubTitle),
			Category: strings.TrimSpace(p.Category),
		})
	}
	for id := range out.Programmes {
		sort.SliceStable(out.Programmes[id], func(i, j int) bool {
			return out.Programmes[id][i].Start.Before(out.Programmes[id][j].Start)
		})
	}
	return out, nil
}

func parseXMLTVTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range xmltvLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// bareNumber picks the display name that is just the channel number.
//
// ErsatzTV publishes three: "2 Music TV", "2" and "Music TV". The bare one is
// the only reliable way to match a playlist entry that carries no tvg-id.
func bareNumber(names []string) string {
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || strings.ContainsAny(n, " \t") {
			continue
		}
		if strings.IndexFunc(n, func(r rune) bool {
			return !(r >= '0' && r <= '9') && r != '.'
		}) < 0 {
			return n
		}
	}
	return ""
}

// MergeGuide pairs a channel lineup with a parsed schedule.
//
// The lineup is authoritative. Every channel the playlist lists gets an entry,
// with an empty schedule when ErsatzTV has nothing planned for it -- XMLTV only
// publishes channels that have programmes, so building the guide from it alone
// would silently drop a channel that had just been added and not yet scheduled,
// which looks indistinguishable from the guide being broken.
//
// Programmes are those overlapping [from, until): one that started before the
// window is included while it is still running, because what is on now is the
// first thing anybody looks for.
func MergeGuide(chs []Channel, x XMLTV, from, until time.Time) Guide {
	byNumber := make(map[string]string, len(x.Numbers))
	for id, num := range x.Numbers {
		byNumber[num] = id
	}

	g := Guide{From: from, Until: until, Channels: make([]GuideChannel, 0, len(chs))}
	for _, ch := range chs {
		id := ch.GuideID
		if _, ok := x.Programmes[id]; !ok {
			// No tvg-id, or one the guide does not carry: fall back to the
			// channel number, which ErsatzTV publishes as a display name.
			if alt, ok := byNumber[ch.Number]; ok {
				id = alt
			}
		}
		gc := GuideChannel{Number: ch.Number, Name: ch.Name, Programmes: []Programme{}}
		for _, p := range x.Programmes[id] {
			if p.Stop.After(from) && p.Start.Before(until) {
				gc.Programmes = append(gc.Programmes, p)
			}
		}
		g.Channels = append(g.Channels, gc)
	}
	return g
}
