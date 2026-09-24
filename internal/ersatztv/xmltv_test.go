package ersatztv

import (
	"testing"
	"time"
)

// A cut-down document in exactly the shape ErsatzTV emits, byte-order mark and
// all. Channel 9 is listed with no programmes, which is what a channel that has
// no playout looks like.
const sampleXMLTV = "\xef\xbb\xbf" + `<?xml version="1.0" encoding="utf-8"?><tv generator-info-name="ersatztv">` +
	`<channel id="C2.146.ersatztv.org"><display-name>2 Music TV</display-name><display-name>2</display-name><display-name>Music TV</display-name></channel>` +
	`<channel id="C3.147.ersatztv.org"><display-name>3 Bob Ross TV</display-name><display-name>3</display-name><display-name>Bob Ross TV</display-name></channel>` +
	`<programme start="20260924124041 -0400" stop="20260924124427 -0400" channel="C2.146.ersatztv.org"><title lang="en">Warner_Records_Vault</title><sub-title lang="en">Citizen_King</sub-title><category lang="en">Music</category></programme>` +
	`<programme start="20260924123629 -0400" stop="20260924124041 -0400" channel="C2.146.ersatztv.org"><title lang="en">JawbreakerVEVO</title><sub-title lang="en">Fireman</sub-title><category lang="en">Music</category></programme>` +
	`<programme start="20260924130000 -0400" stop="20260924133000 -0400" channel="C3.147.ersatztv.org"><title lang="en">The Joy of Painting</title></programme>` +
	`<programme start="not-a-time" stop="20260924133000 -0400" channel="C3.147.ersatztv.org"><title>Broken</title></programme>` +
	`<programme start="20260924140000 -0400" stop="20260924133000 -0400" channel="C3.147.ersatztv.org"><title>Backwards</title></programme>` +
	`</tv>`

func TestParseXMLTV(t *testing.T) {
	// The fixture carries the byte-order mark ErsatzTV really emits.
	// encoding/xml handles it without help -- the mark is kept here so that
	// stays covered rather than assumed.
	x, err := ParseXMLTV([]byte(sampleXMLTV))
	if err != nil {
		t.Fatalf("ParseXMLTV: %v", err)
	}

	music := x.Programmes["C2.146.ersatztv.org"]
	if len(music) != 2 {
		t.Fatalf("music programmes: %d, want 2", len(music))
	}
	// Sorted by start, whatever order the document listed them in.
	if music[0].Title != "JawbreakerVEVO" || music[1].Title != "Warner_Records_Vault" {
		t.Errorf("programmes are not in start order: %q then %q", music[0].Title, music[1].Title)
	}
	if got := music[0].SubTitle; got != "Fireman" {
		t.Errorf("sub-title: %q", got)
	}
	if got := music[0].Duration(); got != 4*time.Minute+12*time.Second {
		t.Errorf("duration: %v", got)
	}

	// One unparseable time and one that ends before it starts: each costs its
	// own entry, not the whole listing.
	if n := len(x.Programmes["C3.147.ersatztv.org"]); n != 1 {
		t.Errorf("bob ross programmes: %d, want 1 (two are malformed)", n)
	}

	// The bare display-name is the fallback join key.
	if got := x.Numbers["C2.146.ersatztv.org"]; got != "2" {
		t.Errorf("bare number: %q, want %q", got, "2")
	}
}

func TestParseXMLTVRejectsRubbish(t *testing.T) {
	if _, err := ParseXMLTV([]byte("this is not xml at all")); err == nil {
		t.Error("expected an error for a non-XML body")
	}
}

func TestMergeGuideKeepsChannelsWithNothingScheduled(t *testing.T) {
	// The lineup is authoritative. XMLTV only publishes channels that have
	// programmes, so a channel that exists and is tunable but has no playout --
	// which is exactly what channel 1 is on the real device -- must still get a
	// row, or the guide looks broken the moment a channel is added.
	x, err := ParseXMLTV([]byte(sampleXMLTV))
	if err != nil {
		t.Fatal(err)
	}
	lineup := []Channel{
		{Number: "1", Name: "ErsatzTV", GuideID: "C1.145.ersatztv.org"},
		{Number: "2", Name: "Music TV", GuideID: "C2.146.ersatztv.org"},
		{Number: "3", Name: "Bob Ross TV", GuideID: "C3.147.ersatztv.org"},
	}

	from := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	g := MergeGuide(lineup, x, from, from.Add(24*time.Hour))

	if len(g.Channels) != 3 {
		t.Fatalf("channels: %d, want all 3 from the lineup", len(g.Channels))
	}
	if g.Channels[0].Number != "1" || g.Channels[0].Name != "ErsatzTV" {
		t.Errorf("first channel: %+v", g.Channels[0])
	}
	if n := len(g.Channels[0].Programmes); n != 0 {
		t.Errorf("channel 1 has no playout, so want 0 programmes, got %d", n)
	}
	if g.Channels[0].Programmes == nil {
		t.Error("an empty schedule must marshal as [], not null")
	}
	if n := len(g.Channels[1].Programmes); n != 2 {
		t.Errorf("channel 2 programmes: %d, want 2", n)
	}
}

func TestMergeGuideMatchesByNumberWithoutATvgID(t *testing.T) {
	x, _ := ParseXMLTV([]byte(sampleXMLTV))
	// A playlist that carried no tvg-id still matches on the channel number,
	// which ErsatzTV publishes as a bare display name.
	lineup := []Channel{{Number: "2", Name: "Music TV"}}

	from := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	g := MergeGuide(lineup, x, from, from.Add(24*time.Hour))
	if n := len(g.Channels[0].Programmes); n != 2 {
		t.Errorf("matched %d programmes by number, want 2", n)
	}
}

func TestMergeGuideWindowIncludesWhatIsOnNow(t *testing.T) {
	x, _ := ParseXMLTV([]byte(sampleXMLTV))
	lineup := []Channel{{Number: "3", Name: "Bob Ross TV", GuideID: "C3.147.ersatztv.org"}}

	// Mid-programme: it began before the window opened and is still running, so
	// it belongs in the listing. What is on now is the first thing anyone looks
	// for.
	from := time.Date(2026, 9, 24, 13, 15, 0, 0, time.FixedZone("EDT", -4*3600))
	g := MergeGuide(lineup, x, from, from.Add(time.Hour))
	if n := len(g.Channels[0].Programmes); n != 1 {
		t.Fatalf("in-progress programme was dropped: got %d", n)
	}

	// Well after everything has finished.
	late := time.Date(2026, 9, 25, 0, 0, 0, 0, time.FixedZone("EDT", -4*3600))
	g = MergeGuide(lineup, x, late, late.Add(time.Hour))
	if n := len(g.Channels[0].Programmes); n != 0 {
		t.Errorf("expected an empty window, got %d", n)
	}
}
