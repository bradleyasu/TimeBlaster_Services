package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ALSACard describes one sound card as reported by /proc/asound/cards.
//
// Card *numbers* are not stable across reboots or USB re-enumeration, so
// Timeblaster addresses the alarm speaker by card *id* (the short name in square
// brackets) and only uses the index for commands like amixer that require one.
type ALSACard struct {
	// Index is the kernel's current card number, e.g. 1 in "1 [Device ...".
	Index int
	// ID is the stable short identifier, e.g. "Device" or "UACDemoV10".
	ID string
	// Description is the longer human-readable name from the second line.
	Description string
}

// DeviceString returns the ALSA device string to hand to a player, for example
// "hw:CARD=Device,DEV=0". Using CARD= rather than a bare index is what keeps the
// alarm speaker working when card numbering changes.
func (c ALSACard) DeviceString() string {
	return fmt.Sprintf("hw:CARD=%s,DEV=0", c.ID)
}

// MPVAudioDevice returns the identifier mpv expects for --audio-device.
func (c ALSACard) MPVAudioDevice() string {
	return "alsa/" + c.DeviceString()
}

// ErrNoALSACard is returned when no card matches the requested selector.
var ErrNoALSACard = errors.New("system: no matching ALSA card")

// DefaultALSACardsPath is where the kernel exposes the card list.
const DefaultALSACardsPath = "/proc/asound/cards"

// cardHeaderRe matches lines such as:
//
//	1 [Device         ]: USB-Audio - USB Audio Device
var cardHeaderRe = regexp.MustCompile(`^\s*(\d+)\s*\[([^\]]+)\]\s*:\s*(.*)$`)

