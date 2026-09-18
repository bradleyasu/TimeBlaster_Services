# Testing

## Running the tests

```bash
make test          # everything, with the race detector
make test-short    # without the race detector; faster
make cover         # coverage report at coverage.html
make check         # what CI would run: lint + test
```

**Every test runs on an ordinary development Mac or Linux machine.** There is no
Raspberry Pi, no Arduino, no mpv, no ErsatzTV and no sound card involved. That is
not a happy accident — it is the reason the codebase is shaped the way it is.

```bash
go test -race ./...
go test -race -run TestBigRedButton ./internal/app/
go test -v ./internal/alarm/
```

## How that is possible

Every out-of-process thing sits behind an interface, and each has a fake that
lives in the non-test file set so other packages' tests can use it.

| Real thing | Interface | Fake |
| --- | --- | --- |
| The USB serial cable | `serialport.Transport` | `serialport.Pipe` — an in-memory bidirectional pipe |
| Finding the serial port | `serialport.Opener` | `serialport.PipeOpener` — hands out pipes, simulates unplugging |
| The Nano | `hardware.Nano` | `hardware.FakeNano` — records commands |
| mpv | `mpv.Controller` | `mpv.Fake` — records commands, can pretend to be dead |
| The alarm player | `audio.Player` | `audio.FakePlayer` — records playback, can crash mid-alarm |
| ErsatzTV | `ersatztv.API` | `ersatztv.Fake` — configurable channels and failures |
| The network helper | `wifi.Manager` | `wifi.FakeManager` |
| The database | `storage.Store` | `storage.Memory` |
| The clock | `system.Clock` | `system.FakeClock` — time only moves when a test moves it |
| Any external command | `system.CommandRunner` | `system.FakeRunner` — records `amixer`/`nmcli` invocations |

`app.New` takes a `Deps` struct; production passes an empty one and gets the real
implementations, tests pass fakes and get the entire application graph.

### The fake clock

Scheduling tests never sleep and never flake. Time advances only when the test
says so:

```go
clk := system.NewFakeClock(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC))
// … the alarm is scheduled for 06:30 …
clk.SetNow(time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC))
scheduler.evaluate()
// … assert it fired …
```

It supports jumping backwards too, which is how NTP steps and DST transitions are
exercised.

## What is covered

### Alarm scheduling — `internal/alarm`

The most important package, and the most thoroughly tested.

* Next-occurrence arithmetic for one-shot, dated one-shot and recurring alarms.
* **Daylight saving.** Real `America/New_York` transitions: 07:00 stays 07:00
  across both the spring-forward and fall-back days; an alarm set for 02:30 on the
  day the clock skips 02:00→03:00 still fires once rather than never.
* Missed-alarm catch-up after a power cut, and the cut-off beyond which an
  occurrence is skipped rather than ringing at lunchtime.
* Not re-firing an occurrence that was already dismissed.
* Snooze, re-ring, snooze counting, and snooze being disabled.
* Auto-stop.
* Only one alarm ringing at a time.
* Disabling or deleting a ringing alarm silences it.
* **Ringing despite a storage failure** — the SD card going read-only must not
  stop the alarm.

### Input processing — `internal/input`

Pure arithmetic, tested exhaustively because it is where "the knob feels wrong"
comes from.

* Calibration, end margins, inversion, and degenerate ranges.
* EMA filtering, snap-on-large-moves, reset on reconnect.
* The volume deadband, including always emitting at the extremes so turning the
  knob fully down really does silence the speaker.
* Channel band mapping with hysteresis: noise at a boundary produces exactly one
  channel change, a deliberate sweep is honoured immediately.
* Button hold detection: short press, hold firing once at the threshold while
  still held, hold honoured on release if polling was starved, duplicate edges
  after a reconnect ignored.
* An end-to-end signal-chain test: twelve noisy ADC readings straddling a band
  boundary must produce one change, not twelve.

### The serial protocol — `internal/protocol`, `internal/serialport`

* Exact wire-format bytes are pinned, so a change that breaks the firmware fails
  here.
* **The CRC is cross-checked against a transcription of the firmware's own
  bitwise implementation**, across every byte value — the format is implemented
  twice and the two are held together by a test.
* Every corruption mode: bad version, flipped bit, truncation, oversized frame,
  dangling escape.
* Resynchronisation: boot banners, half-transmitted frames, a second `STX` before
  a terminator, byte-at-a-time delivery, `CRLF` line endings.

### The hardware link — `internal/hardware`

Driven through an in-memory pipe with a test double standing in for the firmware.

* Connection, full resync, input delivery, ping/pong.
* Resync on `HELLO`, which is the reset-recovery path.
* Periodic time sync on the fake clock.
* Reconnection after the cable is pulled.
* A silent Nano detected by heartbeat timeout.
* Malformed frames counted and dropped **without dropping the link**.
* The write queue dropping non-critical commands rather than blocking.

