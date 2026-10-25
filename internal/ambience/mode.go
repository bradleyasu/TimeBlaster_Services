package ambience

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/bradsheets/timeblaster/internal/mpv"
	"github.com/bradsheets/timeblaster/internal/screen"
	"github.com/bradsheets/timeblaster/internal/system"
)

// How hard to keep trying a channel whose videos will not play.
const (
	// maxFailures counts consecutive failures since a picture last appeared,
	// on top of one per video: every file in the folder gets its chance, and
	// a folder where none of them play is given up on rather than cycled
	// through forever.
	maxFailures  = 2
	errorBackoff = 2 * time.Second
	loadTimeout  = 30 * time.Second
)

// Messages shown over the standby image when there is nothing to play.
const (
	msgNoVideos = "NO AMBIENCE VIDEOS"
)

// Observer is told what the mode is doing.
type Observer interface {
	// AmbienceChannelsChanged reports a new set of channel folders.
	AmbienceChannelsChanged(channels []Channel)
	// AmbienceChannelChanged reports the channel now playing, or nil.
	AmbienceChannelChanged(ch *Channel)
}

// Options configures the mode.
type Options struct {
	// Dir holds the channel folders.
	Dir string
	// RescanInterval is how often Dir is re-read.
	RescanInterval time.Duration
	// StandbyImage is shown when there is nothing to play.
	StandbyImage string
	// BannerDuration is how long the channel banner stays up.
	BannerDuration time.Duration
	// StallTimeout and StallCheckInterval drive the stall watchdog, as for
	// the TV. A zero timeout disables it. StallStartTimeout is the longer
	// allowance before a video's first picture.
	StallTimeout       time.Duration
	StallCheckInterval time.Duration
	StallStartTimeout  time.Duration
	CommandTimeout     time.Duration
}

// Deps are the mode's collaborators.
type Deps struct {
	Player   mpv.Controller
	Overlay  *mpv.OverlayRenderer
	Clock    system.Clock
	Logger   *slog.Logger
	Observer Observer
}

// playback is the channel on screen and where it is in its videos.
type playback struct {
	ch    Channel
	files []string
	idx   int
	// started is set by playback-restart: the file produced a picture. An
	// end-of-file before that is a file that could not play.
	started bool
}

// Mode plays ambience videos.
type Mode struct {
	opts    Options
	player  mpv.Controller
	overlay *mpv.OverlayRenderer
	clock   system.Clock
	log     *slog.Logger
	obs     Observer

	gate screen.Gate

	mu       sync.Mutex
	channels []Channel
	scanned  bool
	skipped  []string
	playing  *playback
	// gen changes with every selection. Work started for one selection --
	// moving to the next video, recovering a stall -- checks it inside the
	// gate and does nothing if the selection has moved on.
	gen      uint64
	failures int
	// message is the text over the standby image, when that is what is
	// showing; empty for a plain standby screen.
	message   string
	onStandby bool
	// overlayHeld marks a message left on screen indefinitely, which has to be
	// taken down when another mode takes over.
	overlayHeld bool
}

var _ screen.Mode = (*Mode)(nil)

// New builds the mode. It starts inactive; the screen director activates it.
func New(opts Options, d Deps) *Mode {
	if opts.RescanInterval <= 0 {
		opts.RescanInterval = 30 * time.Second
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 5 * time.Second
	}
	m := &Mode{
		opts: opts, player: d.Player, overlay: d.Overlay,
		clock: d.Clock, log: d.Logger, obs: d.Observer,
	}
	m.rescan(false)
	return m
}

// ID names this mode in configuration and the API.
func (m *Mode) ID() string { return "ambience" }

// Title names this mode for people.
func (m *Mode) Title() string { return "Ambience" }

// SetObserver installs the observer after construction.
func (m *Mode) SetObserver(o Observer) {
	m.mu.Lock()
	m.obs = o
	m.mu.Unlock()
}

// Channels returns the channel folders, in knob order.
func (m *Mode) Channels() []Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Channel(nil), m.channels...)
}

// ChannelCount is how many positions the channel knob has.
func (m *Mode) ChannelCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.channels)
}

// Showing reports the channel playing.
func (m *Mode) Showing() screen.Showing {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.playing == nil {
		return screen.Showing{}
	}
	return screen.Showing{Number: strconv.Itoa(m.playing.ch.Number), Name: m.playing.ch.Name}
}

// Activate lets the mode use the player.
func (m *Mode) Activate() { m.gate.Open() }

