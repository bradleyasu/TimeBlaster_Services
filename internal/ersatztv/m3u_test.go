package ersatztv

import "testing"

// realPlaylist is captured verbatim from ErsatzTV v26.10.0 running on the
// Raspberry Pi, including the default channel a fresh install creates. Pinning
// the real bytes is what caught that /api/channels had started returning 401
// while this endpoint stayed open.
const realPlaylist = `#EXTM3U url-tvg="http://127.0.0.1:8409/iptv/xmltv.xml" x-tvg-url="http://127.0.0.1:8409/iptv/xmltv.xml"
#EXTINF:0 tvg-id="C1.145.ersatztv.org" channel-id="6Yn5GlwfE0uqkBcy6t6gaw" channel-number="1" CUID="6Yn5GlwfE0uqkBcy6t6gaw" tvg-chno="1" tvg-name="ErsatzTV" tvg-logo="http://127.0.0.1:8409/iptv/logos/gen?text=ErsatzTV" group-title="ErsatzTV" tvc-stream-vcodec="h264" tvc-stream-acodec="aac", ErsatzTV
http://127.0.0.1:8409/iptv/channel/1.ts
`

func TestParseRealPlaylist(t *testing.T) {
	chs := ParseM3U(realPlaylist)
	if len(chs) != 1 {
		t.Fatalf("got %d channels: %+v", len(chs), chs)
	}
	if chs[0].Number != "1" {
		t.Errorf("number: %q", chs[0].Number)
	}
	if chs[0].Name != "ErsatzTV" {
		t.Errorf("name: %q", chs[0].Name)
	}
	if chs[0].FFmpegProfile != "h264" {
		t.Errorf("video codec should be carried through for diagnostics: %q", chs[0].FFmpegProfile)
	}
}

func TestParseM3UOrdersNumerically(t *testing.T) {
	// The band the channel knob maps onto is this order, so "10" must not sort
	// between "1" and "2" the way a plain string sort would put it.
	chs := ParseM3U(sampleChannels)
	want := []string{"1", "2", "2.1", "10"}
	if len(chs) != len(want) {
		t.Fatalf("got %d channels: %+v", len(chs), numbers(chs))
	}
	for i, w := range want {
		if chs[i].Number != w {
			t.Fatalf("order: got %v want %v", numbers(chs), want)
		}
	}
}

func TestParseM3UFallsBackForTheNumberAndName(t *testing.T) {
	// A minimal playlist: no channel-number, no tvg-name. The number comes from
	// the stream URL and the name from the text after the attributes.
	const minimal = `#EXTM3U
#EXTINF:-1 group-title="TV", Late Movie
http://host:8409/iptv/channel/7.m3u8?mode=mixed
`
	chs := ParseM3U(minimal)
	if len(chs) != 1 {
		t.Fatalf("got %+v", chs)
	}
	if chs[0].Number != "7" {
		t.Errorf("number from the URL: %q", chs[0].Number)
	}
	if chs[0].Name != "Late Movie" {
		t.Errorf("name from the trailing text: %q", chs[0].Name)
	}
}

func TestParseM3UHandlesCommasInsideAttributes(t *testing.T) {
	// A comma inside a quoted value must not be mistaken for the separator that
	// precedes the display name.
	const tricky = `#EXTM3U
#EXTINF:0 channel-number="3" group-title="Movies, Classic", Casablanca
http://host/iptv/channel/3.ts
`
	chs := ParseM3U(tricky)
	if len(chs) != 1 || chs[0].Name != "Casablanca" {
		t.Fatalf("got %+v", chs)
	}
}

func TestParseM3USkipsEntriesWithNoUsableNumber(t *testing.T) {
	// An entry with no number would become a band on the channel knob that plays
	// nothing, which is worse than not offering it at all.
	const broken = `#EXTM3U
#EXTINF:0 tvg-name="Broken", Broken
http://host/not-a-channel-url
#EXTINF:0 channel-number="4" tvg-name="Fine", Fine
http://host/iptv/channel/4.ts
`
	chs := ParseM3U(broken)
	if len(chs) != 1 || chs[0].Number != "4" {
		t.Fatalf("got %+v", chs)
	}
}

func TestParseM3UTolerantOfShape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"header only", "#EXTM3U\n", 0},
		{"windows line endings", "#EXTM3U\r\n#EXTINF:0 channel-number=\"1\" tvg-name=\"A\", A\r\nhttp://h/iptv/channel/1.ts\r\n", 1},
		{"blank lines throughout", "#EXTM3U\n\n#EXTINF:0 channel-number=\"1\" tvg-name=\"A\", A\n\nhttp://h/iptv/channel/1.ts\n\n", 1},
		{"extra directives", "#EXTM3U\n#EXTGRP:TV\n#EXTINF:0 channel-number=\"1\" tvg-name=\"A\", A\nhttp://h/iptv/channel/1.ts\n", 1},
		{"an #EXTINF with no URL after it", "#EXTM3U\n#EXTINF:0 channel-number=\"1\" tvg-name=\"A\", A\n", 0},
		{"two #EXTINF in a row", "#EXTM3U\n#EXTINF:0 channel-number=\"1\" tvg-name=\"A\", A\n#EXTINF:0 channel-number=\"2\" tvg-name=\"B\", B\nhttp://h/iptv/channel/2.ts\n", 1},
		{"not a playlist at all", "<html>sign in</html>", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseM3U(tc.in); len(got) != tc.want {
				t.Errorf("got %d channels %+v, want %d", len(got), numbers(got), tc.want)
			}
		})
	}
}

func TestNumberFromURL(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"http://h:8409/iptv/channel/1.ts", "1"},
		{"http://h:8409/iptv/channel/10.m3u8", "10"},
		{"http://h:8409/iptv/channel/2.1.m3u8?mode=mixed", "2.1"},
		{"http://h:8409/iptv/channel/3.ts?mode=ts", "3"},
		{"http://h:8409/something/else", ""},
		{"", ""},
	} {
		if got := numberFromURL(tc.url); got != tc.want {
			t.Errorf("numberFromURL(%q) = %q want %q", tc.url, got, tc.want)
		}
	}
}
