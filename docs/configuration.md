# Configuration

Everything tunable lives in one file:

```text
/etc/timeblaster/timeblaster.toml
```

The shipped copy is [`deploy/config/timeblaster.toml`](../deploy/config/timeblaster.toml),
which is annotated and contains every value at its built-in default — so a
commented-out line and a deleted line behave identically. A test in
`internal/config` asserts that, so the promise cannot quietly stop being true.

## Editing safely

```bash
sudo nano /etc/timeblaster/timeblaster.toml
sudo timeblasterd --check-config              # validate before restarting
sudo systemctl restart timeblaster.service
journalctl -u timeblaster.service -n 30
```

To see what is actually in effect, defaults included:

```bash
timeblasterd --print-config
```

Its output is itself valid TOML, so it doubles as a way to generate a complete
config file.

## Two rules worth knowing

**Unknown keys are a startup error.** A typo is reported by name rather than
silently doing nothing. On an appliance, a setting that quietly never applied is
the kind of thing you discover six months later.

**Validation is total.** If `--check-config` passes, no component needs to
re-check ranges at runtime, and every problem is reported at once rather than one
per restart:

```text
configuration: /etc/timeblaster/timeblaster.toml: general.hostname must not be empty
logging.level must be debug, info, warn or error, got "loud"
input.filter_alpha must be in (0,1], got 3
```

## Configuration versus the database

Two layers, deliberately:

| Layer | Holds | Wins |
| --- | --- | --- |
| `timeblaster.toml` | The installation's defaults and everything hardware-related | Read at startup |
| `timeblaster.db` | The user's choices made in the companion app | Overrides the file |

So reinstalling does not discard preferences, and editing the file does not
silently override a choice the user made in the app. The overlap is small:
timezone, 12/24-hour, display on/off, default sound, overlay on/off.

## Sections

### `[general]`