// Deactivate hands the player to another mode.
func (m *Mode) Deactivate() {
	m.gate.Close()
	m.mu.Lock()
	m.gen++
	m.playing = nil
	m.onStandby, m.message = false, ""
	held := m.overlayHeld
	m.overlayHeld = false
	m.mu.Unlock()

	// A message such as "no ambience videos" stays up until replaced. The next
	// mode might not replace it -- the TV's standby screen has no overlay of
	// its own -- so it is taken down on the way out.
	if held && m.overlay != nil {
		ctx, cancel := context.WithTimeout(context.Background(), m.opts.CommandTimeout)
		if err := m.overlay.Hide(ctx); err != nil {
			m.log.Debug("could not take down the ambience message", "error", err)
		}
		cancel()
	}
}

// SelectBand plays the channel in a channel-knob band.
func (m *Mode) SelectBand(ctx context.Context, band int) error {
	m.mu.Lock()
	channels := m.channels
	m.mu.Unlock()

	if len(channels) == 0 {
		return m.showStandby(ctx, msgNoVideos)
	}
	if band < 0 {
		// The knob has not said where it is. Nothing to choose with yet.
		return m.showStandby(ctx, "")
	}
	if band >= len(channels) {
		// Past the end of a list that shrank: the last channel, not a blank.
		band = len(channels) - 1
	}
	ch := channels[band]

	m.mu.Lock()
	same := m.playing != nil && m.playing.ch == ch
	m.mu.Unlock()
	if same {
		return nil
	}
	return m.start(ctx, ch)
}

// start plays a channel from its first video.
func (m *Mode) start(ctx context.Context, ch Channel) error {
	files, err := Videos(ch.Dir)
	if err != nil || len(files) == 0 {
		// Emptied since the last scan. The scan will catch up; meanwhile say
		// so rather than show a blank.
		m.log.Warn("ambience channel has no videos to play", "channel", ch.Number, "dir", ch.Dir, "error", err)
		return m.showStandby(ctx, fmt.Sprintf("AMBIENCE %d IS EMPTY", ch.Number))
	}

	err = m.gate.Do(func() error {
		// The banner first, as for the TV, so it is up before the picture
		// changes rather than after.
		if m.overlay != nil {
			if err := m.overlay.ShowText(ctx, ch.Label(), m.opts.BannerDuration); err != nil {
				m.log.Debug("could not draw the ambience banner", "error", err)
			}
		}
		if err := m.load(ctx, files, 0); err != nil {
			return err
		}
		m.mu.Lock()
		m.gen++
		m.playing = &playback{ch: ch, files: files}
		m.failures = 0
		m.onStandby, m.message, m.overlayHeld = false, "", false
		m.mu.Unlock()
		return nil
	})
	if err != nil {
		if !errors.Is(err, screen.ErrInactive) {
			m.log.Error("could not start an ambience channel", "channel", ch.Number, "error", err)
		}
		return err
	}

	m.log.Info("ambience channel", "channel", ch.Number, "name", ch.Name, "videos", len(files))
	m.notifyChannel(&ch)
	return nil
}

// load puts one of a channel's videos on screen. A channel of one video loops
// it inside mpv, which is seamless; the loop is a per-file option, so it ends
// with the file and can never carry over to the next mode's picture.
func (m *Mode) load(ctx context.Context, files []string, idx int) error {
	path := files[idx]
	if len(files) == 1 {
		_, err := m.player.Command(ctx, "loadfile", path, "replace", -1, "loop-file=inf")
		return err
	}
	return m.player.LoadFile(ctx, path)
}

// showStandby puts the standby image up, with a message over it if there is
// one. It is de-duplicated, so rescans that find nothing new do not make the
// screen flicker.
func (m *Mode) showStandby(ctx context.Context, message string) error {
	m.mu.Lock()
	already := m.onStandby && m.message == message
	m.mu.Unlock()
	if already {
		return nil
	}

	var hadChannel bool
	err := m.gate.Do(func() error {
		if m.opts.StandbyImage != "" {
			if err := m.player.LoadFile(ctx, m.opts.StandbyImage); err != nil {
				return err
			}
		} else if _, err := m.player.Command(ctx, "stop"); err != nil {
			return err
		}
		if m.overlay != nil {
			var err error
			if message != "" {
				err = m.overlay.ShowText(ctx, message, 0)
			} else {
				err = m.overlay.Hide(ctx)
			}
			if err != nil {
				m.log.Debug("could not update the ambience message", "error", err)
			}
		}
		m.mu.Lock()
		m.gen++
		hadChannel = m.playing != nil
		m.playing = nil
		m.onStandby, m.message, m.overlayHeld = true, message, message != ""
		m.mu.Unlock()
		return nil
	})
	if err != nil {
		if !errors.Is(err, screen.ErrInactive) {
			m.log.Warn("could not show the ambience standby screen", "error", err)
		}
		return err
	}
	if message != "" {
		m.log.Info("ambience has nothing to play", "message", message, "dir", m.opts.Dir)
	}
	if hadChannel {
		m.notifyChannel(nil)
	}
	return nil
}

