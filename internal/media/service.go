// Package media owns what appears on the television: which channel is selected,
// what mpv is told to play, and the channel-change overlay.
//
// It is the only package that knows both about ErsatzTV and about mpv. Nothing
// in the alarm path depends on it, so every failure mode here — no channels, a
// dead ErsatzTV, a crashed mpv, a stream that will not load — degrades the
// television and nothing else.
package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/system"
)

// Observer is notified when the channel or channel list changes, so the input
// router can re-band the knob and the web layer can push an update.
type Observer interface {
	// ChannelListChanged reports a new channel count and list.
	ChannelListChanged(channels []ersatztv.Channel)
	// ChannelChanged reports the newly selected channel, or nil for none.
	ChannelChanged(ch *ersatztv.Channel)
}

// Status describes the television subsystem for the health endpoint.
type Status struct {
	// ErsatzTVReachable is false while the server is down or starting.
	ErsatzTVReachable bool `json:"ersatztv_reachable"`
	// ErsatzTVURL is the configured server address.
	ErsatzTVURL string `json:"ersatztv_url"`
	// ChannelCount is how many channels the knob is divided across.
	ChannelCount int `json:"channel_count"`
	// CurrentChannel is the selected channel number, empty when none.
	CurrentChannel string `json:"current_channel,omitempty"`
	// CurrentChannelName is its display name.
	CurrentChannelName string `json:"current_channel_name,omitempty"`
	// PlayerAlive reports whether mpv is controllable.
	PlayerAlive bool `json:"player_alive"`
	// LastRefresh is when the channel list was last read successfully.
	LastRefresh time.Time `json:"last_refresh,omitzero"`
	// LastError describes the most recent failure.
	LastError string `json:"last_error,omitempty"`
}

// ErrNoChannels means no channel is available to select.
var ErrNoChannels = errors.New("media: no channels are available")

// stoppedScreen marks "playback stopped" in shownImage. It is a sentinel rather
// than an empty string so that the no-image-configured case is still
// de-duplicated: without it, every failed refresh would re-issue a stop.
const stoppedScreen = "\x00stopped"

// Deps are the service's collaborators.
type Deps struct {
	ErsatzTV ersatztv.API
	Player   mpv.Controller
	Overlay  *mpv.OverlayRenderer
	Clock    system.Clock
	Logger   *slog.Logger
	Observer Observer
}

// Service selects and plays channels.
type Service struct {
	etvCfg config.ErsatzTV
	mpvCfg config.MPV

	etv     ersatztv.API
	player  mpv.Controller
	overlay *mpv.OverlayRenderer
	clock   system.Clock
	log     *slog.Logger

	mu       sync.RWMutex
	observer Observer
	channels []ersatztv.Channel
	current  *ersatztv.Channel
	// shownImage is the static image currently on screen, so we do not reload it
	// on every failed refresh and make the television flicker. Empty means a
	// channel is playing, or nothing has been shown yet.
	shownImage string
	// ready becomes true once the channel list has been read successfully. Until
	// then the television shows the booting screen rather than inviting the user
	// to turn a knob that cannot do anything yet.
	ready        bool
	bootDeadline time.Time
	reachable    bool
	lastRefresh  time.Time
	lastErr      error

	refreshNow chan struct{}
}

// NewService builds the media service.
func NewService(etvCfg config.ErsatzTV, mpvCfg config.MPV, d Deps) *Service {
	timeout := mpvCfg.BootingTimeout.Duration
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &Service{
		etvCfg:       etvCfg,
		mpvCfg:       mpvCfg,
		etv:          d.ErsatzTV,
		player:       d.Player,
		overlay:      d.Overlay,
		clock:        d.Clock,
		log:          d.Logger,
		observer:     d.Observer,
		bootDeadline: d.Clock.Now().Add(timeout),
		refreshNow:   make(chan struct{}, 1),
	}
}

// SetObserver installs the observer after construction.
func (s *Service) SetObserver(o Observer) {
	s.mu.Lock()
	s.observer = o
	s.mu.Unlock()
}

// Channels returns the current channel list.
func (s *Service) Channels() []ersatztv.Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ersatztv.Channel(nil), s.channels...)
}

