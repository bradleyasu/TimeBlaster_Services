# TB1 — the Pi ⇄ Nano serial protocol

One bidirectional USB CDC serial link carries everything between the Raspberry Pi
and the Arduino Nano ESP32. There are no separate GPIO lines for individual
controls: a potentiometer moving and an LED changing travel over the same cable,
framed as messages.

The format is implemented twice — [`internal/protocol`](../internal/protocol) in
Go and [the firmware](../firmware/timeblaster-nano/timeblaster-nano.ino) in C++ —
and the Go tests pin the exact bytes and cross-check the CRC against a
transcription of the firmware's implementation, so a change that breaks
compatibility fails a test rather than a bench session.

## Frame format

```text
<STX> TB1 | SEQ | TYPE | arg | arg … | CRC <LF>
 0x02  ^──────────── checksummed region ────────^  0x0A
```

| Element | Value | Why |
| --- | --- | --- |
| `STX` | `0x02` | Frame start. Everything before it is discarded, which is how a reader resynchronises after a reset or a half-transmitted frame. |
| `TB1` | literal | Protocol version. A peer seeing an unknown version logs and drops the frame rather than guessing. |
| `SEQ` | `0`–`4095` | Per-direction, wrapping. Used for `ACK`/`NAK` and for spotting dropped frames in logs. |
| `TYPE` | see below | The message type. |
| args | 0–7 fields | Type-specific, `\|`-separated. |
| `CRC` | 2 hex digits | CRC-8, polynomial `0x07`, init `0x00`, over every byte between `STX` and the `\|` that precedes the CRC. |
| `LF` | `0x0A` | Frame end. A newline keeps frames readable in `picocom`, which matters a great deal when debugging. |

### Why a checksum over USB CDC

USB already provides error detection, so the CRC is not there to catch line
noise. It catches **truncation**: a half-open connection, a Nano that resets
mid-frame, or a reader that joined the stream partway through. Those produce
structurally plausible garbage that a length check alone would not reject. It
costs eight bytes of arithmetic per frame.

### Escaping

Payload bytes that would break framing are backslash-escaped, so display text can
contain anything:

| Byte | Escape |
| --- | --- |
| `\` | `\\` |
| `\|` | `\p` |
| `LF` | `\n` |
| `CR` | `\r` |
| `STX` | `\s` |

### Limits

* Maximum frame length: 512 bytes. Longer input is discarded as noise rather than
  buffered, so a peer spraying bytes cannot grow the reader's memory.
* A trailing `CR` before the `LF` is tolerated, so a firmware using `println()`
  interoperates.

## Messages: Nano → Pi

| Type | Args | Meaning |
| --- | --- | --- |
| `HELLO` | firmware version, hardware id | Sent on boot or reset. The Pi answers with a full resync. |
| `PING` | uptime ms | Liveness beacon, every 2 s. |
| `POT` | index, raw value | A filtered potentiometer reading, `0`–`4095`. |
| `BUTTON` | name, `DOWN`\|`UP` | A debounced button edge. Names: `WIFI`, `ALARM_OFF`. |
| `LOG` | level, text | A firmware diagnostic; surfaced at debug level on the Pi. |
| `ACK` / `NAK` | seq [, reason] | Acknowledgement of a Pi command. |

```text
TB1|12|POT|0|742
TB1|13|POT|1|331
TB1|14|BUTTON|WIFI|DOWN
TB1|15|BUTTON|WIFI|UP
TB1|16|BUTTON|ALARM_OFF|DOWN
```

## Messages: Pi → Nano

| Type | Args | Meaning |
| --- | --- | --- |
| `PONG` | — | Answers `PING`. |
| `TIME` | unix seconds, UTC offset seconds, ms into the second | Clock sync. |
| `ALARM` | `ACTIVE`, `0`\|`1` | Whether an alarm is ringing. |
| `DISPLAY` | `TEXT`, text | Show literal text instead of the clock. |
| `DISPLAY` | `CLOCK` | Return to the clock. |
| `DISPLAY` | `BRIGHTNESS`, 0–100 | Display brightness. |
| `LED` | name, `0`\|`1` | Set a named LED: `ALARM`, `WIFI`, `POWER`. |
| `CONFIG` | key, value | A firmware tunable. Currently only `pot_threshold`. |

```text
TB1|1|TIME|1758220642|-14400|250
TB1|2|ALARM|ACTIVE|1
TB1|3|DISPLAY|TEXT|SETUP
TB1|4|DISPLAY|BRIGHTNESS|75
TB1|5|LED|ALARM|1
```

## Responsibility split

The Nano reports what it *observes*. The Pi decides what it *means*.

The Nano does **not** know that pot 0 selects channels, that ADC 500 is channel 4,
that a five-second hold enters Wi-Fi setup, or when an alarm should ring. All of
that is application policy and lives in `timeblasterd`, where it is configurable
and unit-tested.

What the Nano does own is everything that benefits from being close to the metal:

* 16× oversampling and an EMA over the noisy ESP32 ADC;
* 25 ms button debouncing;
* display multiplexing and brightness PWM;
* free-running the clock between time syncs.

## Time synchronisation

```text
Pi ──TIME──► Nano ──► free-runs the display from millis() ──► Pi ──TIME──► …
     every 60 s                                                   (and on change)
