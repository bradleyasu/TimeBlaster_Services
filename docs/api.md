# HTTP and WebSocket API

Base URL: `http://timeblaster.local:8080`

The companion app uses nothing else, so anything the app can do is available here.
The Raspberry Pi is the source of truth; these endpoints ask it to do things and
read what it thinks. Closing the app has no effect on the device.

There is no authentication. The device is intended for a home LAN, and it holds
nothing sensitive: Wi-Fi credentials live in NetworkManager, never here.

## Conventions

* All bodies are JSON. Unknown fields are **rejected**, so a client typo is
  reported rather than ignored.
* Bodies are capped at 64 KiB.
* Failures return `{"error": "…", "detail": "…"}`.
* `PUT` bodies are partial: omitted fields keep their current value.

## Diagnostics

### `GET /api/health`

Component health. Always returns 200 — a degraded television is not an HTTP
error. Exposes no paths, credentials or SSIDs.

```json
{
  "status": "degraded",
  "version": "1.0.0",
  "uptime": "2h13m40s",
  "components": {
    "alarm": "ok", "alarm_audio": "ok", "alarm_sounds": "ok",
    "ersatztv": "ok", "nano": "down", "network": "ok",
    "tv_player": "ok", "web": "ok", "wifi_helper": "ok"
  },
  "details": {
    "nano_connected": false,
    "ersatztv_reachable": true,
    "tv_player_alive": true,
    "alarm_audio_device_available": true,
    "alarm_subsystem_healthy": true,
    "wifi_helper_reachable": true,
    "wifi_mode": "normal",
    "current_channel": "3",
    "channel_count": 4,
    "alarm_count": 2,
    "alarm_ringing": false
  }
}
```

`status` is `ok`, `degraded`, or `unhealthy` — the last reserved for an impaired
alarm path, because that is the one thing the device exists to do.

### `GET /api/state`

The complete live picture: clock, alarms, audio, media, hardware, Wi-Fi, channels
and the physical knobs' positions. This is what the WebSocket sends on connection.

Knob positions read `-1` until that knob has reported, which makes "is this pot
wired up?" answerable from the app.

## Alarms

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/alarms` | List, with each alarm's computed next occurrence |
| `POST` | `/api/alarms` | Create (`hour` and `minute` required) |
| `GET` | `/api/alarms/{id}` | One alarm |
| `PUT` | `/api/alarms/{id}` | Partial update |
| `DELETE` | `/api/alarms/{id}` | Delete |
| `POST` | `/api/alarms/{id}/enabled` | `{"enabled": true}` |
| `POST` | `/api/alarms/{id}/trigger` | Ring it now, for testing |

```jsonc
{
  "id": 1,
  "label": "Wake up",
  "hour": 6,
  "minute": 30,
  "enabled": true,
  "repeat_days": 62,          // bitmask, bit 0 = Sunday; 62 = weekdays
  "repeat_label": "weekdays",
  "one_shot_date": "",        // "YYYY-MM-DD" pins a one-shot alarm to a date
  "sound_id": "alarm1",
  "snooze_minutes": 9,
  "auto_stop_minutes": 15,
  "next_occurrence": "2026-09-21T06:30:00-04:00"
}
```

Repeat-day bits: Sunday `1`, Monday `2`, Tuesday `4`, Wednesday `8`, Thursday
`16`, Friday `32`, Saturday `64`. Weekdays `62`, weekends `65`, every day `127`,
one-shot `0`.

### The ringing alarm

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/alarm/active` | The active alarm and the next scheduled one |
| `POST` | `/api/alarm/dismiss` | Same as the big red button. `409` when nothing is ringing |
| `POST` | `/api/alarm/snooze` | `409` when nothing is ringing or it is already snoozed |

## Sounds and volume

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/sounds` | Available sounds, including the built-in fallback |
| `POST` | `/api/sounds/{id}/preview` | Play once on the alarm speaker. `409` during a real alarm |
| `POST` | `/api/sounds/preview/stop` | Stop playback |
| `GET` | `/api/volume` | Current volume |
| `PUT` | `/api/volume` | `{"percent": 65}` — **`409` unless `audio.allow_software_volume` is set** |

```json
{ "percent": 65, "knob_authoritative": true, "software_control": false }
```

`knob_authoritative` tells the app the value came from the physical
potentiometer, so it can present it read-only rather than offering a slider that
would immediately be overridden.

## Television

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/channels` | Channels, the current one, and subsystem status |
| `POST` | `/api/channels/select` | `{"number": "3"}`. `404` for an unknown channel |
| `POST` | `/api/channels/clear` | Show the standby image |
| `POST` | `/api/channels/refresh` | Re-read the channel list now (`202`) |

A software channel selection holds only until the knob is next moved; the
response says so, because the channel knob is an absolute-position control and the
app should not appear to have failed when the knob later wins.

## Settings

`GET` / `PUT` `/api/settings`:

```json
{
  "timezone": "America/New_York",
  "clock_24h": false,
  "display_brightness": 75,
  "default_sound_id": "alarm1",
  "channel_overlay_enabled": true
}
```

`PUT` is partial. An unknown timezone or an out-of-range brightness is a `400`.

## Networking

These proxy to the privileged helper; the daemon itself can do none of it.

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/wifi/status` | Mode, SSID, address |
| `GET` | `/api/wifi/networks` | Scan |
| `POST` | `/api/wifi/setup` | Enter setup mode — **disconnects the caller** |
| `DELETE` | `/api/wifi/setup` | Leave setup mode |
| `POST` | `/api/wifi/connect` | `{"ssid": "…", "passphrase": "…", "hidden": false}` |

Credentials are validated against the 802.11 limits, and control characters are
rejected, before anything reaches `nmcli`. A `502` carries a `ConnectResult`
explaining what went wrong and whether the previous network was restored.

## WebSocket

`ws://timeblaster.local:8080/api/ws`

On connection the server sends a `state` event carrying a full snapshot. After
that it sends an event whenever something changes:

```json
{ "type": "channel_changed", "at": "2026-09-18T06:31:02-04:00", "data": { … } }
```

| Type | Meaning |
| --- | --- |
| `state` | Full snapshot, sent on connection |
| `tick` | The Pi's current time, once a second while anyone is listening |
| `alarm_started` / `alarm_stopped` / `alarm_snoozed` | The ringing alarm |
| `alarms_changed` / `schedule_changed` | The alarm set or the next-due time |
| `channel_changed` / `channels_changed` | Television |
| `volume_changed` | The alarm speaker volume |
| `hardware_changed` | The Nano link coming up or going down |
| `wifi_changed` | Entering or leaving setup mode |
| `settings_changed` | Preferences |

A client that stops reading is **disconnected, not waited on**: a phone that went
to sleep with the app open must never be able to stall the alarm scheduler.

The `tick` event is why the app's clock stays anchored to the Pi rather than to
the phone — the Pi is the authoritative clock.

## Examples

```bash
# Health, with a non-zero exit when degraded
tbctl health

# Create a weekday alarm
curl -X POST http://timeblaster.local:8080/api/alarms \
  -H 'Content-Type: application/json' \
  -d '{"hour":6,"minute":30,"label":"Work","repeat_days":62,"sound_id":"alarm1"}'

# Disable it
curl -X POST http://timeblaster.local:8080/api/alarms/1/enabled \
  -H 'Content-Type: application/json' -d '{"enabled":false}'

# Dismiss whatever is ringing
curl -X POST http://timeblaster.local:8080/api/alarm/dismiss

# Watch events
websocat ws://timeblaster.local:8080/api/ws
```