// ChannelCount returns how many channels are available.
func (s *Service) ChannelCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.channels)
}

// Current returns the selected channel, or nil.
func (s *Service) Current() *ersatztv.Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return nil
	}
	cp := *s.current
	return &cp
}

// Status reports the subsystem state.
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{
		ErsatzTVReachable: s.reachable,
		ErsatzTVURL:       s.etv.BaseURL(),
		ChannelCount:      len(s.channels),
		PlayerAlive:       s.player.Alive(),
		LastRefresh:       s.lastRefresh,
	}
	if s.current != nil {
		st.CurrentChannel = s.current.Number
		st.CurrentChannelName = s.current.Name
	}
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
	}
	return st
}

// Refresh re-reads the channel list from ErsatzTV.
func (s *Service) Refresh(ctx context.Context) error {
	channels, err := s.etv.Channels(ctx)
	if err != nil {
		s.mu.Lock()
		wasReachable := s.reachable
		s.reachable = false
		s.lastErr = err
		s.mu.Unlock()

		if wasReachable {
			s.log.Warn("lost contact with ErsatzTV; keeping the current channel list",
				"url", s.etv.BaseURL(), "error", err)
		} else {
			s.log.Debug("ErsatzTV is still unreachable", "url", s.etv.BaseURL(), "error", err)
		}
		return err
	}

	s.mu.Lock()
	wasReachable := s.reachable
	wasReady := s.ready
	changed := !channelsEqual(s.channels, channels)
	s.channels = channels
	s.reachable = true
	s.ready = true
	s.lastRefresh = s.clock.Now()
	s.lastErr = nil
	// If the selected channel disappeared, drop the selection so the knob's
	// position is re-evaluated against the new list.
	dropped := false
	if s.current != nil && findByNumber(channels, s.current.Number) == nil {
		s.current = nil
		dropped = true
	}
	obs := s.observer
	s.mu.Unlock()

	if !wasReachable {
		s.log.Info("ErsatzTV is reachable", "url", s.etv.BaseURL(), "channels", len(channels))
	}
	if changed {
		s.log.Info("channel list updated", "count", len(channels), "channels", channelLabels(channels))
		if obs != nil {
			obs.ChannelListChanged(channels)
		}
	}
	if dropped {
		s.log.Info("the selected channel no longer exists; showing the no-channel image")
		if err := s.ShowNoChannel(ctx); err != nil {
			s.log.Warn("could not show the no-channel image", "error", err)
		}
	}
	// The first successful read means the device has finished coming up, so the
	// booting screen gives way to the one that invites the knob to be turned.
	if !wasReady && !dropped && s.Current() == nil {
		if err := s.ShowNoChannel(ctx); err != nil {
			s.log.Debug("could not swap the booting screen", "error", err)
		}
	}
	return nil
}

// RefreshSoon asks the refresh loop to run immediately.
func (s *Service) RefreshSoon() {
	select {
	case s.refreshNow <- struct{}{}:
	default:
	}
}

// Run keeps the channel list current until the context is cancelled.
//
// It uses exponential backoff while ErsatzTV is unreachable, which is the normal
// state for the first thirty seconds or so after a cold boot: timeblasterd starts
// well before a .NET application has finished initialising its database.
func (s *Service) Run(ctx context.Context) error {
	backoff := s.etvCfg.RetryMinBackoff.Duration
	if backoff <= 0 {
		backoff = 2 * time.Second
	}

	for {
		err := s.Refresh(ctx)

		wait := s.etvCfg.RefreshInterval.Duration
		if err != nil {
			wait = backoff
			backoff = minDuration(backoff*2, s.etvCfg.RetryMaxBackoff.Duration)
		} else {
			backoff = s.etvCfg.RetryMinBackoff.Duration
		}
		if wait <= 0 {
			wait = 30 * time.Second
		}

		timer := s.clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C():
		case <-s.refreshNow:
			timer.Stop()
		}
	}
}

