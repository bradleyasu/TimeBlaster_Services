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
| 2 × HDSP-K511 dual-digit displays | Common anode, 4 digits total. |
| 4 × 74HC595 shift registers | One per digit; see [Display](#display). |
| 32 × 360 Ω resistors | One per segment, decimal points included. |
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
| 0 | `A0` | **Channel.** Absolute position: the travel is divided into as many bands as the current mode has channels -- ErsatzTV channels in TV mode, ambience folders in ambience mode. |
| 1 | `A1` | **Alarm speaker volume.** The knob is authoritative; software never overrides it for long. |
| 2 | `A2` | **Mode.** Divided between the modes in `input.mode_knob`, by default TV, guide and ambience, in thirds from fully anticlockwise. |
| 3 | `A3` | Reserved. **TODO:** unassigned. |

Pot 3 is already filtered, calibrated and delivered to the application as a
typed event; assigning it is a matter of adding a case in
[`internal/app/router.go`](../internal/app/router.go).

### Television modes

The mode knob chooses what the television is doing; the channel knob chooses
within it. Turning the mode knob re-divides the channel knob for the new mode and
shows whichever channel it now points at.

* **TV** -- ErsatzTV channels, as before. The display shows `Ch.02`.
* **Guide** -- the listings, like the old cable guide channel. The channel the
  channel knob points at plays in the top-left quarter; the rest of the screen
  shows the time, what is on that channel now, and a 90-minute grid of every
  channel, five at a time, turning the page every 8 seconds
  (`guide.page_interval`). Changing channel jumps to its page. The display
  shows `Ch.02`, as in TV mode.
* **Ambience** -- looping local videos. Each channel is a numbered folder under
  `/srv/timeblaster/ambient` (`ambience.dir`):

  ```text
  /srv/timeblaster/ambient/ch1/fireplace.mp4         one video: loops seamlessly
  /srv/timeblaster/ambient/ch2-rain/1.mp4, 2.mp4 ...  several: in order, then repeat
  ```

  Videos play in natural order (`2.mp4` before `10.mp4`). The optional name
  after the number (`ch2-rain`) appears in the on-screen banner. The folder is
  owned by the account that ran `setup.sh`, so `scp` straight into it; new
  folders and videos are picked up within 30 seconds. The display shows `Am.02`.

Adding a mode means implementing `screen.Mode`
([`internal/screen`](../internal/screen/mode.go)), building it in
[`internal/app/screen.go`](../internal/app/screen.go), and listing its ID in
`config.ScreenModes` and `input.mode_knob`.

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
| Alarm Off (big red) | `D3` | **Press** to dismiss a ringing alarm. **Hold ten seconds** to shut the Timeblaster down. |

The firmware debounces for 25 ms and reports edges. The Wi-Fi button's
five-second hold is measured on the Pi, where its duration is configurable
(`input.wifi_hold_duration`) and its behaviour is unit-tested.

### Shutting down with the big red button

Hold the red button for ten seconds. Nothing shows for the first two, so an
ordinary press never puts a digit on the display; then it counts `8`, `7`, … `0`
and switches to the loading animation while the Pi shuts down. Letting go at any
point before `0` cancels and the display carries on as normal.

Once shutdown is complete the display goes dark, which is the signal that it is
safe to unplug. That depends on the bootloader setting `POWER_OFF_ON_HALT=1`,
which `setup.sh` applies; without it the board keeps USB powered after halting
and the display shows `OFF` instead. To start it again, unplug and replug it.

Unlike the Wi-Fi hold, this one is timed **on the Nano**, deliberately. The Pi
only sees press and release messages, and if a release were lost it would
believe the button was still down -- an ordinary press would then power the
device off ten seconds later. On the Nano the button, the timer and the display
are all local: letting go cancels instantly, and nothing is sent until the
countdown has run out. The single `SHUTDOWN` request is all the Pi acts on, so a
lost frame can fail to shut down but can never cause one.

It will not shut down:

* **from a press made while an alarm is ringing**, however long it is held --
  someone leaning on the button to silence an alarm must not turn the alarm
  clock off. An alarm starting during a countdown cancels it.
* **from a button held since power-on.** It must be seen released once first, or
  a stuck button would shut the Timeblaster down ten seconds after every boot.
* **if the Pi says no.** The Pi re-checks for a ringing alarm and can refuse;
  the display then returns to normal. If no answer arrives within five seconds
  the display returns to normal too, rather than showing a shutdown that is not
  happening.

### LEDs

Each LED: pin → 220 Ω → LED anode, cathode → GND.

| LED | Pin | Meaning |
| --- | --- | --- |
| `ALARM` | `D4` | Blinks while an alarm is ringing. |
| `WIFI` | `D8` | On while in Wi-Fi setup mode. |
| `POWER` | `D9` | On whenever the firmware is running. |

`D5`, `D6` and `D7` are **not available**: they carry the display's DATA, CLOCK
and LATCH. An LED on DATA or CLOCK would corrupt every display frame, so the
firmware's host tests assert that nothing else claims them.

The Pi addresses LEDs by **name**, never by pin, so rewiring never requires a
change on the Pi.

### Display

The display is driven by a **74HC595 shift-register chain**, one register per
digit, on the Timeblaster PCB. Three signals, no multiplexing:

| Signal | Pin |
| --- | --- |
| DATA (`SER`) | `D5` |
| CLOCK (`SRCLK`) | `D6` |
| LATCH (`RCLK`) | `D7` |

| | |
| --- | --- |
| Registers | 4 × 74HC595, daisy-chained, one per digit |
| Display | 2 × HDSP-K511 dual-digit, **common anode** |
| Polarity | **Active low** — a 0 bit lights a segment |
| Current limiting | 32 × 360 Ω, one per segment including the decimal points |
| Digit order | The first byte clocked out travels furthest and lands on the **rightmost** digit |
| Connector | J2, 5 pins: 3V3, GND, DATA, CLOCK, LATCH |

Because the registers latch and hold, there is **no refresh loop**: a frame is
clocked out once and stays lit until the next one. Driving it costs roughly
200 µs and only happens when the content actually changes.

The segment bit order is the driver's own, and is deliberately **not** the
conventional `a`=bit0 layout:

```text
bit  7    6    5    4    3    2    1    0
     A    B    F    G    C    D    E    DP
```

**Brightness.** The registers' `~OE` is tied low on the board and is not brought
out on J2, so there is **no hardware dimming**. The display is therefore exposed
throughout as a switch: `general.display_on` in the configuration, a toggle in
the companion app, and `SetDisplayOn` on the Pi. The wire format keeps a 0–100
field so that better hardware would need no protocol change; the firmware blanks
the display at 0 and lights it otherwise.

To get real dimming, wire `~OE` to a PWM-capable pin, set `DISPLAY_OE_PIN` in
`firmware/timeblaster-nano/src/Pins.h` (`Display.cpp` already drives it), and
turn the app's toggle back into a slider.

**While the Pi is booting.** The Nano is powered from the Pi's USB, so it comes
up first. Until the Pi sends its first `TIME` message the display runs the
driver's loading animation — a comet lapping the outside edge of all four digits
— rather than sitting blank or frozen. It also resumes if the link is lost long
enough for the Pi to re-announce itself.

**The driver is vendored, not written here.** `src/SevenSegment.{h,cpp}` is
copied byte for byte from the working TimeblasterClock project and must not be
edited in place — see
[firmware/timeblaster-nano/README.md](../firmware/timeblaster-nano/README.md).

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

### From the companion app

The Timeblaster ships with its own firmware image and can flash the Nano itself
over USB. When that image is newer than what the Nano reports, **System →
Controller firmware → UPDATE FIRMWARE** is enabled; otherwise it is greyed out.
It never updates on its own -- only the button starts one. The update is
refused while an alarm is ringing.

An update takes about fifteen seconds. The daemon releases the serial port, the
root helper runs `dfu-util -d 2341:0070 -R -D <image>`, the Nano restarts into
the new firmware, and the update only counts as done once the Nano reports the
new version over the link.

To release new firmware: commit the firmware change, then

```bash
make firmware-release   # builds the image, stamped with that commit
git add deploy/firmware && git commit
make deploy             # the button lights up once it is on the Pi
```

The version is the last commit to touch `src/`, `platformio.ini` or
`version.py`, so changes elsewhere in the repository never look like a firmware
update. A test fails if firmware source is committed without a matching
release.

### By hand

The firmware is a PlatformIO project, matching the toolchain the display driver
was developed and proven with:

```bash
cd firmware/timeblaster-nano
pio run                 # build
pio run -t upload       # flash
pio device monitor      # watch the link
```

It also has host-side tests that need no board at all:

```bash
make -C firmware/timeblaster-nano test
```

Stop `timeblaster.service` before flashing: the serial port takes one reader, and
the daemon holds it.

```bash
sudo systemctl stop timeblaster.service
# … flash …
sudo systemctl start timeblaster.service
```