// RestorePlayback reloads what was on screen after mpv restarts.
func (m *Mode) RestorePlayback(ctx context.Context) {
	if !m.gate.IsOpen() {
		return
	}
	m.mu.Lock()
	p := m.playing
	gen := m.gen
	message, standby := m.message, m.onStandby
	m.onStandby = false // force showStandby to redraw
	m.mu.Unlock()

	if p == nil {
		if standby {
			_ = m.showStandby(ctx, message)
		}
		return
	}
	m.log.Info("restoring ambience after a player restart", "channel", p.ch.Number)
	m.goTo(ctx, gen, p.idx)
}

// goTo loads video idx of the current selection, if the selection is still
// gen. Used to move to the next video, and to reload one that stalled.
func (m *Mode) goTo(ctx context.Context, gen uint64, idx int) {
	err := m.gate.Do(func() error {
		m.mu.Lock()
		p := m.playing
		current := m.gen == gen && p != nil
		var files []string
		if current {
			files = p.files
		}
		m.mu.Unlock()
		if !current {
			return nil // the knob moved on; this work is stale
		}
		if err := m.load(ctx, files, idx); err != nil {
			return err
		}
		m.mu.Lock()
		if m.gen == gen && m.playing != nil {
			m.playing.idx = idx
			m.playing.started = false
		}
		m.mu.Unlock()
		return nil
	})
	if err != nil && !errors.Is(err, screen.ErrInactive) {
		m.log.Warn("could not load the next ambience video", "error", err)
	}
}

// HandlePlayerEvent moves through a channel's videos as each one ends.
func (m *Mode) HandlePlayerEvent(ev mpv.Event) {
	if !m.gate.IsOpen() {
		return
	}
	switch ev.Name {
	case "playback-restart":
		// A picture: whatever was failing has stopped failing.
		m.mu.Lock()
		if m.playing != nil {
			m.playing.started = true
		}
		m.failures = 0
		m.mu.Unlock()
		if m.overlay != nil {
			m.overlay.PlaybackStarted()
		}
		return
	case "end-file":
	default:
		return
	}
	// "stop" and "redirect" are our own loads replacing the file.
	if ev.Reason != "eof" && ev.Reason != "error" {
		return
	}

	m.mu.Lock()
	p := m.playing
	if p == nil {
		m.mu.Unlock()
		return
	}
	gen := m.gen
	// An end before any picture is a file that could not play, whatever mpv
	// calls it. Counting it is what stops a folder of broken files spinning.
	failed := ev.Reason == "error" || !p.started
	if failed {
		m.failures++
	}
	failures := m.failures
	next := (p.idx + 1) % len(p.files)
	limit := len(p.files) + maxFailures
	ch := p.ch
	m.mu.Unlock()

	if failures > limit {
		m.log.Error("no video in this ambience channel will play; giving up on it",
			"channel", ch.Number, "dir", ch.Dir, "failures", failures, "error", ev.Error)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
			defer cancel()
			_ = m.showStandby(ctx, fmt.Sprintf("AMBIENCE %d WILL NOT PLAY", ch.Number))
		}()
		return
	}

	var delay time.Duration
	if failed {
		delay = errorBackoff
		m.log.Warn("an ambience video failed; moving on", "channel", ch.Number,
			"reason", ev.Reason, "error", ev.Error, "failures", failures)
	}
	go func() {
		if delay > 0 {
			<-m.clock.NewTimer(delay).C()
		}
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		m.goTo(ctx, gen, next)
	}()
}