| Key | Default | Notes |
| --- | --- | --- |
| `hostname` | `"timeblaster"` | The mDNS name. Changing it needs a reboot. |
| `timezone` | `""` | IANA name. Empty means the system timezone, resolved to its real name for display. The app's setting overrides this. |
| `clock_24h` | `false` | |
| `display_on` | `true` | Whether the 7-segment display is lit. A switch, not a level: the display has no dimmer. See [hardware.md](hardware.md#display). |
| `restore_channel_on_boot` | `false` | Off because the channel knob is absolute: its physical position selects the right channel within a second anyway. |

### `[logging]`

| Key | Default | Notes |
| --- | --- | --- |
| `level` | `"info"` | `debug` includes every ADC reading — a firehose. |
| `format` | `"text"` | logfmt; reads well in `journalctl`. `json` suits log shipping. |
| `log_pot_readings` | `false` | Separate from `debug` because it drowns everything else. |

### `[storage]`

| Key | Default |
| --- | --- |
| `database_path` | `/var/lib/timeblaster/timeblaster.db` |
| `alarm_sounds_dir` | `/var/lib/timeblaster/alarm-sounds` |
| `runtime_dir` | `/run/timeblaster` (provided by systemd) |

### `[serial]`

| Key | Default | Notes |
| --- | --- | --- |
| `device` | `""` | An explicit path. Prefer `device_globs`. |
| `device_globs` | `/dev/timeblaster-nano`, `/dev/serial/by-id/*Arduino*`, `/dev/serial/by-id/*` | Tried in order. `/dev/ttyACM0` is **not** stable. |
| `baud_rate` | `115200` | USB CDC ignores it; stated for clarity. |
| `time_sync_interval` | `"1m0s"` | The Nano free-runs between syncs, so this can be generous. |
| `heartbeat_timeout` | `"8s"` | No message for this long ⇒ reopen the port. Catches a half-open USB connection. |
| `reconnect_min_backoff` / `reconnect_max_backoff` | `500ms` / `15s` | |
| `write_queue_size` | `64` | When full, non-critical commands are dropped rather than blocking a caller. |

### `[input]`

The knob-feel section. See [hardware.md](hardware.md#calibration).

| Key | Default | Raise it when… |
| --- | --- | --- |
| `adc_min` / `adc_max` | `0` / `4095` | The whole range is compressed |
| `end_margin_percent` | `2.0` | A knob cannot reach 0 % or 100 % |
| `invert_channel_pot` / `invert_volume_pot` | `false` | A knob reads backwards |
| `filter_alpha` | `0.35` | *Lower* it when readings are noisy |
| `filter_snap_threshold` | `0.08` | Fast deliberate turns feel laggy (lower it) |
| `channel_hysteresis` | `0.25` | Channels flicker at a boundary |
| `volume_deadband_percent` | `2` | Volume jitters |
| `wifi_hold_duration` | `"5s"` | The hold feels too short or too long |
| `min_press_duration` | `"30ms"` | Contact bounce survives firmware debouncing |
| `hold_poll_interval` | `"100ms"` | Rarely |

### `[audio]`

See [audio.md](audio.md).

| Key | Default | Notes |
| --- | --- | --- |
| `alarm_device` | `""` | The USB speaker. Use a card **id** or a description substring, never an index. |
| `mixer_control` | `""` | `""` detects; `"none"` forces software volume. |
| `startup_volume_percent` | `40` | Until the knob reports. Persisted volume is never replayed. |
| `allow_software_volume` | `false` | Even when true, the knob wins on its next movement. |
| `player_binary` | `"mpv"` | |
| `extra_player_args` | `[]` | |
| `default_sound_id` | `"alarm1"` | |
| `device_recheck_interval` | `"30s"` | How often an absent speaker is re-probed. |
| `test_duration` | `"10s"` | Caps a preview from the app. |

### `[ersatztv]`

See [ersatztv.md](ersatztv.md).

| Key | Default | Notes |
| --- | --- | --- |
| `base_url` | `http://127.0.0.1:8409` | |
| `request_timeout` | `"5s"` | |
| `refresh_interval` | `"1m0s"` | How often the channel list is re-read. |
| `retry_min_backoff` / `retry_max_backoff` | `2s` / `1m0s` | Covers ErsatzTV's slow cold start. |
| `stream_mode` | `"mixed"` | Try `"hls-direct"` once the library is H.264/AAC MP4. |
| `stream_format` | `"m3u8"` | Or `"ts"`. |

### `[mpv]`

See [troubleshooting.md](troubleshooting.md#the-television-is-black) for
alternative output configurations.

| Key | Default | Notes |
| --- | --- | --- |
| `binary` | `"mpv"` | |
| `ipc_socket` | `/run/timeblaster/mpv-tv.sock` | Channel changes go over this, not a restart. |
| `args` | DRM/KMS, fullscreen, idle | `--input-ipc-server` is added automatically; do not list it. |
| `no_channel_image` | `/usr/share/timeblaster/assets/no-channel.png` | Keeps the console off the television. |
| `booting_image` | `/usr/share/timeblaster/assets/booting.png` | Shown until the channel list first loads. The same screen the boot splash paints, so the handover is invisible. Empty falls back to the no-channel image. |
| `booting_timeout` | `"1m30s"` | How long the booting screen may stay up before giving way to the no-channel screen. |
| `restart_min_backoff` / `restart_max_backoff` | `1s` / `30s` | |
| `startup_timeout` | `"15s"` | How long to wait for the IPC socket. |
| `command_timeout` | `"5s"` | |

### `[overlay]`

The green **CH n** banner. All keys are prefixed `channel_overlay_`.

| Key | Default |
| --- | --- |
| `channel_overlay_enabled` | `true` |
| `channel_overlay_duration` | `"2.5s"` — how long it stays once the picture has arrived |
| `channel_overlay_max_hold` | `"20s"` — how long it may wait for that picture |
| `channel_overlay_text_format` | `"CH %s"` — a second `%s` receives the channel name |
| `channel_overlay_color` | `"#33FF33"` |
| `channel_overlay_font_size` | `96` on a 1280×720 canvas, scaled to the real resolution |
| `channel_overlay_position` | `"top-left"` |
| `channel_overlay_margin_x` / `_y` | `64` / `48` — raise these if your television overscans |
| `channel_overlay_outline` | `3` |

### `[web]`

| Key | Default | Notes |
| --- | --- | --- |
| `listen_address` | `":8080"` | Port 80 works (the unit grants `CAP_NET_BIND_SERVICE`). Update the Avahi service file to match. |
| `read_timeout` / `write_timeout` / `shutdown_timeout` | `15s` / `30s` / `10s` | |
| `static_dir` | `""` | Serve the app from disk instead of the embedded copy; development only. |
| `websocket_ping_interval` | `"20s"` | Keeps connections alive through phone power management. |

### `[wifi]`

See [wifi-setup.md](wifi-setup.md).

| Key | Default | Notes |
| --- | --- | --- |
| `helper_socket` | `/run/timeblaster/wifi.sock` | |
| `interface` | `"wlan0"` | |
| `setup_ssid` | `"TIMEBLASTER-SETUP"` | |
| `setup_passphrase` | `""` | Empty is an open network; only live while you are standing in front of the device. |
| `setup_address` | `"10.42.0.1/24"` | The portal is at this address. |
| `setup_timeout` | `"15m0s"` | Setup mode ends by itself. |
| `connect_timeout` | `"45s"` | |
| `validate_timeout` | `"30s"` | How long to wait for a real IP before accepting a network. |
| `scan_timeout` | `"20s"` | |

## Common adjustments

**A quieter bedroom.** Lower the maximum the knob can reach by capping the
hardware mixer instead of changing Timeblaster:

```bash
amixer -c 2 sset PCM 60% && sudo alsactl store
```

**An alarm that rings longer.** Per alarm, in the app, or as a default:

```toml
# There is no global default for auto-stop; it is per alarm, 1-120 minutes.
# Set it when creating the alarm, or:
curl -X PUT http://timeblaster.local:8080/api/alarms/1 \
  -H 'Content-Type: application/json' -d '{"auto_stop_minutes":30}'
```

**Sticky knobs.**

```toml
[input]
filter_alpha = 0.2            # smooth harder
channel_hysteresis = 0.35     # and demand more deliberate movement
```

**A bigger channel banner on a large television.**

```toml
[overlay]
channel_overlay_font_size = 140
channel_overlay_margin_x = 100
channel_overlay_margin_y = 80
```

**Serving the app on port 80.**

```toml
[web]
listen_address = ":80"
```

Then update `/etc/avahi/services/timeblaster.service` to advertise port 80, and
restart both services.
