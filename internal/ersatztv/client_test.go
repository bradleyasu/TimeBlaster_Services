package ersatztv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A real ErsatzTV playlist. The first entry is captured verbatim from a running
// v26.10.0 server on the Raspberry Pi, attributes and spacing unaltered; the
// rest follow its shape to cover numbering and ordering.
const sampleChannels = `#EXTM3U url-tvg="http://127.0.0.1:8409/iptv/xmltv.xml" x-tvg-url="http://127.0.0.1:8409/iptv/xmltv.xml"
#EXTINF:0 tvg-id="C1.145.ersatztv.org" channel-id="6Yn5GlwfE0uqkBcy6t6gaw" channel-number="1" CUID="6Yn5GlwfE0uqkBcy6t6gaw" tvg-chno="1" tvg-name="Movies 24/7" tvg-logo="http://127.0.0.1:8409/iptv/logos/gen?text=ErsatzTV" group-title="ErsatzTV" tvc-stream-vcodec="h264" tvc-stream-acodec="aac", Movies 24/7
http://127.0.0.1:8409/iptv/channel/1.ts
#EXTINF:0 tvg-id="C10.145.ersatztv.org" channel-number="10" tvg-chno="10" tvg-name="Cartoons" group-title="ErsatzTV", Cartoons
http://127.0.0.1:8409/iptv/channel/10.ts
#EXTINF:0 tvg-id="C21.145.ersatztv.org" channel-number="2.1" tvg-chno="2.1" tvg-name="Sci-Fi Sub" group-title="ErsatzTV", Sci-Fi Sub
http://127.0.0.1:8409/iptv/channel/2.1.m3u8
#EXTINF:0 tvg-id="C2.145.ersatztv.org" channel-number="2" tvg-chno="2" tvg-name="Sci-Fi" group-title="ErsatzTV", Sci-Fi
http://127.0.0.1:8409/iptv/channel/2.ts
`

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL, Timeout: 2 * time.Second, StreamMode: "mixed", StreamFormat: "m3u8"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestNewValidatesBaseURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://host", "not a url at all\n"} {
		if _, err := New(Options{BaseURL: bad}); err == nil {
			t.Errorf("New(%q) should have failed", bad)
		}
	}
	c, err := New(Options{BaseURL: "http://127.0.0.1:8409/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.BaseURL(); got != "http://127.0.0.1:8409" {
		t.Errorf("trailing slash not trimmed: %q", got)
	}
}

func TestChannelsParsesAndSorts(t *testing.T) {
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(sampleChannels))
	})

	chs, err := c.Channels(context.Background())
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	// The playlist, not /api/channels: v26 answers that with 401.
	if gotPath != PlaylistPath {
		t.Errorf("path: %q", gotPath)
	}
	// Numeric ordering: 1, 2, 2.1, 10 — not the lexical 1, 10, 2, 2.1.
	want := []string{"1", "2", "2.1", "10"}
	for i, w := range want {
		if chs[i].Number != w {
			t.Fatalf("order: got %v want %v", numbers(chs), want)
		}
	}
	if chs[0].Name != "Movies 24/7" {
		t.Errorf("name: %q", chs[0].Name)
	}
}

func TestChannelsReadsThePlaylistNotTheAuthenticatedAPI(t *testing.T) {
	// The regression this guards: ErsatzTV v26 returns 401 from /api/, which had
	// Timeblaster reporting ersatztv down on a perfectly healthy server.
	var paths []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(sampleChannels))
	})

	if _, err := c.Channels(context.Background()); err != nil {
		t.Fatalf("Channels: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "/api/") {
			t.Errorf("must not touch the authenticated API, but requested %q", p)
		}
	}
}

func TestChannelsDropsEntriesWithoutANumber(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n" +
			"#EXTINF:0 tvg-name=\"Broken\", Broken\n" +
			"http://host/something-else\n" +
			"#EXTINF:0 channel-number=\"3\" tvg-name=\"Fine\", Fine\n" +
			"http://host/iptv/channel/3.ts\n"))
	})
	chs, err := c.Channels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 1 || chs[0].Number != "3" {
		t.Errorf("got %+v", chs)
	}
}

func TestChannelsEmptyList(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n"))
	})
	chs, err := c.Channels(context.Background())
	if err != nil {
		t.Fatalf("an empty channel list is normal, not an error: %v", err)
	}
	if len(chs) != 0 {
		t.Errorf("got %+v", chs)
	}
}

func TestChannelsReportsServerErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http 500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }},
		{"http 404", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }},
		{"a login page instead of a playlist", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>sign in</html>")) }},
		{"the authenticated api", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.handler)
			_, err := c.Channels(context.Background())
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("got %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestChannelsHandlesAServerThatIsNotListening(t *testing.T) {
	// This is the boot-order case: timeblasterd starts before ErsatzTV.
	c, err := New(Options{BaseURL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Channels(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

func TestChannelsRespectsContextCancellation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Channels(ctx); err == nil {
		t.Fatal("expected a cancellation error")
	}
}

func TestPing(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n"))
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}

	// A server that is listening but not ready must fail the ping, because that is
	// exactly the state ErsatzTV is in while it initialises.
	bad, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	if err := bad.Ping(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("got %v", err)
	}
}

func TestStreamURL(t *testing.T) {
	tests := []struct {
		name   string
		opts   Options
		number string
		want   string
	}{
		{
			name:   "hls with mixed mode",
			opts:   Options{BaseURL: "http://127.0.0.1:8409", StreamMode: "mixed", StreamFormat: "m3u8"},
			number: "3",
			want:   "http://127.0.0.1:8409/iptv/channel/3.m3u8?mode=mixed",
		},
		{
			name:   "direct hls avoids transcoding",
			opts:   Options{BaseURL: "http://127.0.0.1:8409", StreamMode: "hls-direct", StreamFormat: "m3u8"},
			number: "1",
			want:   "http://127.0.0.1:8409/iptv/channel/1.m3u8?mode=hls-direct",
		},
		{
			name:   "mpeg-ts",
			opts:   Options{BaseURL: "http://127.0.0.1:8409", StreamMode: "ts", StreamFormat: "ts"},
			number: "2.1",
			want:   "http://127.0.0.1:8409/iptv/channel/2.1.ts?mode=ts",
		},
		{
			name:   "no mode leaves the query empty",
			opts:   Options{BaseURL: "http://etv.local:8409", StreamFormat: "m3u8"},
			number: "7",
			want:   "http://etv.local:8409/iptv/channel/7.m3u8",
		},
		{
			name:   "base url with a path prefix",
			opts:   Options{BaseURL: "http://proxy.local/ersatz/", StreamMode: "mixed", StreamFormat: "m3u8"},
			number: "4",
			want:   "http://proxy.local/ersatz/iptv/channel/4.m3u8?mode=mixed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.StreamURL(Channel{Number: tc.number}); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestStreamURLEscapesChannelNumbers(t *testing.T) {
	// Channel numbers come from ErsatzTV's database; a strange one must not be
	// able to alter the URL's structure.
	c, err := New(Options{BaseURL: "http://127.0.0.1:8409", StreamFormat: "m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	got := c.StreamURL(Channel{Number: "../../etc/passwd"})
	if got != "http://127.0.0.1:8409/iptv/channel/..%2F..%2Fetc%2Fpasswd.m3u8" {
		t.Errorf("got %s", got)
	}
}

func TestChannelSortKeyAndLabel(t *testing.T) {
	for _, tc := range []struct {
		number       string
		major, minor int
	}{
		{"1", 1, 0}, {"10", 10, 0}, {"2.1", 2, 1}, {"", 0, 0}, {"abc", 0, 0},
	} {
		maj, min := Channel{Number: tc.number}.SortKey()
		if maj != tc.major || min != tc.minor {
			t.Errorf("SortKey(%q) = %d,%d want %d,%d", tc.number, maj, min, tc.major, tc.minor)
		}
	}

	if got := (Channel{Number: "3", Name: "Movies"}).Label(); got != "3 Movies" {
		t.Errorf("Label: %q", got)
	}
	if got := (Channel{Number: "3"}).Label(); got != "3" {
		t.Errorf("Label without name: %q", got)
	}
}

func TestFakeAPI(t *testing.T) {
	f := NewFake(Channel{Number: "2", Name: "B"}, Channel{Number: "1", Name: "A"})

	chs, err := f.Channels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 2 || chs[0].Number != "1" {
		t.Errorf("fake should sort: %+v", chs)
	}
	if f.CallCount() != 1 {
		t.Errorf("call count: %d", f.CallCount())
	}
	if got := f.StreamURL(chs[0]); got != "http://fake.ersatztv/iptv/channel/1.m3u8" {
		t.Errorf("StreamURL: %q", got)
	}

	boom := errors.New("ersatztv is restarting")
	f.SetError(boom)
	if _, err := f.Channels(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Channels: %v", err)
	}
	if err := f.Ping(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Ping: %v", err)
	}
}

func numbers(chs []Channel) []string {
	out := make([]string, len(chs))
	for i, c := range chs {
		out[i] = c.Number
	}
	return out
}