// Run rescans the folder and watches for stalled playback until cancelled.
func (m *Mode) Run(ctx context.Context) error {
	check := m.opts.StallCheckInterval
	if m.opts.StallTimeout <= 0 {
		check = 0
	} else if check <= 0 || check >= m.opts.StallTimeout {
		check = m.opts.StallTimeout / 4
	}
	tick := check
	if tick <= 0 || tick > m.opts.RescanInterval {
		tick = m.opts.RescanInterval
	}

	nextScan := m.clock.Now().Add(m.opts.RescanInterval)
	var stall stallState
	for {
		timer := m.clock.NewTimer(tick)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C():
		}
		if !m.clock.Now().Before(nextScan) {
			m.rescan(true)
			nextScan = m.clock.Now().Add(m.opts.RescanInterval)
		}
		if check > 0 {
			m.checkStall(ctx, &stall)
		}
	}
}

// rescan re-reads the channel folders, announcing a change.
func (m *Mode) rescan(announce bool) {
	channels, skipped, err := Scan(m.opts.Dir)
	if err != nil {
		m.log.Warn("could not read the ambience folder", "dir", m.opts.Dir, "error", err)
		return
	}

	m.mu.Lock()
	changed := !m.scanned || !slices.Equal(channels, m.channels)
	newSkips := !slices.Equal(skipped, m.skipped)
	m.channels, m.skipped, m.scanned = channels, skipped, true
	p := m.playing
	obs := m.obs
	m.mu.Unlock()

	if newSkips && len(skipped) > 0 {
		m.log.Warn("ignoring some of the ambience folder", "dir", m.opts.Dir, "ignored", skipped)
	}
	if changed {
		m.log.Info("ambience channels", "dir", m.opts.Dir, "count", len(channels))
		if announce && obs != nil {
			obs.AmbienceChannelsChanged(channels)
		}
	}

	// Videos added to or removed from the channel on screen: start it again
	// so the new set plays. The knob has not moved, so nothing else would.
	if p != nil && m.gate.IsOpen() {
		files, err := Videos(p.ch.Dir)
		if err == nil && len(files) > 0 && !slices.Equal(files, p.files) {
			m.log.Info("the ambience channel's videos changed; restarting it", "channel", p.ch.Number)
			ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
			_ = m.start(ctx, p.ch)
			cancel()
		}
	}
}

// stallState is the watchdog's memory between checks.
type stallState struct {
	key     string
	lastPos float64
	movedAt time.Time
}

// checkStall reloads a video whose playback has stopped advancing, the same
// failure the TV's watchdog exists for: the USB speaker can wedge mpv's audio
// output and freeze the picture with no event at all.
func (m *Mode) checkStall(ctx context.Context, st *stallState) {
	now := m.clock.Now()
	m.mu.Lock()
	p := m.playing
	gen := m.gen
	var key string
	var idx int
	stallAfter := m.opts.StallTimeout
	if p != nil {
		idx = p.idx
		key = fmt.Sprintf("%d/%d", gen, idx)
		if !p.started && m.opts.StallStartTimeout > stallAfter {
			// Not stalled, starting: the longer allowance, as for the TV.
			stallAfter = m.opts.StallStartTimeout
		}
	}
	m.mu.Unlock()

	if p == nil || !m.gate.IsOpen() || !m.player.Alive() {
		st.key, st.movedAt = "", now
		return
	}
	if key != st.key {
		// A new video starts its own clock.
		st.key, st.lastPos, st.movedAt = key, -1, now
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, m.opts.CommandTimeout)
	pos, state := mpv.ProbePosition(probeCtx, m.player)
	cancel()
	switch state {
	case mpv.PositionHalted:
		st.movedAt = now
		return
	case mpv.PositionKnown:
		// A looping video jumps back to zero, which is movement too.
		if pos != st.lastPos {
			st.lastPos, st.movedAt = pos, now
			return
		}
	}
	if now.Sub(st.movedAt) < stallAfter {
		return
	}

	m.mu.Lock()
	m.failures++
	failures := m.failures
	limit := len(p.files) + maxFailures
	m.mu.Unlock()

	if failures > limit {
		m.log.Error("ambience playback keeps stalling; giving up on the channel", "channel", p.ch.Number)
		_ = m.showStandby(ctx, fmt.Sprintf("AMBIENCE %d WILL NOT PLAY", p.ch.Number))
		st.key = ""
		return
	}
	m.log.Warn("ambience playback has stopped advancing; reloading the video",
		"channel", p.ch.Number, "video", p.files[idx], "stalled_for", now.Sub(st.movedAt).Round(time.Second))
	m.goTo(ctx, gen, idx)
	st.lastPos, st.movedAt = -1, now
}

func (m *Mode) notifyChannel(ch *Channel) {
	m.mu.Lock()
	obs := m.obs
	m.mu.Unlock()
	if obs != nil {
		obs.AmbienceChannelChanged(ch)
	}
}
