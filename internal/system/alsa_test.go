package system

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Real /proc/asound/cards output from a Raspberry Pi 5 with HDMI audio and a USB
// speaker attached.
const sampleCards = ` 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0
                      vc4-hdmi-0
 1 [vc4hdmi1       ]: vc4-hdmi - vc4-hdmi-1
                      vc4-hdmi-1
 2 [Device         ]: USB-Audio - USB Audio Device
                      Generic USB Audio Device at usb-xhci-hcd.1-1, full speed
`

func TestParseALSACards(t *testing.T) {
	cards := ParseALSACards(sampleCards)
	if len(cards) != 3 {
		t.Fatalf("got %d cards: %+v", len(cards), cards)
	}
	if cards[2].Index != 2 || cards[2].ID != "Device" {
		t.Errorf("third card: %+v", cards[2])
	}
	if !strings.Contains(cards[2].Description, "USB Audio Device") {
		t.Errorf("description lost: %q", cards[2].Description)
	}
	if got := cards[2].DeviceString(); got != "hw:CARD=Device,DEV=0" {
		t.Errorf("DeviceString: %q", got)
	}
	if got := cards[2].MPVAudioDevice(); got != "alsa/hw:CARD=Device,DEV=0" {
		t.Errorf("MPVAudioDevice: %q", got)
	}
}

func TestParseALSACardsEmpty(t *testing.T) {
	if got := ParseALSACards(""); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	if got := ParseALSACards("--- no soundcards ---\n"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestSelectALSACard(t *testing.T) {
	cards := ParseALSACards(sampleCards)
	tests := []struct {
		name     string
		selector string
		wantID   string
		wantErr  bool
	}{
		{"empty prefers usb", "", "Device", false},
		{"exact id", "Device", "Device", false},
		{"index", "0", "vc4hdmi0", false},
		{"full alsa string", "hw:CARD=vc4hdmi1,DEV=0", "vc4hdmi1", false},
		{"mpv style", "alsa/hw:CARD=Device,DEV=0", "Device", false},
		{"substring of description", "usb audio", "Device", false},
		{"substring of id", "hdmi1", "vc4hdmi1", false},
		{"no match", "bluetooth", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectALSACard(cards, tc.selector)
			if tc.wantErr {
				if !errors.Is(err, ErrNoALSACard) {
					t.Fatalf("got %+v, %v; want ErrNoALSACard", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tc.wantID {
				t.Errorf("got %q want %q", got.ID, tc.wantID)
			}
		})
	}
}

func TestSelectALSACardNoCards(t *testing.T) {
	if _, err := SelectALSACard(nil, "Device"); !errors.Is(err, ErrNoALSACard) {
		t.Fatalf("got %v", err)
	}
}

func TestSelectALSACardRefusesToGuessWhenNoUSBIsPresent(t *testing.T) {
	// The failure this prevents: on a Pi with nothing plugged in the first card
	// is HDMI, so falling back to it would route the alarm to the television and
	// then report itself healthy. Being told to plug the speaker in is far
	// better than an alarm you cannot hear.
	cards := ParseALSACards(sampleCards)[:2] // HDMI only
	_, err := SelectALSACard(cards, "")
	if !errors.Is(err, ErrNoALSACard) {
		t.Fatalf("got %v, want ErrNoALSACard", err)
	}
	// The message has to be actionable: it should name the way out.
	if !strings.Contains(err.Error(), "audio.alarm_device") {
		t.Errorf("error should say how to fix it: %v", err)
	}
	if !strings.Contains(err.Error(), "vc4hdmi0") {
		t.Errorf("error should list what is available: %v", err)
	}
}

func TestSelectALSACardStillHonoursAnExplicitHDMIChoice(t *testing.T) {
	// Refusing to guess must not mean refusing to obey.
	cards := ParseALSACards(sampleCards)[:2]
	got, err := SelectALSACard(cards, "vc4hdmi0")
	if err != nil || got.ID != "vc4hdmi0" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestPickMixerControl(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "prefers PCM",
			in:   "Simple mixer control 'Mic',0\nSimple mixer control 'PCM',0\nSimple mixer control 'Speaker',0\n",
			want: "PCM",
		},
		{
			name: "falls back to Speaker",
			in:   "Simple mixer control 'Speaker',0\n",
			want: "Speaker",
		},
		{
			name: "skips capture-only controls",
			in:   "Simple mixer control 'Mic Capture',0\nSimple mixer control 'Weird',0\n",
			want: "Weird",
		},
		{
			name: "no controls at all",
			in:   "",
			want: "",
		},
		{
			name: "only capture controls",
			in:   "Simple mixer control 'Mic',0\n",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickMixerControl(tc.in); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestAmixerSetArgs(t *testing.T) {
	tests := []struct {
		pct  int
		want []string
	}{
		{50, []string{"-c", "2", "--", "sset", "PCM", "50%"}},
		{-5, []string{"-c", "2", "--", "sset", "PCM", "0%"}},
		{140, []string{"-c", "2", "--", "sset", "PCM", "100%"}},
	}
	for _, tc := range tests {
		if got := AmixerSetArgs(2, "PCM", tc.pct); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("AmixerSetArgs(%d) = %v want %v", tc.pct, got, tc.want)
		}
	}
}
