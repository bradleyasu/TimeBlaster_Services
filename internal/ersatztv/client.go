// Package ersatztv is Timeblaster's client for the ErsatzTV server that turns the
// media library into live channels.
//
// The coupling here is deliberately thin. Timeblaster needs three things from
// ErsatzTV — what channels exist, what they are called, and what URL plays them —
// and nothing about the alarm clock depends on any of it. If ErsatzTV is down,
// starting, or removed entirely, the channel list is simply empty and the
// television shows the no-channel image.
package ersatztv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Channel is one ErsatzTV channel.
type Channel struct {
	// ID is ErsatzTV's internal identifier.
	ID int `json:"id"`
	// Number is the channel number as shown in guides. It is a string because
	// ErsatzTV supports sub-channels such as "2.1".
	Number string `json:"number"`
	// Name is the display name, used in the channel-change overlay.
	Name string `json:"name"`
	// StreamingMode is ErsatzTV's per-channel mode, e.g. HttpLiveStreamingDirect.
	StreamingMode string `json:"streamingMode,omitempty"`
	// Language is an ISO code, carried through for completeness.
	Language string `json:"language,omitempty"`
	// FFmpegProfile names the transcoding profile. Useful in diagnostics when a
	// channel unexpectedly pegs the CPU.
	FFmpegProfile string `json:"ffmpegProfile,omitempty"`
}

// Label renders the channel for logs and the UI.
func (c Channel) Label() string {
	if c.Name == "" {
		return c.Number
	}
	return c.Number + " " + c.Name
}

// SortKey converts a channel number into a comparable pair so that "2.1" sorts
// after "2" and before "10". Plain string sorting would put "10" before "2",
// which would silently scramble the channel knob's mapping.
func (c Channel) SortKey() (major, minor int) {
	parts := strings.SplitN(c.Number, ".", 2)
	major, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	}
	return major, minor
}

// SortChannels orders channels by number. This order is what the channel
// potentiometer's position bands map onto, so it must be stable and predictable.
func SortChannels(chs []Channel) {
	sort.SliceStable(chs, func(i, j int) bool {
		ai, an := chs[i].SortKey()
		bi, bn := chs[j].SortKey()
		if ai != bi {
			return ai < bi
		}
		if an != bn {
			return an < bn
		}
		return chs[i].Number < chs[j].Number
	})
}

// API is the ErsatzTV interface the rest of Timeblaster depends on.
type API interface {
	// Channels returns the configured channels, sorted by number.
	Channels(ctx context.Context) ([]Channel, error)
	// StreamURL returns the playback URL for a channel.
	StreamURL(c Channel) string
	// Ping reports whether the server is reachable.
	Ping(ctx context.Context) error
	// BaseURL returns the configured server address, for diagnostics.
	BaseURL() string
}

// Options configure the client.
type Options struct {
	// BaseURL is the ErsatzTV address, e.g. http://127.0.0.1:8409.
	BaseURL string
	// Timeout bounds a single request.
	Timeout time.Duration
	// StreamMode is the mode query parameter: mixed, hls, ts, hls-direct,
	// segmenter, segmenter-v2, segmenter-fmp4 or ts-legacy. "hls-direct" avoids
	// transcoding when the library is already H.264/AAC in MP4, which is the
	// intended format; "mixed" lets ErsatzTV decide.
	StreamMode string
	// StreamFormat is "m3u8" for HLS or "ts" for MPEG-TS.
	StreamFormat string
	// HTTPClient overrides the default client, which tests use to avoid a network.
	HTTPClient *http.Client
}

// ErrUnavailable indicates the server could not be reached or returned an
// unusable response. It is distinguished so callers can treat it as "retry
// later" rather than "something is wrong with the configuration".
var ErrUnavailable = errors.New("ersatztv: server unavailable")

// Client is the HTTP implementation of API.
type Client struct {
	base   *url.URL
	http   *http.Client
	mode   string
	format string
}

// New creates a client. An unparsable base URL is an error rather than a
// deferred surprise on the first request.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("ersatztv: empty base URL")
	}
	u, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("ersatztv: parsing base URL %q: %w", opts.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("ersatztv: base URL must be http or https, got %q", opts.BaseURL)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// ErsatzTV is on localhost; a small pool is plenty and keeps the
				// daemon's footprint down.
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	format := opts.StreamFormat
	if format == "" {
		format = "m3u8"
	}
	return &Client{base: u, http: hc, mode: opts.StreamMode, format: format}, nil
}

// BaseURL reports the configured server address.
func (c *Client) BaseURL() string { return c.base.String() }

// PlaylistPath is the IPTV playlist the channel list is read from.
//
// Not /api/channels: ErsatzTV v26 requires authentication there and answers an
// anonymous request with 401, while the playlist every other client consumes
// stays open. See m3u.go.
const PlaylistPath = "/iptv/channels.m3u"

// Channels fetches the channel list.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	body, err := c.get(ctx, PlaylistPath)
	if err != nil {
		return nil, err
	}

	raw := string(body)
	// A playlist should announce itself. Anything else means we are talking to
	// something that is not ErsatzTV, or to a login page.
	if !strings.Contains(raw, "#EXTM3U") && strings.TrimSpace(raw) != "" {
		return nil, fmt.Errorf("%w: %s did not return an M3U playlist", ErrUnavailable, PlaylistPath)
	}

	// ParseM3U already drops entries with no channel number, so nothing that
	// would become a dead band on the channel knob survives.
	return ParseM3U(raw), nil
}

// Ping checks that the server is up. It uses the playlist endpoint because a
// server that is listening but has not finished initialising its database will
// accept a TCP connection and then fail the request — which is exactly the state
// we need to distinguish during boot.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.get(ctx, PlaylistPath)
	return err
}

// StreamURL builds the playback URL for a channel.
//
// Built rather than taken from the M3U playlist so that Timeblaster never has to
// parse and cache a playlist it would otherwise ignore, and so the stream mode
// stays configurable per installation.
func (c *Client) StreamURL(ch Channel) string {
	u := *c.base
	prefix := strings.TrimRight(u.Path, "/") + "/iptv/channel/"
	// Path holds the decoded form and RawPath the encoded one. Assigning both is
	// what makes url.URL escape the channel number exactly once: setting only
	// Path would leave a stray slash able to alter the URL's structure, and
	// pre-escaping into Path alone would double-encode it.
	u.Path = prefix + ch.Number + "." + c.format
	u.RawPath = prefix + url.PathEscape(ch.Number) + "." + c.format
	if c.mode != "" {
		q := u.Query()
		q.Set("mode", c.mode)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("ersatztv: building request: %w", err)
	}
	req.Header.Set("Accept", "*/*")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, u.String(), err)
	}
	defer resp.Body.Close()

	// Bound the read: a misconfigured base URL could point at something that
	// streams forever, and this runs on a device with 8 GB of RAM shared with
	// FFmpeg.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrUnavailable, u.String(), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s returned %s", ErrUnavailable, u.String(), resp.Status)
	}
	return body, nil
}

var _ API = (*Client)(nil)