### Audio — `internal/audio`

* The sound library: extensions, hidden files, truncated files, subdirectories.
* Magic-number validation, including an HTML error page renamed to `.wav`.
* The full fallback chain down to the generated tone.
* The generated WAV's header sizes actually match the file.
* **Command generation**: the exact mpv argument list, asserting that the alarm
  targets the USB device, loops, and never touches video output.
* The volume conflict policy: the knob becomes authoritative, software volume is
  refused by default, `amixer` is invoked with the right arguments.
* A player crashing mid-alarm.

### Television — `internal/media`, `internal/mpv`

* Band-to-channel mapping, clamping, and the empty-channel-list case.
* ErsatzTV going away **without** discarding the channel list or blanking the TV.
* A deleted channel dropping the selection.
* Reselecting the same channel issuing no `loadfile`.
* Stream failure recovery, and the fallback to the standby image.
* mpv IPC against a fake server: round trips, events, error responses, garbage
  lines not killing the connection, in-flight requests unblocking when the socket
  dies.
* Overlay ASS generation, including **colour channel order** (ASS is BGR, which is
  how you end up with a blue banner) and escaping a channel name containing
  braces.

### Networking — `internal/wifi`

* `nmcli` terse-format parsing, including an SSID containing an escaped colon and
  duplicate SSIDs from a mesh network.
* Command generation for scan, connect and access-point setup.
* **Credential validation as a security boundary**: newlines, NULs and
  out-of-range lengths rejected before anything reaches a command line.
* Rollback on a wrong password, asserting the known-good profile is *not* deleted.
* A connection that associates but never gets an IP address failing validation.
* The captive portal: page content, probe redirects, input rejection, no external
  resources.
* Client and server over a real unix socket.

### The web API — `internal/web`

* Full alarm CRUD, including partial updates not clobbering untouched fields.
* Every validation failure mapped to the right status code.
* Conflict handling for dismiss and snooze.
* Static assets, client-route fallback, genuine 404s for missing files.
* A panic in a handler contained rather than taking the daemon down.
* The health endpoint asserted not to leak paths or credentials.

### End to end — `internal/app`

The whole graph, no hardware:

* The channel knob selecting channels through the real input, media and mpv path.
* The channel count re-dividing when ErsatzTV gains channels.
* The volume knob reaching `amixer`.
* The big red button dismissing an alarm — and doing nothing when none is ringing.
* The Wi-Fi button requiring a genuine five-second hold.
* **An alarm ringing and being dismissed with ErsatzTV down, mpv dead, the Nano
  unplugged and the network gone** — the central promise of the design.
* An alarm ringing with every sound file deleted.
* Settings persisting and applying.
* Volume *not* being restored from storage at startup.
* The supervisor restarting a panicking subsystem.
* A restart with existing alarms — a regression test for a real ordering bug.

## Writing a test

Use the fixture helpers rather than building the graph by hand:

```go
func TestSomething(t *testing.T) {
    h := newHarness(t, nil)      // internal/app: the whole application
    h.syncChannels(t)

    h.app.router.PotReport(input.PotChannel, 3800)

    waitFor(t, "channel selection", func() bool {
        cur := h.app.media.Current()
        return cur != nil && cur.Number == "4"
    })
}
```

Three conventions worth keeping:

1. **Use the fake clock, never `time.Sleep`,** for anything time-dependent.
   `waitFor` exists only for waiting on another goroutine to observe a change, not
   for waiting out a duration.
2. **Name what the test protects,** not what it calls. `TestAlarmRingsWithEverythingElseBroken`
   says why it exists; `TestTrigger2` does not.
3. **Comment the non-obvious assertion.** A future reader should not have to
   reconstruct why a particular number matters.

## The race detector

`make test` always enables it. The concurrency here is real — a serial read loop,
a write queue, a scheduler, a WebSocket hub, several supervised goroutines — and
races in it would show up as an alarm that occasionally does not ring, which is
the worst possible way to find out.

## What is not covered by tests

Honestly stated, because these are the parts to check by hand on real hardware:

* **mpv's actual DRM/KMS output.** Command generation is tested; whether the
  picture appears on a particular television is not something a unit test can
  know. See [troubleshooting.md](troubleshooting.md#the-television-is-black).
* **Real ALSA behaviour.** The device selection logic and the `amixer` command are
  tested; whether a specific USB speaker honours them is not.
* **NetworkManager's real behaviour.** Parsing and command generation are tested
  against captured output; the actual radio is not.
* **The Arduino firmware.** The protocol is cross-checked against the Go
  implementation, but the firmware has no test harness of its own. Verify it on
  the bench with `picocom`, as described in
  [serial-protocol.md](serial-protocol.md#debugging-by-hand).
* **Long-running behaviour.** Multi-day DST transitions and week-long uptime are
  simulated with the fake clock, not observed.
