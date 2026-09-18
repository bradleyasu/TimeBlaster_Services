# Audio

Timeblaster has two completely independent audio paths.

```text
        Raspberry Pi 5
        /            \
       /              \
  TV / media        Alarm
   audio            audio
      │               │
   mpv (TV)      mpv (alarm, --no-video)
      │               │
    HDMI          USB audio
      │               │
      ▼               ▼
  Television      Bedside speaker
```

They share no process, no device and no mixer. Starting an alarm issues **no
commands at all** to the television instance, so video keeps playing while the
alarm rings.

## Why the alarm device is always explicit

The alarm device is never left to "whatever Linux considers the default". On a Pi
with HDMI attached, the default is frequently HDMI — and an alarm that goes off
through a switched-off television is a silent alarm.

Configure it in `/etc/timeblaster/timeblaster.toml`:

```toml
[audio]
alarm_device = "Device"
```

### Choosing a stable identifier

ALSA card *numbers* are assigned in probe order and move between reboots and
replugs. The card **id** does not. List them:

```bash
cat /proc/asound/cards
```

```text
 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0
                      vc4-hdmi-0
 1 [vc4hdmi1       ]: vc4-hdmi - vc4-hdmi-1
                      vc4-hdmi-1
 2 [Device         ]: USB-Audio - USB Audio Device
                      Generic USB Audio Device at usb-xhci-hcd.1-1, full speed
```

`alarm_device` accepts, in order of preference:

| Form | Example | Stable? |
| --- | --- | --- |
| Substring of the description | `"USB"` | Yes |
| Card id | `"Device"` | Yes |
| Full ALSA device string | `"hw:CARD=Device,DEV=0"` | Yes |
| Card index | `"2"` | **No** — avoid |
| Empty | `""` | Yes — picks the first USB audio card |

The default of `""` picks the first card whose description mentions USB, falling
back to the first card present. On a typical build that is exactly right and
needs no configuration at all.

If the selector matches nothing, Timeblaster logs an error and marks
`alarm_audio` down in `/api/health` — it does **not** silently fall back to HDMI,
because a loud log line is far better than an alarm you never hear.

## Volume

The physical potentiometer on pot 1 is authoritative.

```text
Volume knob ──► Nano ADC ──► USB serial ──► timeblasterd
                                               │
                              ┌────────────────┴────────────────┐
                              │                                 │
                    hardware mixer available?            no mixer control
                              │                                 │
                    amixer -c N sset PCM 65%          mpv software volume
                              │                                 │
                              ▼                                 ▼
                         USB speaker                      USB speaker
```

Timeblaster prefers the card's own mixer control, discovered once at startup:

```bash
amixer -c 2 scontrols
```

```text
Simple mixer control 'PCM',0
```

It tries `PCM`, `Speaker`, `Master`, `Headphone`, `Digital` in that order, then
any control that is not obviously a capture control. Cheap USB speakers often
expose none at all, in which case it falls back to mpv's software volume — which
works, but gives coarser control at low levels.

To override:

```toml
[audio]
mixer_control = "Speaker"    # a specific control
# mixer_control = "none"     # force software volume
```

### The knob-versus-app conflict policy

This is stated explicitly because it is the sort of thing that otherwise produces
a confusing fight between two sources of truth:

* At boot, volume starts at `audio.startup_volume_percent` (default 40) and is
  marked **provisional**. The persisted value is deliberately *not* replayed.
* The first report from the knob promotes volume to **authoritative** and applies
  the knob's real position.
* The companion app's volume endpoint is **rejected** unless
  `audio.allow_software_volume = true`. The app displays the volume read-only and
  says where it comes from.
* When software volume is enabled, a value set from the app holds until the next
  movement of the knob, which always wins.

The persisted "last known volume" is a diagnostic only. A knob's physical position
*is* the volume; restoring a stale number over it would leave the hardware and the
software disagreeing, with the user holding the hardware.

## The alarm player

Alarm audio is played by a dedicated, audio-only mpv process:

```bash
mpv --no-video --no-terminal --no-config --audio-display=no \
    --ao=alsa --audio-device=alsa/hw:CARD=Device,DEV=0 \
    --loop-file=inf --volume=65 \
    --input-ipc-server=/run/timeblaster/alarm-1758220642.sock \
    /var/lib/timeblaster/alarm-sounds/alarm1.mp3
```

mpv rather than a Go MP3 decoder because it is already installed for video,
handles every container a user might drop in, and exposes the same JSON IPC the
television instance uses. `--no-config` means a stray `~/.config/mpv/mpv.conf`
can never change alarm behaviour.

