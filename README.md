# Timeblaster

A purpose-built alarm clock and television appliance, running on a Raspberry Pi 5
with an Arduino Nano ESP32 as its hardware I/O controller.

```text
                         iPhone / browser
                                │
                           Home Wi-Fi
                                │
                     http://timeblaster.local
                                │
                                ▼
┌───────────────────── Raspberry Pi 5 ──────────────────────┐
│                                                           │
│  timeblasterd                                             │
│   ├── alarm scheduling      ├── Nano USB serial           │
│   ├── alarm audio           ├── ErsatzTV integration      │
│   ├── configuration/state   ├── mpv TV playback control   │
│   ├── HTTP + WebSocket API  └── Wi-Fi setup coordination  │
│   └── PWA companion app                                   │
│                                                           │
│  ErsatzTV ──► mpv ──────────────────────────────── HDMI   │
│  alarm player ──────────────────────────────── USB audio  │
│  timeblaster-wifi (root) · Avahi / mDNS                   │
└───────────────────────────────────────────────────────────┘
                                │
                           USB serial
                                │
                                ▼
                  ┌──────────────────────────┐
                  │   Arduino Nano ESP32     │
                  │ 4 pots · 2 buttons       │
                  │ 7-segment display · LEDs │
                  └──────────────────────────┘
```

## What it does

* **Wakes you up.** Recurring and one-shot alarms, snooze, day-of-week repeats,
  daylight-saving-correct scheduling, and a big red button on top that stops the
  noise.
* **Plays television.** ErsatzTV turns folders of media into live channels; a
  potentiometer on the front selects between them; mpv drives HDMI directly with
  no desktop environment.
* **Is configured from your phone.** A PWA at `http://timeblaster.local` that
  installs to the Home Screen.
* **Joins Wi-Fi with one gesture.** Hold a button for five seconds. That is the
  entire networking interface.

## The rule that shapes everything

**The alarm clock is the highest-priority function, and nothing else is allowed
to compromise it.**

That single constraint drives most of the design decisions here:

* `internal/alarm` depends only on storage, a clock and an audio sink. It has no
  dependency on ErsatzTV, mpv, Wi-Fi, the Nano or the web server.
* The health endpoint reports `unhealthy` **only** when alarm scheduling itself is
  impaired. A dead television, an absent Arduino and a missing network are all
  merely `degraded`.
* An alarm rings even when every sound file has been deleted, using a tone the
  daemon synthesises at startup.
* An alarm rings even when the SD card has gone read-only.
* A panicking subsystem is logged with its stack and restarted; only a failure of
  the scheduler itself stops the daemon.

There is an end-to-end test named `TestAlarmRingsWithEverythingElseBroken` that
holds all of it in place.

## Getting started

```bash
# On a Raspberry Pi 5 with a fresh Raspberry Pi OS Lite (64-bit)
git clone <your-repo-url> timeblaster
cd timeblaster
sudo ./setup.sh
```

Then:

1. Copy alarm MP3s into `/var/lib/timeblaster/alarm-sounds/`
2. Add media and create channels at `http://timeblaster.local:8409`
3. Open `http://timeblaster.local` and create an alarm
4. `tbctl health` to confirm everything is up

Full detail in [docs/installation.md](docs/installation.md).

## Documentation

| Document | Covers |
| --- | --- |
| [architecture.md](docs/architecture.md) | The design of record: layout, components, interfaces, services, data model, protocol, hardware, audio routing, installation strategy |
| [installation.md](docs/installation.md) | Installing, upgrading, backing up, restoring, factory reset |
| [configuration.md](docs/configuration.md) | Every configuration key and when to change it |
| [hardware.md](docs/hardware.md) | Bill of materials, wiring, pinout, calibration, flashing the firmware |
| [serial-protocol.md](docs/serial-protocol.md) | The TB1 wire format and the Pi/Nano responsibility split |
| [audio.md](docs/audio.md) | The two independent audio paths, device selection, the volume policy |
| [ersatztv.md](docs/ersatztv.md) | Media format, channels, the channel knob, the standby image, the banner |
| [wifi-setup.md](docs/wifi-setup.md) | The five-second hold, the captive portal, the privilege split |
| [api.md](docs/api.md) | HTTP and WebSocket API reference |
| [testing.md](docs/testing.md) | What is tested, how, and what is not |
| [troubleshooting.md](docs/troubleshooting.md) | Symptoms, causes, fixes |

