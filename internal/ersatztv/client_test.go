package ersatztv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A real ErsatzTV /api/channels response.
const sampleChannels = `[
  {"id":1,"number":"1","name":"Movies 24/7","ffmpegProfile":"Default","language":"eng","streamingMode":"HttpLiveStreamingDirect"},
  {"id":3,"number":"10","name":"Cartoons","ffmpegProfile":"Default","language":"eng","streamingMode":"TransportStream"},
  {"id":2,"number":"2.1","name":"Sci-Fi Sub","ffmpegProfile":"Default","language":"eng","streamingMode":"HttpLiveStreamingDirect"},
  {"id":4,"number":"2","name":"Sci-Fi","ffmpegProfile":"Default","language":"eng","streamingMode":"HttpLiveStreamingDirect"}
]`

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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleChannels))
	})

	chs, err := c.Channels(context.Background())
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	if gotPath != "/api/channels" {
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
	if chs[0].StreamingMode != "HttpLiveStreamingDirect" {
		t.Errorf("streaming mode: %q", chs[0].StreamingMode)
	}
}

func TestChannelsDropsEntriesWithoutANumber(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"number":"","name":"Broken"},{"id":2,"number":"3","name":"Fine"}]`))
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
		_, _ = w.Write([]byte(`[]`))
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
		{"malformed json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>starting up</html>")) }},
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
		_, _ = w.Write([]byte(`[]`))
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