// ParseALSACards parses the contents of /proc/asound/cards.
//
// The format is two lines per card: a header line with index, id and driver, then
// an indented continuation line with the long name. Both are captured because the
// long name ("USB Audio Device") is usually what a user will recognise, while the
// id is what we address the card by.
func ParseALSACards(contents string) []ALSACard {
	var (
		cards []ALSACard
		sc    = bufio.NewScanner(strings.NewReader(contents))
	)
	for sc.Scan() {
		line := sc.Text()
		m := cardHeaderRe.FindStringSubmatch(line)
		if m == nil {
			// Continuation line: attach it to the card we just parsed.
			if n := len(cards); n > 0 && strings.TrimSpace(line) != "" {
				cards[n-1].Description = strings.TrimSpace(cards[n-1].Description + " / " + strings.TrimSpace(line))
			}
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		cards = append(cards, ALSACard{
			Index:       idx,
			ID:          strings.TrimSpace(m[2]),
			Description: strings.TrimSpace(m[3]),
		})
	}
	return cards
}

// SelectALSACard picks the card matching selector from cards.
//
// Selector forms, in priority order:
//
//	""                 -> the first card that looks like USB audio, else the first card
//	"hw:CARD=Device"   -> exact card id after CARD=
//	"1"                -> card index
//	"Device"           -> exact card id, else case-insensitive substring of id or description
//
// Returning an explicit error rather than silently falling back to card 0 is
// deliberate: an alarm playing out of the television instead of the bedside
// speaker is a worse failure than a loud log line.
func SelectALSACard(cards []ALSACard, selector string) (ALSACard, error) {
	if len(cards) == 0 {
		return ALSACard{}, fmt.Errorf("%w: no sound cards present", ErrNoALSACard)
	}

	sel := strings.TrimSpace(selector)
	if sel == "" {
		for _, c := range cards {
			if isUSBAudio(c) {
				return c, nil
			}
		}
		return cards[0], nil
	}

	// Accept a full ALSA device string and use the CARD= component.
	if i := strings.Index(strings.ToUpper(sel), "CARD="); i >= 0 {
		sel = sel[i+len("CARD="):]
		if j := strings.IndexAny(sel, ","); j >= 0 {
			sel = sel[:j]
		}
		sel = strings.TrimSpace(sel)
	}

	for _, c := range cards {
		if c.ID == sel {
			return c, nil
		}
	}
	if idx, err := strconv.Atoi(sel); err == nil {
		for _, c := range cards {
			if c.Index == idx {
				return c, nil
			}
		}
	}
	needle := strings.ToLower(sel)
	for _, c := range cards {
		if strings.Contains(strings.ToLower(c.ID), needle) ||
			strings.Contains(strings.ToLower(c.Description), needle) {
			return c, nil
		}
	}
	return ALSACard{}, fmt.Errorf("%w: %q (available: %s)", ErrNoALSACard, selector, describeCards(cards))
}

func isUSBAudio(c ALSACard) bool {
	return strings.Contains(strings.ToLower(c.Description), "usb")
}

func describeCards(cards []ALSACard) string {
	parts := make([]string, 0, len(cards))
	for _, c := range cards {
		parts = append(parts, fmt.Sprintf("%d:%s", c.Index, c.ID))
	}
	return strings.Join(parts, ", ")
}

// ALSAProbe discovers sound cards and mixer controls on the running system.
type ALSAProbe struct {
	// CardsPath overrides /proc/asound/cards; used by tests.
	CardsPath string
	Runner    CommandRunner
}

// Cards returns the currently present sound cards.
func (p ALSAProbe) Cards() ([]ALSACard, error) {
	path := p.CardsPath
	if path == "" {
		path = DefaultALSACardsPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("system: reading %s: %w", path, err)
	}
	return ParseALSACards(string(b)), nil
}

// Find resolves a selector against the currently present cards.
func (p ALSAProbe) Find(selector string) (ALSACard, error) {
	cards, err := p.Cards()
	if err != nil {
		return ALSACard{}, err
	}
	return SelectALSACard(cards, selector)
}

// preferredMixerControls are the simple-mixer control names we try, in order.
// USB audio class devices almost always expose one of these.
var preferredMixerControls = []string{"PCM", "Speaker", "Master", "Headphone", "Digital"}

// MixerControl finds a usable playback volume control on the card, returning "" and
// no error when the card has no hardware volume control at all — a common case for
// cheap USB speakers, where the caller should fall back to software volume.
func (p ALSAProbe) MixerControl(ctx context.Context, card ALSACard) (string, error) {
	if p.Runner == nil {
		return "", errors.New("system: ALSAProbe has no CommandRunner")
	}
	out, err := p.Runner.Run(ctx, "amixer", "-c", strconv.Itoa(card.Index), "scontrols")
	if err != nil {
		return "", fmt.Errorf("system: listing mixer controls for card %d: %w", card.Index, err)
	}
	return PickMixerControl(string(out)), nil
}

// scontrolRe matches: Simple mixer control 'PCM',0
var scontrolRe = regexp.MustCompile(`Simple mixer control '([^']+)'`)

// PickMixerControl chooses the best playback control from `amixer scontrols` output.
// Split out from MixerControl so the selection rules are unit-testable.
func PickMixerControl(scontrols string) string {
	var found []string
	for _, m := range scontrolRe.FindAllStringSubmatch(scontrols, -1) {
		found = append(found, m[1])
	}
	for _, want := range preferredMixerControls {
		for _, have := range found {
			if strings.EqualFold(have, want) {
				return have
			}
		}
	}
	// Fall back to the first control that is not obviously a capture control.
	for _, have := range found {
		if !strings.Contains(strings.ToLower(have), "mic") &&
			!strings.Contains(strings.ToLower(have), "capture") {
			return have
		}
	}
	return ""
}

// AmixerSetArgs builds the argument list that sets a control to pct percent.
// Exported so command generation can be asserted without amixer being installed.
func AmixerSetArgs(cardIndex int, control string, pct int) []string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return []string{"-c", strconv.Itoa(cardIndex), "--", "sset", control, strconv.Itoa(pct) + "%"}
}
