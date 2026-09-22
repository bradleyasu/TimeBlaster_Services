# Troubleshooting

## Start here

```bash
tbctl health      # component summary; exits non-zero when degraded
```

```text
status   DEGRADED
version  1.0.0
uptime   2h13m40s

  alarm          ok
  alarm_audio    ok
  alarm_sounds   ok
  ersatztv       ok
  nano           down          ← the Arduino is not connected
  network        ok
  tv_player      ok
  web            ok
  wifi_helper    ok
```

The status word is chosen deliberately:

* **ok** — everything is working.
* **degraded** — something is broken, but *the alarm clock is not*. A missing
  Nano, a dead television, an unreachable ErsatzTV all land here.
* **unhealthy** — alarm scheduling itself is impaired. This is the only state
  that means "the device has stopped doing its actual job".

## Viewing logs

```bash
# The main daemon
journalctl -u timeblaster.service -f
journalctl -u timeblaster.service -n 100 --no-pager
journalctl -u timeblaster.service --since "1 hour ago"

# Everything Timeblaster-related at once, interleaved
journalctl -u timeblaster.service -u timeblaster-wifi.service -u ersatztv.service -f

# Only warnings and errors
journalctl -u timeblaster.service -p warning

# Across a reboot
journalctl -u timeblaster.service -b -1
```

### Turning on debug logging

```bash
# Temporarily, in the foreground — the quickest way to watch input events:
sudo systemctl stop timeblaster.service
sudo -u timeblaster /usr/local/bin/timeblasterd \
  --config /etc/timeblaster/timeblaster.toml --log-level debug --log-time
```

Or permanently:

```toml
[logging]
level = "debug"
```

Debug level includes every potentiometer reading, which is a firehose. It is the
right tool for diagnosing a knob and the wrong one to leave on.

### What to look for

Timeblaster logs every state change that matters:

```text
timeblaster starting version=1.0.0 hostname=timeblaster timezone=America/New_York
Nano connected device=/dev/timeblaster-nano
alarm audio device resolved card_id=Device card_index=2 mixer_control=PCM
ErsatzTV is reachable url=http://127.0.0.1:8409 channels=4
channel change number=3 name=Sci-Fi url=…
alarm volume knob moved percent=65
button hold threshold reached button=WIFI held=5.001s
alarm triggered alarm_id=1 label="Wake up" sound=alarm1 scheduled=2026-09-18T06:30:00-04:00
big red button pressed; dismissing the active alarm
alarm stopped alarm_id=1 reason=dismissed rang_for=12s
mpv exited; restarting error=… backoff=2s
Nano disconnected device=/dev/timeblaster-nano error=… reconnect_in=1s
```

---

## Alarms

### An alarm did not ring

```bash
tbctl alarms          # is it enabled? is NEXT what you expect?
tbctl health          # alarm and alarm_audio
journalctl -u timeblaster.service --since "6 hours ago" | grep -i alarm
```

| Cause | How to tell | Fix |
| --- | --- | --- |
| Alarm disabled | `tbctl alarms` shows `ON: no` | Enable it in the app |
| Wrong timezone | `NEXT` is offset by hours | Set it in **SETTINGS**, or `general.timezone` |
| Wrong repeat days | `REPEAT` column | Edit the alarm |
| The Pi was off across the alarm time | No log entries at all | Within 5 minutes it catches up automatically; beyond that it is skipped by design |
| Audio failed but the alarm fired | `alarm triggered` present, `alarm audio failed to start` follows | See [audio.md](audio.md) |

**Missed-alarm policy.** If the daemon was down across an alarm time, it fires on
startup provided it is less than five minutes late. Beyond that the occurrence is
skipped — waking up five minutes late is far better than not waking up, but a
Timeblaster that was unplugged overnight should not start shouting at lunchtime.

### The alarm rang but there was no sound

The alarm and its audio are independent. `alarm triggered` in the log with no
sound means the audio path failed, and the alarm is still active and dismissible.

```bash
tbctl health                   # alarm_audio
tbctl play alarm1              # test the path directly
speaker-test -D hw:CARD=Device,DEV=0 -c 2 -t sine -l 1   # bypass Timeblaster
```

