// Package guide is the television's guide mode: the listings, in the style of
// the old cable guide channel, with the channel the channel knob points at
// playing in the top-left corner.
//
// The corner video is the TV mode's own playback, borrowed rather than
// reimplemented, so it gets the same reloading, stall recovery and giving up
// as the full-screen picture. The guide adds the layout around it.
package guide

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/screen"
	"github.com/bradsheets/timeblaster/internal/system"
)

// OverlayID is the guide's osd-overlay slot, apart from the channel banner's
// so neither can wipe the other.
const OverlayID = 2

// TV is the television mode the guide borrows its picture from: the media
// service.
type TV interface {
	Activate()
	Deactivate()
	SetQuiet(quiet bool)
	SelectBand(ctx context.Context, band int) error
	RestorePlayback(ctx context.Context)
	HandlePlayerEvent(ev mpv.Event)
	ChannelCount() int
	Showing() screen.Showing
	Guide(ctx context.Context, window time.Duration) (ersatztv.Guide, error)
}

// Options configures the guide.
type Options struct {
	// PageInterval is how long each page of channels stays up when there are
	// more than fit at once.
	PageInterval time.Duration
	// RefreshInterval is how often the listings are re-read.
	RefreshInterval time.Duration
	CommandTimeout  time.Duration
	// Clock24h and Location follow the user's settings as they change.
	Clock24h func() bool
	Location func() *time.Location
}

// Deps are the guide's collaborators.
type Deps struct {
	Player mpv.Controller
	TV     TV
	Clock  system.Clock
	Logger *slog.Logger
}

// Mode is the guide.
type Mode struct {
	opts   Options
	player mpv.Controller
	tv     TV
	clock  system.Clock
	log    *slog.Logger

	gate screen.Gate

	mu sync.Mutex
	// active is set between Activate and Deactivate. Deactivate only hands
	// the TV back if the guide had it.
	active bool
	// laidOut means the picture is in the corner; drawn is the overlay last
	// sent, so an unchanged guide is not resent every second.
	laidOut bool
	drawn   string
	// listings are the last ones read, kept through a failed refresh.
	listings  []ersatztv.GuideChannel
	fetchedAt time.Time
	// page, and when it was turned.
	page    int
	pageAt  time.Time
	playing string
}

var _ screen.Mode = (*Mode)(nil)

// New builds the guide. It starts inactive.
func New(opts Options, d Deps) *Mode {
	if opts.PageInterval <= 0 {
		opts.PageInterval = 8 * time.Second
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = time.Minute
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 5 * time.Second
	}
	if opts.Clock24h == nil {
		opts.Clock24h = func() bool { return false }
	}
	if opts.Location == nil {
		opts.Location = func() *time.Location { return time.Local }
	}
	return &Mode{opts: opts, player: d.Player, tv: d.TV, clock: d.Clock, log: d.Logger}
}

// ID names this mode in configuration and the API.
func (m *Mode) ID() string { return "guide" }

// Title names this mode for people.
func (m *Mode) Title() string { return "Guide" }

// ChannelCount is the TV's: the channel knob chooses the corner channel.
func (m *Mode) ChannelCount() int { return m.tv.ChannelCount() }

// Showing is the corner channel.
func (m *Mode) Showing() screen.Showing { return m.tv.Showing() }

// Activate takes the screen, and the TV's playback with it.
func (m *Mode) Activate() {
	m.tv.Activate()
	m.tv.SetQuiet(true)
	m.mu.Lock()
	m.active = true
	m.laidOut, m.drawn = false, ""
	m.fetchedAt = time.Time{} // read fresh listings on the way in
	m.mu.Unlock()
	m.gate.Open()
}

// Deactivate gives the screen back: the TV's playback stops, the picture
// returns to full size, and the guide is taken down -- all before the next
// mode starts, so it never inherits a shrunken picture or a guide drawn over
// its own.
func (m *Mode) Deactivate() {
	m.gate.Close()
	m.mu.Lock()
	was := m.active
	m.active = false
	laidOut, drawn := m.laidOut, m.drawn != ""
	m.laidOut, m.drawn = false, ""
	m.mu.Unlock()
	if !was {
		return
	}

	m.tv.Deactivate()
	m.tv.SetQuiet(false)

	ctx, cancel := context.WithTimeout(context.Background(), m.opts.CommandTimeout)
	defer cancel()
	if drawn {
		if err := m.clearOverlay(ctx); err != nil {
			m.log.Debug("could not take the guide down", "error", err)
		}
	}
	if laidOut {
		if err := m.setLayout(ctx, false); err != nil {
			m.log.Warn("could not return the picture to full size", "error", err)
		}
	}
}

