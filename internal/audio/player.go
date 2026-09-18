package audio

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bradsheets/timeblaster/internal/mpv"
)

// PlayOptions describe one playback request.
type PlayOptions struct {
	// Device is the ALSA device string for the alarm speaker, e.g.
	// "alsa/hw:CARD=Device,DEV=0". It is always explicit: playing an alarm out of
	// whatever Linux considers the default would send it to the television.
	Device string
	// Loop repeats the sound until stopped, which is what an alarm does.
	Loop bool
	// VolumePercent is the initial software volume.
	VolumePercent int
	// ExtraArgs are appended to the command line from configuration.
	ExtraArgs []string
}

// Handle controls one in-flight playback.
type Handle interface {
	// Stop ends playback. It is safe to call more than once.
	Stop() error
	// Done is closed when playback ends for any reason.
	Done() <-chan struct{}
	// SetVolume adjusts the player's software volume, used when the USB device
	// has no hardware mixer control.
	SetVolume(pct int) error
	// Err reports why playback ended, or nil if it was stopped deliberately.
	Err() error
}

// Player starts playback. Keeping it an interface is what lets the whole alarm
// path be tested without spawning processes or owning a sound card.
type Player interface {
	Play(ctx context.Context, path string, opts PlayOptions) (Handle, error)
}

// MPVPlayerOptions configure the mpv-backed player.
type MPVPlayerOptions struct {
	// Binary is the player executable.
	Binary string
	// SocketDir is where per-playback IPC sockets are created.
	SocketDir string
	// CommandTimeout bounds a single IPC request.
	CommandTimeout time.Duration
	// StartupTimeout bounds waiting for the IPC socket to appear. Volume control
	// needs IPC, but playback itself does not, so exceeding this is logged rather
	// than treated as a failure: audible-but-unadjustable beats silent.
	StartupTimeout time.Duration
}

// MPVPlayer plays audio through a dedicated, audio-only mpv process.
//
// mpv is reused rather than decoding MP3 in Go because it is already installed
// for video, handles every container a user might drop in, and exposes the same
// JSON IPC the TV instance uses. The process is entirely separate from the TV
// instance: different process, different socket, different audio device.
type MPVPlayer struct {
	opts MPVPlayerOptions
	log  *slog.Logger
}

// NewMPVPlayer creates the production player.
func NewMPVPlayer(opts MPVPlayerOptions, log *slog.Logger) *MPVPlayer {
	if opts.Binary == "" {
		opts.Binary = "mpv"
	}
	if opts.SocketDir == "" {
		opts.SocketDir = "/run/timeblaster"
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 3 * time.Second
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 3 * time.Second
	}
	return &MPVPlayer{opts: opts, log: log}
}

// BuildArgs produces the mpv command line for a playback request.
//
// Exported and pure so the generated command can be asserted in tests: the
// difference between "--audio-device=alsa/hw:CARD=Device,DEV=0" and the default
// device is the difference between the bedside speaker and the television, and
// that is not something to discover at 06:30.
func BuildArgs(path string, opts PlayOptions, socket string) []string {
	args := []string{
		"--no-video",
		"--no-terminal",
		"--no-input-default-bindings",
		"--no-config", // never let a stray ~/.config/mpv change alarm behaviour
		"--msg-level=all=warn",
		"--audio-display=no",
		"--idle=no",
		"--keep-open=no",
	}
	if socket != "" {
		args = append(args, "--input-ipc-server="+socket)
	}
	if opts.Device != "" {
		args = append(args, "--ao=alsa", "--audio-device="+opts.Device)
	}
	if opts.Loop {
		args = append(args, "--loop-file=inf")
	}
	if opts.VolumePercent >= 0 {
		args = append(args, "--volume="+strconv.Itoa(clampPercent(opts.VolumePercent)))
	}
	args = append(args, opts.ExtraArgs...)
	// The file always comes last so no option can be mistaken for it.
	return append(args, path)
}