// SelectBand selects the channel in the given zero-based position band.
//
// The band comes from the channel potentiometer's absolute position, already
// filtered and hysteresis-stabilised by the input package. A band of -1, or one
// beyond the end of the list, means "no channel" and shows the static image.
func (s *Service) SelectBand(ctx context.Context, band int) error {
	s.mu.RLock()
	channels := s.channels
	s.mu.RUnlock()

	if band < 0 || band >= len(channels) {
		if len(channels) > 0 && band >= len(channels) {
			// The knob is past the end of a shrunken list; clamp to the last
			// channel rather than blanking the screen.
			band = len(channels) - 1
		} else {
			return s.ShowNoChannel(ctx)
		}
	}
	return s.SelectChannel(ctx, channels[band])
}

// SelectNumber selects a channel by its number, which is what the companion app
// and tbctl use.
func (s *Service) SelectNumber(ctx context.Context, number string) error {
	s.mu.RLock()
	ch := findByNumber(s.channels, number)
	s.mu.RUnlock()
	if ch == nil {
		return fmt.Errorf("%w: no channel numbered %q", ErrNoChannels, number)
	}
	return s.SelectChannel(ctx, *ch)
}

// SelectChannel plays a channel and shows the change overlay.
func (s *Service) SelectChannel(ctx context.Context, ch ersatztv.Channel) error {
	s.mu.Lock()
	unchanged := s.current != nil && s.current.Number == ch.Number && s.shownImage == ""
	s.mu.Unlock()
	if unchanged {
		return nil
	}

	url := s.etv.StreamURL(ch)
	s.log.Info("channel change", "number", ch.Number, "name", ch.Name, "url", url)

	if err := s.player.LoadFile(ctx, url); err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		s.log.Error("could not start the channel stream",
			"number", ch.Number, "url", url, "error", err)
		return fmt.Errorf("media: loading channel %s: %w", ch.Number, err)
	}

	s.mu.Lock()
	s.current = &ch
	s.shownImage = ""
	s.lastErr = nil
	obs := s.observer
	s.mu.Unlock()

	// The overlay is cosmetic: a failure must not turn a successful channel
	// change into an error.
	if s.overlay != nil {
		if err := s.overlay.ShowChannel(ctx, ch.Number, ch.Name); err != nil {
			s.log.Debug("could not draw the channel overlay", "error", err)
		}
	}
	if obs != nil {
		cp := ch
		obs.ChannelChanged(&cp)
	}
	return nil
}

// ShowNoChannel displays the configured static image fullscreen.
//
// This is what keeps a Linux console off the television: mpv stays running with
// the image loaded whenever no channel is selected.
func (s *Service) ShowNoChannel(ctx context.Context) error {
	image := s.standbyImage()

	shown := image
	if shown == "" {
		shown = stoppedScreen
	}

	s.mu.Lock()
	already := s.shownImage == shown && s.current == nil
	s.mu.Unlock()
	if already {
		return nil
	}

	if image == "" {
		s.log.Debug("no no-channel image is configured; stopping playback instead")
		if _, err := s.player.Command(ctx, "stop"); err != nil {
			return fmt.Errorf("media: stopping playback: %w", err)
		}
	} else if err := s.player.LoadFile(ctx, image); err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		if errors.Is(err, mpv.ErrNotConnected) {
			// Normal at startup: the daemon puts something on screen as early as
			// it can, which is usually a second or two before mpv has finished
			// claiming the display. The supervisor restores playback the moment
			// it connects, so this corrects itself. Logging it as an error would
			// put a red line in every single boot and teach you to ignore them.
			s.log.Debug("player not ready for the standby image yet; "+
				"it will be restored when mpv connects", "image", image)
		} else {
			s.log.Error("could not display the no-channel image", "image", image, "error", err)
		}
		return fmt.Errorf("media: loading %s: %w", image, err)
	}

	s.mu.Lock()
	had := s.current != nil
	s.current = nil
	s.shownImage = shown
	obs := s.observer
	s.mu.Unlock()

	if had {
		s.log.Info("no channel selected; showing the static image", "image", image)
	}
	if obs != nil {
		obs.ChannelChanged(nil)
	}
	return nil
}

