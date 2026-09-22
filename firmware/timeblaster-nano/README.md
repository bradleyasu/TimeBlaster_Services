# Timeblaster — Arduino Nano ESP32 firmware

The Nano is the hardware I/O controller. It reads the potentiometers and
buttons, drives the 7-segment display and the LEDs, and reports what it sees to
the Raspberry Pi over USB serial. **The Pi decides what any of it means.**

## Layout

```text
platformio.ini        board and toolchain
src/
  main.cpp            setup/loop, the local clock, message dispatch
  Pins.h              every pin assignment, in one place
  Display.h/.cpp      what to show on the display
  Inputs.h/.cpp       potentiometers, buttons, LEDs
  Protocol.h/.cpp     the TB1 wire format
  SevenSegment.h/.cpp the display driver — carried over unmodified, see below
test/host/            host-side tests; no board required
```

## The display driver is not ours to change

`src/SevenSegment.h` and `src/SevenSegment.cpp` are copied **byte for byte**
from the working TimeblasterClock project:

```text
~/Projects/TimeblasterClock/Timeblaster/src/SevenSegment.{h,cpp}
```

That code is proven on the real hardware. It is vendored verbatim so it stays
diffable against the original, and so a bug here can never be a bug we
introduced into it. Everything Timeblaster-specific sits on top in
`Display.cpp`.

If the driver is improved upstream, re-copy both files and rerun `make test`:

```bash
cp ~/Projects/TimeblasterClock/Timeblaster/src/SevenSegment.{h,cpp} src/
make test
```

### What the hardware actually is

This matters because it is nothing like a directly driven display, and the
assumptions differ at every level:

| | |
| --- | --- |
| Drive | Four 74HC595 shift registers in a daisy chain, **one per digit** |
| Pins | Three: `D5` DATA, `D6` CLOCK, `D7` LATCH |
| Multiplexing | **None.** The registers latch and hold; there is no refresh loop |
| Display | 2 × HDSP-K511 dual-digit, **common anode** |
| Polarity | **Active low** — the driver inverts before shifting; a 0 bit lights a segment |
| Current limiting | 32 × 360 Ω, one per segment including the decimal points |
| Digit order | The first byte out travels furthest and lands on the **rightmost** digit |
| Brightness | **Not controllable** — `~OE` is tied low and is not on the 5-pin header |

The segment bit order is the driver's own and is **not** the conventional
`a`=bit0 layout:

```text
bit  7    6    5    4    3    2    1    0
     A    B    F    G    C    D    E    DP
```

### Brightness

There is no hardware dimming, so the protocol's `DISPLAY|BRIGHTNESS` message is
honoured as much as this board allows: **0 blanks the display, anything else
turns it on.** That is stated plainly rather than silently ignored.

To get real dimming, wire the 595s' `~OE` to a PWM-capable pin and set
`DISPLAY_OE_PIN` in `Pins.h`; `Display.cpp` already drives it if it is set.

## Building and flashing

The toolchain matches the one the display driver was developed with, so the
driver compiles unchanged.

```bash
pio run                 # build
pio run -t upload       # flash
pio device monitor      # watch the link
```

Stop the daemon first — the serial port takes one reader:

```bash
sudo systemctl stop timeblaster.service
pio run -t upload
sudo systemctl start timeblaster.service
```

## Tests

The firmware has host-side tests that need no board:

```bash
make test
```

The Arduino API is stubbed, and the stub **captures every bit clocked into the
shift-register chain**. That is what lets the segment patterns, the active-low
polarity and the right-to-left digit order be asserted as concrete bytes:

```text
"12.30"  ->  0x11 0x23 0x28 0xB7
```

It also checks that nothing claims `D5`, `D6` or `D7` — an LED on DATA or CLOCK
would corrupt every frame, and that mistake is invisible until the hardware is
assembled.

### Interoperability with the Pi

The TB1 wire format is implemented twice, here and in Go. They are held together
by tests on both sides:

* `make test` here checks the CRC against its known check value.
* `internal/protocol/interop_test.go` decodes **real output from this encoder**
  (`internal/protocol/testdata/firmware-frames.bin`) and re-encodes it, asserting
  the two agree byte for byte.

After a deliberate protocol change, regenerate the fixture and update the
expectations in `interop_test.go`:

```bash
make fixture
go test ./internal/protocol/
```

## What this firmware deliberately does not do

* Decide that a given ADC value means "channel 4".
* Decide that a button held for five seconds means "Wi-Fi setup".
* Store an alarm schedule, or decide when to ring.

All of that is application policy and lives in `timeblasterd`, where it is
configurable and unit-tested. See
[docs/serial-protocol.md](../../docs/serial-protocol.md).

What the firmware *does* own is the work that benefits from being close to the
hardware: oversampling and smoothing the noisy ESP32 ADC, debouncing the
buttons, driving the display, and free-running the clock between time syncs so
the display never visibly freezes because Linux was briefly busy.