Note it is **not** started with a cancellable context: an alarm must not stop
because the HTTP request that started it was cancelled. Only an explicit stop
ends it.

## Sound files

Drop files into `/var/lib/timeblaster/alarm-sounds/`. They are picked up within
five minutes, or immediately on a service restart.

```bash
sudo cp klaxon.mp3 /var/lib/timeblaster/alarm-sounds/
sudo chown timeblaster:timeblaster /var/lib/timeblaster/alarm-sounds/klaxon.mp3
tbctl sounds
```

Accepted: `.mp3`, `.wav`, `.ogg`, `.flac`, `.m4a`, `.aac`. Files smaller than 512
bytes, dotfiles and subdirectories are ignored.

An alarm stores a **sound id** — the file name without its extension — not a path,
so moving the sounds directory does not orphan every alarm.

### What happens when a sound goes missing

The fallback chain, in order:

1. the sound the alarm names;
2. the configured default (`audio.default_sound_id`, or the app's setting);
3. any other playable file in the library;
4. the **built-in generated tone**.

Each substitution is logged at warn level with the reason, so a silently swapped
sound is still visible in the journal.

The built-in tone is synthesised at startup into `/run/timeblaster/fallback-tone.wav`
— a pulsed two-tone beep, two seconds long, designed to loop cleanly. Generating
audio in Go is normally the wrong call, but it guarantees the device makes a noise
even when every file is missing or corrupt, and that guarantee is worth the fifty
lines it costs.

Files are validated at selection time *and* again at play time, because a file can
be deleted between the two. Validation checks existence, a plausible size, and the
container's magic bytes — enough to catch a text file or a truncated download
renamed to `.mp3`, which is the realistic corruption, without pretending to be a
decoder.

## Television audio

Television audio rides with the video over HDMI, handled entirely by the
television mpv instance:

```toml
[mpv]
args = [
  # …
  "--audio-device=auto",
]
```

To pin it to a specific HDMI output — useful on a Pi 5, which has two:

```toml
args = [
  # …
  "--audio-device=alsa/hw:CARD=vc4hdmi0,DEV=0",
]
```

## Diagnosing

```bash
# What does Timeblaster think?
tbctl health
curl -s http://timeblaster.local:8080/api/state | python3 -m json.tool | grep -A10 '"audio"'

# Play a sound through the alarm path, end to end.
tbctl play alarm1
tbctl stop

# Bypass Timeblaster entirely and test the card itself.
speaker-test -D hw:CARD=Device,DEV=0 -c 2 -t sine -l 1
aplay -D hw:CARD=Device,DEV=0 /usr/share/sounds/alsa/Front_Center.wav

# Watch the volume path.
journalctl -u timeblaster.service -f | grep -E 'volume|audio'

# Check the mixer directly.
amixer -c 2 scontrols
amixer -c 2 sget PCM
```

### Common problems

| Symptom | Cause | Fix |
| --- | --- | --- |
| Alarm plays through the television | `alarm_device` matched an HDMI card, or the USB speaker was absent at startup | Set `alarm_device` explicitly; check `tbctl health` |
| `alarm_audio: down` | The selector matched nothing | `cat /proc/asound/cards`, fix the selector |
| Alarm is silent but health is ok | Card volume at zero, or the speaker is muted | `amixer -c N sset PCM 70%`, check the speaker's own knob |
| Volume knob does nothing | Nano not connected, or no mixer control and nothing playing | `tbctl health`; software volume only affects live playback |
| Volume steps coarsely | No hardware mixer; mpv's software volume is in use | `amixer -c N scontrols` to confirm; some cheap speakers have none |
| Speaker works, then stops after a replug | Card renumbered | Use a card **id**, not an index |

### Permissions

The service account must be in the `audio` group:

```bash
id timeblaster            # should list: audio
sudo usermod -aG audio timeblaster
sudo systemctl restart timeblaster.service
```

## PipeWire

Raspberry Pi OS **Lite** ships no sound server, and Timeblaster targets raw ALSA
because it is the shortest, most predictable path to a specific device — with no
daemon that can go missing between the alarm firing and the speaker.

If you have installed PipeWire yourself, ALSA access still works through its
compatibility layer. To use PipeWire's output explicitly instead:

```toml
[audio]
extra_player_args = ["--ao=pipewire"]
alarm_device = ""     # let PipeWire route it
```

Be aware that this makes the alarm depend on a user-session daemon being alive,
which is a step down in reliability for the device's most important function.
