// Package audio owns the dedicated alarm-speaker path: which sounds exist,
// playing one, stopping it, and setting the volume on the USB audio device.
//
// It is completely independent of television playback. Starting an alarm issues
// no commands to the TV mpv instance, so the video stream keeps running while the
// alarm rings out of the bedside speaker.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Sound is one playable alarm sound.
type Sound struct {
	// ID is the stable identifier stored against an alarm: the file name without
	// its extension. Storing an id rather than a path means moving the sounds
	// directory does not orphan every alarm.
	ID string `json:"id"`
	// Name is a human-readable label derived from the file name.
	Name string `json:"name"`
	// Path is the absolute file path.
	Path string `json:"-"`
	// SizeBytes is the file size, surfaced in the API so an obviously truncated
	// file is visible without SSH.
	SizeBytes int64 `json:"size_bytes"`
	// Fallback marks the built-in generated tone rather than a user file.
	Fallback bool `json:"fallback,omitempty"`
}

// Errors returned by the library.
var (
	// ErrNoSounds means the sounds directory contains nothing playable.
	ErrNoSounds = errors.New("audio: no playable alarm sounds")
	// ErrSoundNotFound means the requested id does not exist.
	ErrSoundNotFound = errors.New("audio: sound not found")
	// ErrUnplayable means the file exists but does not look like usable audio.
	ErrUnplayable = errors.New("audio: file is not playable")
)

// supportedExtensions are the container formats mpv will happily play. MP3 is the
// documented format; the others cost nothing to allow and mean a user who drops
// in a WAV is not met with a mystery.
var supportedExtensions = map[string]bool{
	".mp3": true, ".wav": true, ".ogg": true, ".flac": true, ".m4a": true, ".aac": true,
}

// minPlayableSize rejects zero-length and obviously truncated files. A few
// hundred bytes cannot contain a usable alarm sound, and catching it here gives a
// clear error instead of silence at 06:30.
const minPlayableSize = 512

// Library enumerates and validates the alarm sounds on disk.
//
// It is a generic sound library rather than two hardcoded files: the UI currently
// offers two choices, but dropping alarm3.mp3 into the directory makes it
// available with no code change.
type Library struct {
	dir string

	mu       sync.RWMutex
	sounds   []Sound
	fallback *Sound
}

// NewLibrary creates a library over dir. Call Refresh to populate it.
func NewLibrary(dir string) *Library { return &Library{dir: dir} }

// Dir reports the directory being scanned.
func (l *Library) Dir() string { return l.dir }

// Refresh rescans the sounds directory.
//
// A missing or unreadable directory is reported but leaves any previously loaded
// list intact: an SD card hiccup should not erase the user's sound choices from
// the running daemon.
func (l *Library) Refresh() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("audio: reading sounds directory %s: %w", l.dir, err)
	}

	var sounds []Sound
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // editor swap files, macOS metadata, and so on
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !supportedExtensions[ext] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() < minPlayableSize {
			continue
		}
		id := strings.TrimSuffix(name, filepath.Ext(name))
		sounds = append(sounds, Sound{
			ID:        id,
			Name:      displayName(id),
			Path:      filepath.Join(l.dir, name),
			SizeBytes: info.Size(),
		})
	}
	sort.Slice(sounds, func(i, j int) bool { return sounds[i].ID < sounds[j].ID })

	l.mu.Lock()
	l.sounds = sounds
	l.mu.Unlock()
	return nil
}

// Sounds returns the current library, with the fallback tone appended when one
// has been registered.
func (l *Library) Sounds() []Sound {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := append([]Sound(nil), l.sounds...)
	if l.fallback != nil {
		out = append(out, *l.fallback)
	}
	return out
}

// Get looks up a sound by id.
func (l *Library) Get(id string) (Sound, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.sounds {
		if s.ID == id {
			return s, nil
		}
	}
	if l.fallback != nil && l.fallback.ID == id {
		return *l.fallback, nil
	}
	return Sound{}, fmt.Errorf("%w: %q", ErrSoundNotFound, id)
}

// SetFallback registers the built-in tone. It is kept separate from the scanned
// list so it is always available even when the sounds directory is empty or
// unreadable.
func (l *Library) SetFallback(s Sound) {
	s.Fallback = true
	l.mu.Lock()
	l.fallback = &s
	l.mu.Unlock()
}

// Fallback returns the registered fallback tone.
func (l *Library) Fallback() (Sound, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.fallback == nil {
		return Sound{}, false
	}
	return *l.fallback, true
}

// Resolve picks the sound to actually play, in this order:
//
//  1. the requested id, if it exists and is playable;
//  2. the configured default, if it exists and is playable;
//  3. any other playable sound in the library;
//  4. the built-in fallback tone.
//
// This is the rule that makes an alarm ring even after someone has deleted the
// MP3 it referred to. It returns the chosen sound plus a reason string that the
// caller logs, so a silently substituted sound is still visible in the journal.
func (l *Library) Resolve(requested, defaultID string) (Sound, string, error) {
	try := func(id string) (Sound, bool) {
		if id == "" {
			return Sound{}, false
		}
		s, err := l.Get(id)
		if err != nil {
			return Sound{}, false
		}
		if err := Validate(s); err != nil {
			return Sound{}, false
		}
		return s, true
	}

	if s, ok := try(requested); ok {
		return s, "", nil
	}
	if s, ok := try(defaultID); ok {
		return s, fmt.Sprintf("requested sound %q is unavailable; using the default %q", requested, defaultID), nil
	}
	for _, s := range l.Sounds() {
		if s.Fallback {
			continue
		}
		if err := Validate(s); err == nil {
			return s, fmt.Sprintf("requested sound %q and default %q are unavailable; using %q", requested, defaultID, s.ID), nil
		}
	}
	if s, ok := l.Fallback(); ok {
		return s, fmt.Sprintf("no usable sound files in %s; using the built-in fallback tone", l.dir), nil
	}
	return Sound{}, "", ErrNoSounds
}

