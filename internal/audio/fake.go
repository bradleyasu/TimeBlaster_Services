package audio

import (
	"context"
	"errors"
	"sync"
)

// FakePlayer is a Player for tests. It records playback requests and lets a test
// end or fail a playback on demand.
type FakePlayer struct {
	mu      sync.Mutex
	plays   []PlayRecord
	handles []*FakeHandle
	// Err, when set, makes Play fail, standing in for a missing USB device.
	Err error
}

// PlayRecord is one recorded playback request.
type PlayRecord struct {
	Path string
	Opts PlayOptions
}

// NewFakePlayer returns an initialised fake player.
func NewFakePlayer() *FakePlayer { return &FakePlayer{} }

// Play records the request and returns a controllable handle.
func (f *FakePlayer) Play(_ context.Context, path string, opts PlayOptions) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	f.plays = append(f.plays, PlayRecord{Path: path, Opts: opts})
	h := &FakeHandle{done: make(chan struct{}), volume: opts.VolumePercent}
	f.handles = append(f.handles, h)
	return h, nil
}

// Plays returns every recorded request.
func (f *FakePlayer) Plays() []PlayRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PlayRecord(nil), f.plays...)
}

// LastPlay returns the most recent request.
func (f *FakePlayer) LastPlay() (PlayRecord, bool) {
	plays := f.Plays()
	if len(plays) == 0 {
		return PlayRecord{}, false
	}
	return plays[len(plays)-1], true
}

// LastHandle returns the most recently created handle.
func (f *FakePlayer) LastHandle() *FakeHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.handles) == 0 {
		return nil
	}
	return f.handles[len(f.handles)-1]
}

// SetError makes subsequent Play calls fail.
func (f *FakePlayer) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}

// FakeHandle is a controllable playback handle.
type FakeHandle struct {
	mu       sync.Mutex
	stopped  bool
	volume   int
	err      error
	done     chan struct{}
	doneOnce sync.Once
	// VolumeErr makes SetVolume fail.
	VolumeErr error
}

// Stop ends the fake playback.
func (h *FakeHandle) Stop() error {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	h.doneOnce.Do(func() { close(h.done) })
	return nil
}

// Done reports playback completion.
func (h *FakeHandle) Done() <-chan struct{} { return h.done }

// SetVolume records a software volume change.
func (h *FakeHandle) SetVolume(pct int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.VolumeErr != nil {
		return h.VolumeErr
	}
	h.volume = pct
	return nil
}

// Err reports the failure that ended playback.
func (h *FakeHandle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Volume reports the last software volume applied.
func (h *FakeHandle) Volume() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.volume
}

// Stopped reports whether Stop was called.
func (h *FakeHandle) Stopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

// Crash ends the playback with an error, standing in for a player process that
// died mid-alarm.
func (h *FakeHandle) Crash(err error) {
	if err == nil {
		err = errors.New("player exited")
	}
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	h.doneOnce.Do(func() { close(h.done) })
}

// Finish ends the playback cleanly.
func (h *FakeHandle) Finish() {
	h.doneOnce.Do(func() { close(h.done) })
}

var (
	_ Player = (*FakePlayer)(nil)
	_ Handle = (*FakeHandle)(nil)
)