// Play starts an mpv process for the given file.
func (p *MPVPlayer) Play(ctx context.Context, path string, opts PlayOptions) (Handle, error) {
	socket := filepath.Join(p.opts.SocketDir, fmt.Sprintf("alarm-%d.sock", time.Now().UnixNano()))
	if err := os.MkdirAll(p.opts.SocketDir, 0o750); err != nil {
		// Not fatal: without IPC we lose software volume control, not playback.
		p.log.Warn("could not create the alarm IPC socket directory; software volume will be unavailable",
			"dir", p.opts.SocketDir, "error", err)
		socket = ""
	}

	args := BuildArgs(path, opts, socket)
	// Deliberately not exec.CommandContext: an alarm must not be killed because
	// the HTTP request that started it was cancelled. Stop() is the only way out.
	cmd := exec.Command(p.opts.Binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("audio: starting %s: %w", p.opts.Binary, err)
	}
	p.log.Info("alarm player started",
		"pid", cmd.Process.Pid, "file", path, "device", opts.Device, "loop", opts.Loop)

	h := &mpvHandle{
		cmd:    cmd,
		socket: socket,
		log:    p.log,
		done:   make(chan struct{}),
	}
	go h.wait()

	// Connect for volume control in the background so Play returns immediately:
	// the alarm should be audible the instant mpv opens the device.
	if socket != "" {
		go h.connect(p.opts.StartupTimeout, p.opts.CommandTimeout)
	}
	return h, nil
}

type mpvHandle struct {
	cmd    *exec.Cmd
	socket string
	log    *slog.Logger

	mu       sync.Mutex
	client   *mpv.Client
	stopped  bool
	err      error
	lastVol  int
	haveVol  bool
	done     chan struct{}
	doneOnce sync.Once
}

func (h *mpvHandle) wait() {
	err := h.cmd.Wait()

	h.mu.Lock()
	stopped := h.stopped
	if !stopped {
		h.err = err
	}
	client := h.client
	h.mu.Unlock()

	if client != nil {
		client.Close()
	}
	if h.socket != "" {
		_ = os.Remove(h.socket)
	}
	h.doneOnce.Do(func() { close(h.done) })
}

func (h *mpvHandle) connect(startupTimeout, cmdTimeout time.Duration) {
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
		client, err := mpv.Dial(ctx, h.socket, cmdTimeout, h.log)
		cancel()
		if err == nil {
			h.mu.Lock()
			h.client = client
			pending, have := h.lastVol, h.haveVol
			h.mu.Unlock()
			// Apply any volume change that arrived while we were connecting.
			if have {
				_ = h.applyVolume(pending)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.log.Debug("alarm player IPC did not become ready; software volume is unavailable",
		"socket", h.socket)
}

func (h *mpvHandle) applyVolume(pct int) error {
	h.mu.Lock()
	client := h.client
	h.mu.Unlock()
	if client == nil {
		return nil // queued; connect() will apply it
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return client.SetProperty(ctx, "volume", clampPercent(pct))
}

// SetVolume adjusts the player's software volume.
func (h *mpvHandle) SetVolume(pct int) error {
	h.mu.Lock()
	h.lastVol, h.haveVol = pct, true
	h.mu.Unlock()
	return h.applyVolume(pct)
}

// Stop terminates playback.
func (h *mpvHandle) Stop() error {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	client := h.client
	h.mu.Unlock()

	// Ask politely over IPC first so mpv closes the ALSA device cleanly; a device
	// left open by SIGKILL can refuse the next open with EBUSY.
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = client.Command(ctx, "quit")
		cancel()
		select {
		case <-h.done:
			return nil
		case <-time.After(750 * time.Millisecond):
		}
	}
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-h.done:
			return nil
		case <-time.After(time.Second):
			_ = h.cmd.Process.Kill()
		}
	}
	return nil
}

// Done is closed when playback ends.
func (h *mpvHandle) Done() <-chan struct{} { return h.done }

// Err reports why playback ended.
func (h *mpvHandle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

var (
	_ Player = (*MPVPlayer)(nil)
	_ Handle = (*mpvHandle)(nil)
)