```

The Pi is the authoritative clock, but sends `TIME` only once a minute. Between
syncs the Nano advances its own time from `millis()`, so **the display never
visibly freezes because Linux was briefly busy** — which is exactly what happens
while ErsatzTV is transcoding.

`TIME` carries milliseconds into the current second so the Nano can align its
second boundary rather than jumping by up to a second on every resync.

The Pi sends `TIME`:

* immediately on connection and on every `HELLO`;
* every `serial.time_sync_interval` (default 60 s);
* immediately on a timezone change.

## Liveness and recovery

```text
Nano ──PING every 2 s──► Pi ──PONG──► Nano
```

* The Pi treats **no message for `serial.heartbeat_timeout`** (default 8 s) as a
  dead link, closes the port and reopens it. This catches a half-open USB
  connection, which a plain read would block on forever.
* The Nano re-sends `HELLO` if the Pi goes quiet for 10 s, so a restarted daemon
  picks it up promptly instead of waiting for the next reconnect cycle.
* On every connection and every `HELLO`, the Pi performs a **full resync**: time,
  brightness, alarm state, LEDs and display contents. The Nano therefore never
  needs to persist anything.
* On disconnect the Pi **resets all derived input state** — filters unprimed,
  hysteresis cleared, button holds forgotten — because knobs may have been turned
  and buttons pressed while the cable was out. The Nano's first reports
  re-establish the truth.

## Rate limiting

The Nano sends a `POT` message only when the filtered value moves by at least
`pot_report_threshold` counts (default 12 of 4095, well below anything a human
hand produces) or every 5 s as a heartbeat. Without this the link would carry a
continuous stream of ADC noise and the journal would be unreadable.

The Pi does the higher-level work — calibration, dead zones, hysteresis, range
mapping — in [`internal/input`](../internal/input), where it is pure arithmetic
and thoroughly tested.

## Malformed input

Every failure mode is counted and dropped, never fatal:

| Failure | Handling |
| --- | --- |
| Unknown protocol version | Frame dropped, logged at warn. |
| Bad checksum | Frame dropped, counted in `MalformedFrames`, logged at most once every 5 s. |
| Unparsable arguments | Message dropped, logged at debug; the link stays up. |
| Unknown message type | Ignored, so a newer peer can send messages the other predates. |
| Frame over 512 bytes | Discarded as noise; `Oversized` incremented. |
| Garbage between frames | Discarded; `Discarded` byte count incremented, which is a useful health signal. |

## Debugging by hand

```bash
# Watch the raw link (stop timeblasterd first — the port takes one reader).
sudo systemctl stop timeblaster.service
picocom -b 115200 /dev/timeblaster-nano
# STX renders as ^B; frames look like:  ^BTB1|12|POT|0|742|3A

# Or with the daemon's own debug logging, which decodes them for you:
sudo -u timeblaster timeblasterd --config /etc/timeblaster/timeblaster.toml \
  --log-level debug --log-time
```