## Repository layout

```text
cmd/
  timeblasterd/        the main daemon (unprivileged)
  timeblaster-wifi/    the privileged network helper
  tbctl/               operator CLI
internal/
  alarm/       scheduling, recurrence, snooze, dismissal
  app/         composition root: wiring, event routing, supervision
  audio/       the alarm speaker: sound library, player, volume
  config/      TOML configuration, defaults, validation
  ersatztv/    ErsatzTV client
  hardware/    the Nano session: framing, reconnect, time sync
  input/       filtering, calibration, hysteresis, hold detection
  logging/     journald-friendly structured logging
  media/       channel selection and playback
  mpv/         process supervision, JSON IPC, ASS overlays
  protocol/    the TB1 wire format (pure, no I/O)
  serialport/  transports, device discovery, frame scanning
  state/       runtime state and broadcast events
  storage/     SQLite: alarms and settings
  system/      clock, command execution, ALSA discovery
  web/         HTTP API and the embedded PWA
  wifi/        helper RPC, nmcli, captive portal
  wsocket/     WebSocket fan-out
firmware/      the Arduino Nano ESP32 sketch
deploy/        systemd units, config, udev, Avahi, assets
docs/          documentation
scripts/       asset generation
```

## Development

Everything runs on an ordinary Mac or Linux machine. No Raspberry Pi, no Arduino,
no mpv, no ErsatzTV, no sound card.

```bash
make test           # full suite with the race detector
make check          # lint + test, what CI would run
make dev            # run the daemon locally against a scratch config
make build-pi       # cross-compile for linux/arm64
make deploy PI=timeblaster.local
make logs PI=timeblaster.local
make help           # everything else
```

Every out-of-process dependency sits behind an interface with a fake — the serial
cable is an in-memory pipe, the clock only advances when a test says so. See
[docs/testing.md](docs/testing.md).

## The Pi / Nano split

The Pi owns all policy. The Nano reports what it observes and renders what it is
told.

| Raspberry Pi | Arduino Nano |
| --- | --- |
| Time, timezone, alarms, scheduling, dismissal | Reading potentiometers and buttons |
| Alarm audio and volume mapping | ADC oversampling and smoothing |
| Which channel a knob position means | Button debouncing |
| The five-second hold that enters Wi-Fi setup | Driving the 7-segment display and LEDs |
| Configuration, persistence, the web app | Free-running the clock between syncs |

The Nano deliberately does **not** know that pot 0 selects channels, that a
five-second hold means anything, or when an alarm should ring. Keeping that on the
Pi means it is configurable, testable, and changeable without a soldering iron.

## Status

The foundation is complete and tested. Known gaps, all marked `TODO` in the code:

* **Pots 2 and 3 are unassigned.** They are filtered, calibrated and delivered to
  the application as typed events; assigning one is a case in
  `internal/app/router.go`.
* **The channel overlay's enabled flag applies on restart,** not live.
* **Configuration is read at startup only** — there is no live reload.
* **Factory reset** is documented as a manual procedure rather than implemented.

## Dependencies

Four, all pure Go, so the binaries cross-compile to `linux/arm64` with nothing
but the Go toolchain:

| Module | For |
| --- | --- |
| `github.com/BurntSushi/toml` | Configuration |
| `go.bug.st/serial` | The USB serial port |
| `github.com/coder/websocket` | The companion app's event stream |
| `modernc.org/sqlite` | Storage, without cgo |