// standbyImage picks which static screen belongs on the television.
//
// The booting screen stays up until the channel list has been read once, so the
// device never invites the user to turn the channel knob before there is
// anything behind it. The deadline stops it lingering forever when ErsatzTV is
// simply broken: past it the device is not booting, something is wrong, and the
// no-channel screen is the more honest thing to show.
func (s *Service) standbyImage() string {
	s.mu.RLock()
	ready := s.ready
	deadline := s.bootDeadline
	s.mu.RUnlock()

	booting := s.mpvCfg.BootingImage
	if !ready && booting != "" && s.clock.Now().Before(deadline) {
		return booting
	}
	return s.mpvCfg.NoChannelImage
}

// RestorePlayback re-establishes what should be on screen. It is called after
// mpv restarts: the supervisor hands back a fresh, idle player, and this puts
// the current channel (or the static image) back without the user noticing
// anything beyond a brief gap.
func (s *Service) RestorePlayback(ctx context.Context) {
	s.mu.Lock()
	current := s.current
	// Force a reload even though the selection has not changed.
	s.current, s.shownImage = nil, ""
	s.mu.Unlock()

	if current == nil {
		if err := s.ShowNoChannel(ctx); err != nil {
			s.log.Warn("could not restore the no-channel image after a player restart", "error", err)
		}
		return
	}
	s.log.Info("restoring playback after a player restart", "channel", current.Number)
	if err := s.SelectChannel(ctx, *current); err != nil {
		s.log.Warn("could not restore the channel after a player restart",
			"channel", current.Number, "error", err)
		if err := s.ShowNoChannel(ctx); err != nil {
			s.log.Warn("could not fall back to the no-channel image", "error", err)
		}
	}
}

// HandlePlayerEvent reacts to mpv's asynchronous events.
//
// A stream that ends or fails — ErsatzTV restarting mid-programme, a transcode
// dying — leaves mpv idle and the television black. Reloading the same channel
// is almost always the right response, and falling back to the static image
// after that fails keeps a console off the screen.
func (s *Service) HandlePlayerEvent(ev mpv.Event) {
	if ev.Name != "end-file" {
		return
	}
	switch ev.Reason {
	case "eof", "error":
	default:
		// "stop", "quit" and "redirect" are our own doing or benign.
		return
	}

	s.mu.RLock()
	current := s.current
	s.mu.RUnlock()

	if current == nil {
		// The standby image ended. With --image-display-duration=inf it should
		// not, but if it ever does the television goes black -- which is the
		// single thing this subsystem exists to prevent, so put it back rather
		// than assuming nothing is wrong.
		s.log.Warn("the standby image ended unexpectedly; restoring it", "reason", ev.Reason)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Clear what we believe is on screen, or ShowNoChannel would decide
			// the right image is already up and do nothing.
			s.mu.Lock()
			s.shownImage = ""
			s.mu.Unlock()

			if err := s.ShowNoChannel(ctx); err != nil {
				s.log.Error("could not restore the standby image", "error", err)
			}
		}()
		return
	}

	s.log.Warn("the channel stream ended unexpectedly; reloading",
		"channel", current.Number, "reason", ev.Reason, "error", ev.Error)

	go func(ch ersatztv.Channel) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		s.mu.Lock()
		s.current = nil // force SelectChannel to actually reload
		s.mu.Unlock()

		if err := s.SelectChannel(ctx, ch); err != nil {
			s.log.Error("could not reload the channel after a stream failure",
				"channel", ch.Number, "error", err)
			if err := s.ShowNoChannel(ctx); err != nil {
				s.log.Error("could not show the no-channel image", "error", err)
			}
		}
	}(*current)
}

// ChannelNumberInt returns a channel's number as an integer where possible,
// which the 7-segment display and the API find convenient.
func ChannelNumberInt(number string) (int, bool) {
	n, err := strconv.Atoi(number)
	return n, err == nil
}

func findByNumber(chs []ersatztv.Channel, number string) *ersatztv.Channel {
	for i := range chs {
		if chs[i].Number == number {
			return &chs[i]
		}
	}
	return nil
}

func channelsEqual(a, b []ersatztv.Channel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Number != b[i].Number || a[i].Name != b[i].Name || a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func channelLabels(chs []ersatztv.Channel) []string {
	out := make([]string, len(chs))
	for i, c := range chs {
		out[i] = c.Label()
	}
	return out
}

func minDuration(a, b time.Duration) time.Duration {
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}