// Validate checks that a sound is still playable right now. It is called at play
// time as well as at selection time, because the file can be deleted between the
// two.
func Validate(s Sound) error {
	if s.Path == "" {
		return fmt.Errorf("%w: %q has no path", ErrUnplayable, s.ID)
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnplayable, s.Path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnplayable, s.Path, err)
	}
	if info.Size() < minPlayableSize {
		return fmt.Errorf("%w: %s is only %d bytes", ErrUnplayable, s.Path, info.Size())
	}

	var head [12]byte
	if _, err := f.Read(head[:]); err != nil {
		return fmt.Errorf("%w: reading %s: %w", ErrUnplayable, s.Path, err)
	}
	if !looksLikeAudio(head[:], filepath.Ext(s.Path)) {
		return fmt.Errorf("%w: %s does not start with recognisable audio data", ErrUnplayable, s.Path)
	}
	return nil
}

// looksLikeAudio does a cheap magic-number check. It is not a decoder: the goal
// is to catch a text file or a truncated download that was renamed to .mp3,
// which is the realistic failure, not to validate every frame.
func looksLikeAudio(head []byte, ext string) bool {
	if len(head) < 4 {
		return false
	}
	switch {
	case head[0] == 'I' && head[1] == 'D' && head[2] == '3':
		return true // ID3v2-tagged MP3
	case head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return true // raw MPEG audio frame sync
	case string(head[0:4]) == "RIFF":
		return true // WAV
	case string(head[0:4]) == "OggS":
		return true // Ogg
	case string(head[0:4]) == "fLaC":
		return true // FLAC
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		return true // MP4/M4A
	}
	// Some MP3s begin with a run of silence or a stray byte before the first
	// frame sync. Trust the extension in that case rather than refusing to ring.
	return strings.EqualFold(ext, ".mp3")
}

// displayName turns "alarm1" into "Alarm 1" for the companion app.
func displayName(id string) string {
	var b strings.Builder
	prevDigit := false
	for i, r := range id {
		switch {
		case r == '-' || r == '_':
			b.WriteRune(' ')
			prevDigit = false
			continue
		case r >= '0' && r <= '9':
			if !prevDigit && i > 0 {
				b.WriteRune(' ')
			}
			prevDigit = true
		default:
			prevDigit = false
		}
		if i == 0 {
			b.WriteString(strings.ToUpper(string(r)))
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// --- fallback tone -----------------------------------------------------------

// FallbackToneID is the id of the built-in generated tone.
const FallbackToneID = "builtin-fallback"

// WriteFallbackTone generates a short alarm tone as a 16-bit mono WAV at path.
//
// Generating audio in Go is normally the wrong call — decoding MP3 certainly is —
// but a few seconds of synthesised beeping is trivial, has no dependencies, and
// guarantees the Timeblaster makes a noise even if every sound file is missing or
// corrupt. That guarantee is worth more than the fifty lines it costs.
func WriteFallbackTone(path string) (Sound, error) {
	const (
		sampleRate = 22050
		// Two seconds: long enough to be audible, short enough to loop cleanly.
		seconds = 2
		// A pulsed two-tone beep is far more alarm-like, and much harder to sleep
		// through, than a continuous sine.
		toneA     = 880.0
		toneB     = 1174.0
		beepMS    = 250
		amplitude = 0.35
	)
	total := sampleRate * seconds
	pcm := make([]byte, total*2)

	beepSamples := sampleRate * beepMS / 1000
	for i := range total {
		slot := i / beepSamples
		var v float64
		// Alternate tone, tone, silence, silence for a classic alarm cadence.
		if slot%4 < 2 {
			freq := toneA
			if slot%4 == 1 {
				freq = toneB
			}
			v = math.Sin(2 * math.Pi * freq * float64(i) / sampleRate)
			// Short fades stop each beep clicking.
			pos := i % beepSamples
			const fade = 64
			if pos < fade {
				v *= float64(pos) / fade
			} else if rem := beepSamples - pos; rem < fade {
				v *= float64(rem) / fade
			}
		}
		s := int16(v * amplitude * math.MaxInt16)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return Sound{}, fmt.Errorf("audio: creating directory for the fallback tone: %w", err)
	}
	if err := os.WriteFile(path, buildWAV(pcm, sampleRate), 0o644); err != nil {
		return Sound{}, fmt.Errorf("audio: writing the fallback tone: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Sound{}, fmt.Errorf("audio: checking the fallback tone: %w", err)
	}
	return Sound{
		ID:        FallbackToneID,
		Name:      "Built-in tone",
		Path:      path,
		SizeBytes: info.Size(),
		Fallback:  true,
	}, nil
}

// buildWAV wraps 16-bit mono PCM in a canonical RIFF/WAVE header.
func buildWAV(pcm []byte, sampleRate int) []byte {
	const (
		numChannels   = 1
		bitsPerSample = 16
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8

	out := make([]byte, 0, 44+len(pcm))
	u32 := func(v uint32) { out = binary.LittleEndian.AppendUint32(out, v) }
	u16 := func(v uint16) { out = binary.LittleEndian.AppendUint16(out, v) }

	out = append(out, "RIFF"...)
	u32(uint32(36 + len(pcm)))
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	u32(16) // PCM chunk size
	u16(1)  // PCM format
	u16(numChannels)
	u32(uint32(sampleRate))
	u32(uint32(byteRate))
	u16(uint16(blockAlign))
	u16(bitsPerSample)
	out = append(out, "data"...)
	u32(uint32(len(pcm)))
	return append(out, pcm...)
}