// SelectBand puts a channel in the corner.
func (m *Mode) SelectBand(ctx context.Context, band int) error {
	// The guide first: it is quick, and the screen is a guide from the first
	// moment rather than after the stream has started.
	if err := m.draw(ctx, true); err != nil {
		return err
	}
	err := m.tv.SelectBand(ctx, band)
	m.draw(ctx, false) //nolint:errcheck // the highlight follows; a failure was logged
	return err
}

// RestorePlayback lays the guide out again after mpv restarts, which forgets
// both the picture's size and every overlay, then restores the picture.
func (m *Mode) RestorePlayback(ctx context.Context) {
	m.mu.Lock()
	m.laidOut, m.drawn = false, ""
	m.mu.Unlock()
	if err := m.draw(ctx, true); err != nil && !errors.Is(err, screen.ErrInactive) {
		m.log.Warn("could not restore the guide after a player restart", "error", err)
	}
	m.tv.RestorePlayback(ctx)
}

// HandlePlayerEvent goes to the TV's playback, which owns the corner video.
func (m *Mode) HandlePlayerEvent(ev mpv.Event) {
	if m.gate.IsOpen() {
		m.tv.HandlePlayerEvent(ev)
	}
}

// Run keeps the guide current -- the clock, the listings, the pages -- until
// the context is cancelled. It redraws only what has changed.
func (m *Mode) Run(ctx context.Context) error {
	for {
		timer := m.clock.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C():
		}
		if !m.gate.IsOpen() {
			continue
		}
		if err := m.draw(ctx, false); err != nil && !errors.Is(err, screen.ErrInactive) {
			m.log.Debug("could not redraw the guide", "error", err)
		}
	}
}

// draw renders the guide and sends it if it differs from what is on screen.
// layout also puts the picture in the corner, which only needs doing once per
// activation.
func (m *Mode) draw(ctx context.Context, layout bool) error {
	frame := m.frame(ctx)
	ass := Render(frame)

	return m.gate.Do(func() error {
		m.mu.Lock()
		needLayout := layout && !m.laidOut
		same := ass == m.drawn
		m.mu.Unlock()

		cmdCtx, cancel := context.WithTimeout(ctx, m.opts.CommandTimeout)
		defer cancel()
		if needLayout {
			if err := m.setLayout(cmdCtx, true); err != nil {
				return err
			}
			m.mu.Lock()
			m.laidOut = true
			m.mu.Unlock()
		}
		if same {
			return nil
		}
		if _, err := m.player.Command(cmdCtx, "osd-overlay", OverlayID, "ass-events", ass,
			mpv.OverlayResX, mpv.OverlayResY, 0, false, false); err != nil {
			return err
		}
		m.mu.Lock()
		m.drawn = ass
		m.mu.Unlock()
		return nil
	})
}

// frame gathers what the guide shows right now, turning the page if it is
// time.
func (m *Mode) frame(ctx context.Context) Frame {
	now := m.clock.Now().In(m.opts.Location())
	playing := m.tv.Showing().Number

	m.mu.Lock()
	stale := m.fetchedAt.IsZero() || m.clock.Since(m.fetchedAt) >= m.opts.RefreshInterval
	m.mu.Unlock()
	if stale {
		g, err := m.tv.Guide(ctx, 3*time.Hour)
		m.mu.Lock()
		// Tried either way: a down ErsatzTV is asked again in a minute, not
		// every second.
		m.fetchedAt = m.clock.Now()
		if err == nil {
			m.listings = g.Channels
		}
		m.mu.Unlock()
		if err != nil {
			m.log.Debug("could not read the listings; showing the last ones", "error", err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	pages := Pages(len(m.listings))
	switch {
	case playing != m.playing:
		// A new corner channel: show its page, and give it a full turn.
		m.playing = playing
		if p := PageOf(m.listings, playing); p >= 0 {
			m.page = p
		}
		m.pageAt = m.clock.Now()
	case pages > 1 && m.clock.Since(m.pageAt) >= m.opts.PageInterval:
		m.page = (m.page + 1) % pages
		m.pageAt = m.clock.Now()
	}
	return Frame{
		Now:      now,
		Clock24h: m.opts.Clock24h(),
		Channels: m.listings,
		Playing:  playing,
		Page:     m.page,
	}
}

// setLayout puts the picture in the top-left quarter, or back to full size.
func (m *Mode) setLayout(ctx context.Context, corner bool) error {
	margin := 0.0
	if corner {
		margin = 0.5
	}
	for _, p := range []string{"video-margin-ratio-right", "video-margin-ratio-bottom"} {
		if err := m.player.SetProperty(ctx, p, margin); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mode) clearOverlay(ctx context.Context) error {
	_, err := m.player.Command(ctx, "osd-overlay", OverlayID, "none", "",
		mpv.OverlayResX, mpv.OverlayResY, 0, false, false)
	return err
}