See [audio.md](audio.md#diagnosing).

### The alarm will not stop

```bash
tbctl dismiss                              # from a shell
curl -X POST http://timeblaster.local:8080/api/alarm/dismiss
sudo systemctl restart timeblaster.service # last resort; shutdown silences audio
```

If the big red button does nothing, the Nano is probably disconnected —
`tbctl health` → `nano`.

Every alarm also auto-stops after `auto_stop_minutes` (default 15), so it cannot
ring indefinitely.

---

## The Nano

### `nano: down`

```bash
tbctl ports                       # serial ports this machine can see
ls -l /dev/timeblaster-nano       # the udev symlink
lsusb | grep -i -E 'arduino|espressif'
journalctl -u timeblaster.service | grep -i nano
```

| Symptom | Cause | Fix |
| --- | --- | --- |
| No serial ports at all | Cable is charge-only, or the board is unpowered | Use a data cable; check the Nano's power LED |
| `/dev/ttyACM0` exists but not the symlink | udev rule not installed, or a different USB id | `sudo udevadm control --reload-rules && sudo udevadm trigger`; check ids with `udevadm info -a -n /dev/ttyACM0 \| grep idVendor` |
| Port exists, permission denied | Service account not in `dialout` | `sudo usermod -aG dialout timeblaster` then restart the service |
| Connects then drops every few seconds | Firmware not sending `PING`, or a flaky cable | Reflash; try another cable and port |

Timeblaster retries forever with backoff, so plugging the Nano in later needs no
restart.

### Knobs do nothing

```bash
sudo systemctl stop timeblaster.service
sudo -u timeblaster timeblasterd --config /etc/timeblaster/timeblaster.toml \
  --log-level debug --log-time | grep -E 'pot report|knob'
```

* **No `pot report` lines** — the Nano is not reporting. Check wiring and the
  firmware's pin assignments.
* **`pot report` lines but no `knob` lines** — the values are arriving but are not
  meaningful movement. Either the range is wrong (see
  [hardware.md](hardware.md#calibration)) or hysteresis is swallowing it.

### The channel flickers between two channels

ADC noise sitting on a band boundary. Raise the hysteresis:

```toml
[input]
channel_hysteresis = 0.35    # default 0.25; the knob will feel slightly stickier
filter_alpha = 0.25          # and smooth harder
```

### The volume jumps around

```toml
[input]
volume_deadband_percent = 4   # default 2
filter_alpha = 0.25
```

If it is severe, suspect a long unshielded wiper lead picking up mains hum.

---

## The television is black

This is usually mpv's output configuration. Work through it in order.

### 1. Is mpv even running?

```bash
tbctl health                    # tv_player
pgrep -a mpv
journalctl -u timeblaster.service | grep -i mpv
```

### 2. Can mpv open DRM at all?

```bash
sudo systemctl stop timeblaster.service
sudo -u timeblaster mpv --vo=gpu --gpu-context=drm --fullscreen \
  /usr/share/timeblaster/assets/no-channel.png
```

If that fails, the problem is permissions or output configuration, not
Timeblaster.

### 3. Permissions

The service account needs **both** `video` and `render`. Missing `render` is the
most common cause on a Pi 5, and the error message is not obvious.

```bash
id timeblaster                  # must include: video render
ls -l /dev/dri/                 # card0/card1 and renderD128
sudo usermod -aG video,render timeblaster
sudo systemctl restart timeblaster.service
```

### 4. Try a different output configuration

Edit `[mpv] args` in `/etc/timeblaster/timeblaster.toml`. The default targets
DRM/KMS with OpenGL, which suits most setups:

```toml
args = ["--vo=gpu", "--gpu-context=drm", "--gpu-api=opengl", "--hwdec=auto-safe", …]
```

Alternatives, in rough order of what to try next:

```toml
# Vulkan via gpu-next. Often lower CPU on a Pi 5; "background=none" works around
# a direct-scanout issue in the V3D Vulkan driver during fullscreen transitions.
args = ["--vo=gpu-next", "--gpu-api=vulkan", "--gpu-context=drm",
        "--background=none", "--hwdec=auto-safe", "--fullscreen", …]

# The dedicated DRM video output. Lowest overhead; no OpenGL involved at all.
args = ["--vo=drm", "--hwdec=auto-safe", "--fullscreen", …]

# Software decoding, to rule out the hardware decoder.
args = ["--vo=gpu", "--gpu-context=drm", "--hwdec=no", "--fullscreen", …]
```

Test each by hand before committing it to the config:

```bash
sudo systemctl stop timeblaster.service
sudo -u timeblaster mpv --vo=gpu-next --gpu-api=vulkan --gpu-context=drm \
  --background=none --fullscreen 'http://127.0.0.1:8409/iptv/channel/1.m3u8'
```

### 5. Pick the right connector

A Pi 5 has two HDMI ports. If the picture goes to the wrong one:

```bash
ls /sys/class/drm/            # card1-HDMI-A-1, card1-HDMI-A-2, …
```

```toml
args = […, "--drm-connector=HDMI-A-1"]
```

### The Linux console or a login prompt is visible

What should happen on a finished install: dark, then the Timeblaster boot
screen, then the standby screen. No kernel messages, no `[ OK ]` lines, no login
prompt, ever.

```bash
cat /boot/firmware/cmdline.txt
systemctl is-enabled getty@tty1.service     # should be: disabled
systemctl is-enabled getty@tty2.service     # should be: enabled (the rescue login)
systemctl status timeblaster-splash.service
```

| What you see | Cause | Fix |
| --- | --- | --- |
| A `login:` prompt | The getty on the television's VT is still enabled | `sudo systemctl disable --now getty@tty1.service` |
| Kernel messages scrolling | `quiet`/`console=tty3` missing from `cmdline.txt` | Rerun `sudo ./setup.sh`, then reboot |
| Green `[ OK ]` lines | `systemd.show_status=false` missing | As above |
| A blinking cursor | `vt.global_cursor_default=0` missing | As above |
| The Raspberry Pi logos | `logo.nologo` missing | As above |

All of the `cmdline.txt` options need a reboot. `setup.sh` never reboots by
itself; it tells you when one is needed.

**Note that the local login moves rather than disappearing.** Press
**Ctrl+Alt+F2** for a console. SSH and the serial console are untouched. To put
it back on the television's VT, rerun with `--keep-console` or:

```bash
sudo systemctl enable --now getty@tty1.service
```

### The boot screen does not appear

The screen is painted straight into the framebuffer and the process then exits,
so nothing is holding the display when mpv starts. The usual cause of a blank
boot is that there is no framebuffer to paint into.

```bash
sudo timeblaster-splash --check        # reports geometry, paints nothing
ls -l /dev/fb0
journalctl -u timeblaster-splash.service -b
```

| Symptom | Cause | Fix |
| --- | --- | --- |
| `no framebuffer at /dev/fb0` | fbdev emulation is off | Check `dtoverlay=vc4-kms-v3d` in `/boot/firmware/config.txt`; the boot stays dark but the television is otherwise fine |
| `unsupported framebuffer depth` | The screen is not 16- or 32-bit | Harmless; the boot stays dark |
| It appears, then the screen goes dark for a while | Normal — mpv is starting | If it lasts more than ~15 s, see [The television is black](#the-television-is-black) |
| It stays up for good | The daemon never got the display | `tbctl health` → `tv_player` |

Paint it by hand to check the whole path end to end:

```bash
sudo systemctl stop timeblaster.service
sudo timeblaster-splash --text "HELLO"
sudo timeblaster-splash --clear
sudo systemctl start timeblaster.service
```

### ErsatzTV warns about hardware acceleration

"The following channels use ffmpeg profiles that are not configured for
hardware acceleration (Nvenc)" is a **false positive on a Raspberry Pi** and
should be ignored — enabling NVENC would break the channel, because there is no
NVIDIA GPU, and the Pi 5 has no hardware video encoder of any kind. See
[ersatztv.md](ersatztv.md#not-configured-for-hardware-acceleration-nvenc).

### The television says "BOOTING, PLEASE STAND BY..." long after boot

That screen stays up until the channel list has been read for the first time,
because telling you to turn the channel knob before ErsatzTV has any channels
would be inviting you to do something that cannot work. After
`mpv.booting_timeout` (default 90 s) it gives way to the standby screen anyway.

So a boot screen that lingers means ErsatzTV has not come up:

```bash
tbctl health                     # ersatztv
systemctl status ersatztv.service
```

### Video stutters or drops frames

Almost always transcoding. See [ersatztv.md](ersatztv.md#media-format).

```bash
top -b -n1 | head -15           # is ffmpeg pinning the CPU?
```

---

## Network and the companion app

### `timeblaster.local` does not resolve

```bash
# From the Pi
hostname                                    # should be: timeblaster
systemctl status avahi-daemon
avahi-browse -at | grep -i timeblaster

# From your phone or laptop
ping timeblaster.local
dns-sd -B _http._tcp                        # macOS
avahi-browse -at                            # Linux
```

| Cause | Fix |
| --- | --- |
| Avahi not running | `sudo systemctl enable --now avahi-daemon` |
| Hostname not set | `sudo hostnamectl set-hostname timeblaster`, then reboot |
| Your network blocks mDNS | Use the IP address: `nmcli device show wlan0 \| grep IP4.ADDRESS` |
| Windows client | Install Bonjour, or use the IP address |

### The app loads but shows no data

```bash
curl -s http://timeblaster.local:8080/api/health
curl -s http://timeblaster.local:8080/api/state | python3 -m json.tool | head -30
```

If `/api/health` answers but the page is blank, it is almost certainly a stale
service worker. Force-reload, or on iOS remove the Home Screen icon and re-add it.

### The app shows stale data

The WebSocket has dropped. The connection dot at the top right goes red; the app
reconnects with backoff, and refetches whenever it returns to the foreground.

```bash
journalctl -u timeblaster.service | grep -i companion
```

---

## Services

### The daemon will not start

```bash
systemctl status timeblaster.service
journalctl -u timeblaster.service -n 50 --no-pager
sudo timeblasterd --check-config           # validate the config alone
```

Common causes:

| Message | Cause | Fix |
| --- | --- | --- |
| `contains unknown keys` | A typo in the config file | The message names the key |
| `is not a duration` | A bare number where `"5s"` was needed | Quote it with a unit |
| `permission denied` on the database | Wrong ownership after a manual copy | `sudo chown -R timeblaster:timeblaster /var/lib/timeblaster` |
| `address already in use` | Something else is on the port | `sudo ss -tlnp \| grep 8080` |

### It restarts in a loop

```bash
journalctl -u timeblaster.service | grep -E 'panic|critical subsystem'
```

Only a failure of the alarm scheduler itself stops the daemon. Everything else is
supervised: a panicking subsystem is logged with its stack and restarted, and the
rest of the device carries on.

### The database is corrupt

```bash
sudo systemctl stop timeblaster.service
sudo -u timeblaster sqlite3 /var/lib/timeblaster/timeblaster.db 'PRAGMA integrity_check;'
```

If it cannot be repaired, move it aside — the daemon creates a fresh one, losing
alarms and settings but nothing else:

```bash
sudo mv /var/lib/timeblaster/timeblaster.db /var/lib/timeblaster/timeblaster.db.broken
sudo systemctl start timeblaster.service
```

If the database cannot be opened *at all* at startup, the daemon falls back to
in-memory state and says so loudly. Alarms created then do not survive a restart,
but the clock still works.

---

## Collecting information for a bug report

```bash
{
  echo "=== version ==="        ; timeblasterd --version
  echo "=== health ==="         ; tbctl health
  echo "=== os ==="             ; cat /etc/os-release; uname -a
  echo "=== hardware ==="       ; tr -d '\0' < /proc/device-tree/model; echo
  echo "=== services ==="       ; systemctl status --no-pager timeblaster.service timeblaster-wifi.service ersatztv.service
  echo "=== audio ==="          ; cat /proc/asound/cards
  echo "=== serial ==="         ; tbctl ports; ls -l /dev/serial/by-id/ 2>/dev/null
  echo "=== drm ==="            ; ls -l /dev/dri/; id timeblaster
  echo "=== network ==="        ; nmcli device status
  echo "=== config ==="         ; timeblasterd --print-config
  echo "=== logs ==="           ; journalctl -u timeblaster.service -n 200 --no-pager
} > timeblaster-diagnostics.txt 2>&1
```

`--print-config` contains no secrets: the only sensitive value Timeblaster holds
is the setup access point's passphrase, and Wi-Fi credentials for your real
network live in NetworkManager, never in Timeblaster's configuration or database.
