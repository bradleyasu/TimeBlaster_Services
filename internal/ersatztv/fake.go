package ersatztv

import (
	"context"
	"sync"
)

// Fake is an in-memory API for tests and for running the daemon on a development
// machine with no ErsatzTV installed.
type Fake struct {
	mu       sync.Mutex
	channels []Channel
	guide    XMLTV
	// Err, when set, is returned by Channels and Ping, simulating a server that
	// is down or still starting.
	Err error
	// Base is reported by BaseURL.
	Base string
	// calls counts Channels invocations so tests can assert on refresh behaviour.
	calls int
}

// NewFake returns a fake with the given channels.
func NewFake(channels ...Channel) *Fake {
	return &Fake{channels: channels, Base: "http://fake.ersatztv"}
}

// Channels returns the configured channels.
func (f *Fake) Channels(context.Context) ([]Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.Err != nil {
		return nil, f.Err
	}
	out := append([]Channel(nil), f.channels...)
	SortChannels(out)
	return out, nil
}

// SetChannels replaces the channel list, standing in for a user editing their
// channels in the ErsatzTV web UI.
func (f *Fake) SetChannels(chs ...Channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = chs
}

// SetError makes subsequent calls fail.
func (f *Fake) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}

// CallCount reports how many times Channels has been called.
func (f *Fake) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// StreamURL builds a predictable fake URL.
func (f *Fake) StreamURL(c Channel) string {
	return f.BaseURL() + "/iptv/channel/" + c.Number + ".m3u8"
}

// Guide returns the configured guide.
func (f *Fake) Guide(context.Context) (XMLTV, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return XMLTV{}, f.Err
	}
	if f.guide.Programmes == nil {
		return XMLTV{Programmes: map[string][]Programme{}, Numbers: map[string]string{}}, nil
	}
	return f.guide, nil
}

// SetGuide replaces the schedule the fake serves.
func (f *Fake) SetGuide(x XMLTV) {
	f.mu.Lock()
	f.guide = x
	f.mu.Unlock()
}

// Ping reports the configured error.
func (f *Fake) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Err
}

// BaseURL reports the fake address.
func (f *Fake) BaseURL() string {
	if f.Base == "" {
		return "http://fake.ersatztv"
	}
	return f.Base
}

var _ API = (*Fake)(nil)
