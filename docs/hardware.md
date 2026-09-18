# Hardware

## Overview

```text
┌─────────────────── Timeblaster enclosure ───────────────────┐
│                                                             │
│  Raspberry Pi 5 ──HDMI──────────────────────────► Television │
│       │                                                     │
│       ├──USB─────────► USB speaker (alarm audio)            │
│       │                                                     │
│       └──USB─────────► Arduino Nano ESP32                   │
│                              │                              │
│                   ┌──────────┼──────────┐                   │
│                   │          │          │                   │
│            4 potentiometers  │      2 buttons               │
│                        7-segment display                    │
│                             LEDs                            │
└─────────────────────────────────────────────────────────────┘
```

The Pi is the application brain and the source of truth. The Nano is the hardware
I/O controller and owns no policy. See
[serial-protocol.md](serial-protocol.md) for where the line is drawn and why.

## Bill of materials

| Part | Notes |
| --- | --- |
| Raspberry Pi 5 (4 GB or 8 GB) | 8 GB is comfortable once ErsatzTV is transcoding. |
| Official 27 W USB-C supply | Under-powering a Pi 5 with USB peripherals causes maddening, intermittent faults. |
| microSD (32 GB+, A2) or an NVMe HAT | Media should live on external storage, not the boot card. |
| Arduino Nano ESP32 | ESP32-S3, 3.3 V logic, native USB CDC. |
| USB-C to USB-A cable | Pi → Nano. Data, not charge-only. |
| Powered USB speaker with a USB audio interface | Anything enumerating as USB Audio Class works. |
| 4 × 10 kΩ linear potentiometers | Linear (B taper), not logarithmic. |
| 1 × momentary push button | Wi-Fi setup. |
| 1 × large momentary button, red | Alarm Off. The big one on top. |
| 4-digit 7-segment display, common cathode | Or a TM1637/MAX7219 module; see [Display](#display). |
| 3 × LEDs plus 220 Ω resistors | Alarm, Wi-Fi, Power. |
| HDMI cable | Pi → television. |

## Arduino Nano ESP32 pinout

The pin assignments live at the top of
[`timeblaster-nano.ino`](../firmware/timeblaster-nano/timeblaster-nano.ino) as
named constants. Change them there; nothing on the Pi refers to a pin number.

### Potentiometers

All four are wired identically:

```text
        3V3 ──────┬──────────────┐
                  │              │
                 ┌┴┐            ┌┴┐
                 │ │ 10kΩ       │ │
        A0 ──────┤ │◄── wiper   │ │◄── wiper ──── A1
                 │ │            │ │
                 └┬┘            └┬┘
                  │              │
        GND ──────┴──────────────┘
```

| Pot | Pin | Function |
| --- | --- | --- |
| 0 | `A0` | **TV channel.** Absolute position: the travel is divided into as many bands as there are channels. |
| 1 | `A1` | **Alarm speaker volume.** The knob is authoritative; software never overrides it for long. |
| 2 | `A2` | Reserved. **TODO:** unassigned. |
| 3 | `A3` | Reserved. **TODO:** unassigned. |

Pots 2 and 3 are already filtered, calibrated and delivered to the application as
typed events; assigning one is a matter of adding a case in
[`internal/app/router.go`](../internal/app/router.go).

**Wiring notes**

* Use **3V3, never 5 V.** The ESP32-S3's ADC inputs are not 5 V tolerant.
* Linear (B) taper. A logarithmic pot makes the channel bands wildly uneven.
* If a knob reads backwards, set `input.invert_channel_pot` or
  `input.invert_volume_pot` in the config rather than resoldering.
* Keep the wiper leads short. Long unshielded runs pick up mains hum, which shows
  up as a jittery reading; raise `input.filter_alpha` smoothing or
  `input.channel_hysteresis` if so.

### Buttons

Both are wired pin → GND and use the ESP32's internal pull-up, so they read LOW
when pressed. No external resistor is needed.

```text
        D2 ──────┬────── [ Wi-Fi button ] ────── GND
                 │
            (internal pull-up)
```

| Button | Pin | Function |
| --- | --- | --- |
| Wi-Fi setup | `D2` | **Hold five seconds** to enter Wi-Fi setup mode. A short press does nothing. |
| Alarm Off (big red) | `D3` | **Press** to dismiss a ringing alarm. Does nothing when no alarm is ringing. |

The firmware debounces for 25 ms and reports edges only. The five-second hold is
measured on the Pi, where its duration is configurable
(`input.wifi_hold_duration`) and its behaviour is unit-tested.

### LEDs

Each LED: pin → 220 Ω → LED anode, cathode → GND.

| LED | Pin | Meaning |
| --- | --- | --- |
| `ALARM` | `D4` | Blinks while an alarm is ringing. |
| `WIFI` | `D5` | On while in Wi-Fi setup mode. |
| `POWER` | `D6` | On whenever the firmware is running. |

The Pi addresses LEDs by **name**, never by pin, so rewiring never requires a
change on the Pi.

### Display

The reference wiring is a directly driven 4-digit common-cathode display:

| Signal | Pin |
| --- | --- |
| Segments a–g | `D7`–`D13` |
| Decimal point | `A7` |
| Digit enables 1–4 | `A4`, `A5`, `A6`, `D1` |

Segments are current-limited by 220 Ω resistors on each segment line.

**Using a driver module instead.** A TM1637 or MAX7219 board is fewer wires and
no multiplexing. To switch, replace only three functions in the firmware —
`displayInit()`, `displayRefresh()` and the brightness handling — and leave the
rest alone. Nothing above those functions, and nothing at all on the Pi, knows
how the display is driven.

## Raspberry Pi connections

| Port | Device | Notes |
| --- | --- | --- |
| HDMI 0 | Television | Use the port nearer the USB-C socket; it is the primary output. |
| USB 3 (blue) | Nano | Data-capable cable. |
| USB 2 (black) | USB speaker | USB 2 is plenty for audio and keeps a USB 3 port free. |
| USB 3 (blue) | Media storage, if external | |

### Why the Nano is USB-powered from the Pi

The Nano draws well under 500 mA and powering it from the Pi means one supply and
one power switch. The consequence is that **rebooting the Pi resets the Nano**,
which is handled: the Nano sends `HELLO` on boot and the Pi responds with a full
resync.

## Verifying the hardware

```bash
# Is the Nano enumerated?
tbctl ports
ls -l /dev/timeblaster-nano        # the udev rule's stable symlink
lsusb | grep -i arduino

# Is the USB speaker present, and what is its stable ALSA id?
cat /proc/asound/cards
aplay -l

# Test the speaker directly, bypassing Timeblaster entirely.
speaker-test -D hw:CARD=Device,DEV=0 -c 2 -t sine -l 1

# Watch input events arrive as you turn each knob and press each button.
sudo systemctl stop timeblaster.service
sudo -u timeblaster timeblasterd --config /etc/timeblaster/timeblaster.toml \
  --log-level debug --log-time
```

Turning a knob should produce a stream of `pot report` debug lines and — for pots
0 and 1 — occasional `channel knob selected band` or `alarm volume knob moved`
info lines. If you see raw reports but no info lines, the filtering or hysteresis
is swallowing the movement; see [troubleshooting.md](troubleshooting.md).

## Calibration

Most builds need no calibration. If a knob cannot reach one extreme:

1. Watch the raw values at the ends of travel:
   ```bash
   journalctl -u timeblaster.service -f | grep 'pot report'
   ```
2. If the maximum reads, say, 3980 rather than 4095, raise the end margin so the
   top of the travel still means 100 %:
   ```toml
   [input]
   end_margin_percent = 4.0
   ```
3. If the *whole* range is compressed — say 200 to 3900 — set the range directly:
   ```toml
   [input]
   adc_min = 200
   adc_max = 3900
   ```

## Flashing the firmware

```bash
arduino-cli core install arduino:esp32
arduino-cli compile --fqbn arduino:esp32:nano_nora firmware/timeblaster-nano
arduino-cli upload  --fqbn arduino:esp32:nano_nora -p /dev/ttyACM0 firmware/timeblaster-nano
```

Or open the sketch in the Arduino IDE and select **Arduino Nano ESP32**.

Stop `timeblaster.service` before flashing: the serial port takes one reader, and
the daemon holds it.

```bash
sudo systemctl stop timeblaster.service
# … flash …
sudo systemctl start timeblaster.service
```
