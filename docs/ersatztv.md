# ErsatzTV and the television

ErsatzTV turns folders of media into live TV channels. Timeblaster plays those
channels through mpv on HDMI and selects between them with the channel knob.

## The coupling is deliberately thin

Timeblaster needs exactly three things from ErsatzTV:

1. what channels exist,
2. what they are called,
3. what URL plays them.

All three come from the **IPTV playlist**, `/iptv/channels.m3u` — the same
interface Plex, Jellyfin and Kodi consume. Not the JSON API at `/api/channels`:
ErsatzTV v26 requires authentication there and answers an anonymous request with
`401`, while the playlist stays open. Reading the playlist means Timeblaster
needs no credentials and no configuration to find your channels.

Nothing in the alarm path touches any of it. If ErsatzTV is down, starting,
misconfigured or removed entirely, the television shows the standby image and the
alarm clock is completely unaffected.

```text
timeblasterd ─GET /iptv/channels.m3u─► ErsatzTV :8409
      │                                   │
      │ loadfile over JSON IPC            │ HLS or MPEG-TS
      ▼                                   ▼
    mpv ◄───────────────────────────────────
      │
   DRM/KMS ──► HDMI ──► Television
```

## Installing

`setup.sh` downloads the current `linux-arm64` release into `/opt/ersatztv`,
creates an `ersatztv` system user and installs `ersatztv.service`. Skip it with
`--skip-ersatztv` if you only want the alarm clock.

ErsatzTV is a **separate service with its own failure domain**. `timeblaster.service`
only `Wants=` it, never `Requires=`, so an ErsatzTV crash can never stop the alarm
clock.

```bash
systemctl status ersatztv.service
journalctl -u ersatztv.service -f
```

Its web interface: `http://timeblaster.local:8409`

## Media format

The library should be MP4 containers holding H.264 video and AAC audio:

```text
Container: MP4
Video:     H.264 / AVC
Audio:     AAC
```

This is not arbitrary. In that format ErsatzTV can stream **without transcoding**,
which on a Pi 5 is the difference between a few percent of one core and pinning
all four while FFmpeg re-encodes. It also leaves headroom for the thing that
actually matters — firing an alarm on time — rather than competing with a
transcode for CPU.

Convert with:

```bash
ffmpeg -i input.mkv -c:v libx264 -preset slow -crf 20 \
       -c:a aac -b:a 192k -movflags +faststart output.mp4
```

`-movflags +faststart` puts the index at the front, which makes seeking and stream
startup noticeably quicker.

### Checking what you have

```bash
ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height \
        -of default=noprint_wrappers=1 file.mp4
ffprobe -v error -select_streams a:0 -show_entries stream=codec_name \
        -of default=noprint_wrappers=1 file.mp4
```

Anything that is not `h264` plus `aac` in an MP4 will be transcoded on the fly.

## Adding media and creating channels

1. Put media somewhere the `ersatztv` user can read — external storage is
   strongly preferred over the boot card:
   ```bash
   sudo mkdir -p /srv/media/cartoons
   sudo chown -R ersatztv:ersatztv /srv/media
   ```
2. Open `http://timeblaster.local:8409`.
3. **Media Sources → Local → Add**, point it at the folder, and let it scan.
4. **Collections** to group what belongs together.
5. **Schedules** to decide the ordering — sequential, shuffled, block scheduling.
6. **Channels → Add**. Give it a **number**; that number is what the channel knob
   selects and what the green **CH n** banner shows.

Timeblaster picks up channel changes within `ersatztv.refresh_interval` (default
60 s), or immediately via the companion app's **REFRESH** button or
`curl -X POST http://timeblaster.local:8080/api/channels/refresh`.

## "Not configured for hardware acceleration (Nvenc)"

ErsatzTV's health page reports this against every channel on a Raspberry Pi.
**Ignore it. Acting on it will break playback.**

The warning comes from ErsatzTV noticing that its FFmpeg build has NVENC
compiled in while no profile uses it. Compiled in is not the same as present:
NVENC is NVIDIA's encoder, and a Raspberry Pi has no NVIDIA GPU.

Worse, the Pi 5 has **no hardware video encoder at all**. The Pi 4's H.264
encoder block was dropped; what remains is an HEVC *decoder* (`/dev/video19`,
`rpi-hevc-dec`) and the camera ISP. Both hardware encoders fail outright:

```console
$ ffmpeg -f lavfi -i testsrc -frames:v 2 -c:v h264_nvenc -f null -
[h264_nvenc] Cannot load libcuda.so.1

$ ffmpeg -f lavfi -i testsrc -frames:v 2 -c:v h264_v4l2m2m -f null -
[h264_v4l2m2m] Could not find a valid device
```

So leaving hardware acceleration set to **None** is the only correct setting
here, and the warning is a false positive on this hardware.

### What to do instead

The lever that actually matters on a Pi is **not transcoding in the first
place**. Software encoding with libx264 works, but it burns CPU and heat that
the device would rather spend elsewhere — and the whole point of the media
format below is to avoid needing it.

```bash
# While a channel is playing. An idle ffmpeg means it is streaming direct.
top -b -n1 | head -15
```

If FFmpeg is busy, the fix is the media, not the encoder: convert the library to
H.264/AAC in MP4 and try `stream_mode = "hls-direct"`.

## Streaming mode

```toml
[ersatztv]
stream_mode = "mixed"      # mixed | hls | ts | hls-direct | segmenter | …
stream_format = "m3u8"     # m3u8 (HLS) or ts (MPEG-TS)
```

* `mixed` — the default. ErsatzTV decides per channel. A safe starting point.
* `hls-direct` — avoids transcoding when the media is already in the intended
  format. **Try this once your library is H.264/AAC in MP4**; it is by far the
  lightest option, and the one this project is designed around.
* `ts` with `stream_format = "ts"` — MPEG-TS. Sometimes more tolerant of odd
  media, at the cost of more transcoding.

Watch the effect:

```bash
# While a channel is playing:
top -b -n1 | head -15          # is ffmpeg busy?
journalctl -u ersatztv.service -f | grep -i ffmpeg
```

An idle `ffmpeg` means direct streaming; a busy one means transcoding.

## How the channel knob maps to channels

The knob is an **absolute-position selector**, not an endless encoder. With N
channels its travel is divided into N equal bands:

```text
 4 channels:    0–25%      25–50%     50–75%     75–100%
                  ch 1       ch 2       ch 3       ch 4

10 channels:   0–10% 10–20% 20–30% … 90–100%
                ch 1   ch 2   ch 3     ch 10
```

The channel count is **never hardcoded**. Add a channel in ErsatzTV and the knob
re-divides within a minute, with no restart — and the knob's current position is
re-evaluated immediately, so the right channel is selected without touching
anything.

Channels are ordered by number, sorted numerically, so `2.1` sits between `2` and
`10` rather than where a plain string sort would put it.

Hysteresis of `input.channel_hysteresis` (default 0.25 of one band) stops ADC
noise on a boundary flipping the channel back and forth. Deliberately sweeping
across several bands is always honoured immediately.

## The standby image

Whenever no channel is selected — at boot, when the knob sits past the end of a
shrunken channel list, or when ErsatzTV has no channels — the television shows a
fullscreen static image:

```toml
[mpv]
no_channel_image = "/usr/share/timeblaster/assets/no-channel.png"
```

This is what keeps a Linux console off the television. mpv stays running with the
image loaded rather than exiting, so there is never a moment where the desktop-less
console is visible.

Replace it with anything mpv can display:

```bash
sudo cp my-image.png /usr/share/timeblaster/assets/no-channel.png
sudo systemctl restart timeblaster.service
```

The shipped image is generated by `scripts/make-assets.py`, so it can always be
regenerated rather than being an unexplainable binary in the repository.

## The channel-change banner

Every channel change draws a brief green banner over the video:

```text
CH 3
```

It is drawn with mpv's own ASS overlay (`osd-overlay`), not a second graphical
stack — there is no compositor on a Lite install, and starting one for a
two-second banner would be absurd. It **does not restart playback**.

The banner **holds until the picture actually arrives**, then stays for
`channel_overlay_duration` and goes. Tuning takes a second or two warm and
considerably longer when ErsatzTV has to cold-start a channel, so hiding it on a
fixed timer meant it came and went while the screen still showed the previous
content — exactly when the viewer most wants to know something is happening.
`channel_overlay_max_hold` caps the wait, for a stream that never starts at all.

```toml
[overlay]
channel_overlay_enabled = true
channel_overlay_duration = "2.5s"
channel_overlay_max_hold = "20s"
channel_overlay_text_format = "CH %s"     # or "CH %s - %s" to include the name
channel_overlay_color = "#33FF33"
channel_overlay_font_size = 96
channel_overlay_position = "top-left"     # top-right | bottom-left | bottom-right | center
channel_overlay_margin_x = 64
channel_overlay_margin_y = 48
channel_overlay_outline = 3
```

The overlay canvas is 1280×720 and mpv scales it to the television's real
resolution, so `font_size` looks the same on a 720p set and a 4K one. If your
television overscans and clips the banner, increase the margins.

## Recovery

Every failure in this path is handled without human intervention:

| Failure | What happens |
| --- | --- |
| ErsatzTV is not up yet at boot | Timeblaster retries with exponential backoff (2 s → 60 s) and stays fully functional meanwhile |
| ErsatzTV restarts | The channel list is **kept**, not discarded; the television keeps playing |
| The selected channel is deleted | The selection is dropped and the standby image is shown |
| A stream ends or errors | mpv reports `end-file`; Timeblaster reloads the same channel, falling back to the standby image if that fails |
| mpv crashes | The supervisor restarts it with backoff and reloads the current channel |
| The television is switched off and on | mpv and DRM/KMS handle the HDMI renegotiation; nothing on the Pi needs to change |

## Diagnosing

```bash
tbctl channels                    # what Timeblaster sees
tbctl channel 3                   # select one by hand
tbctl channel off                 # show the standby image

# What ErsatzTV says. This is the exact request Timeblaster makes.
curl -s http://127.0.0.1:8409/iptv/channels.m3u

# /api/ needs authentication in v26 and will answer 401; that is expected and
# is not what Timeblaster uses.
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8409/api/channels

# Play a channel directly, bypassing Timeblaster, to isolate the problem:
mpv --vo=gpu --gpu-context=drm 'http://127.0.0.1:8409/iptv/channel/1.m3u8?mode=mixed'

journalctl -u timeblaster.service -f | grep -E 'channel|mpv|ersatztv'
```

### Common problems

| Symptom | Cause | Fix |
| --- | --- | --- |
| `ersatztv: down` in health | Not running, or still starting | `systemctl status ersatztv.service` |
| `ersatztv: down` while the web UI works | The playlist is not reachable | `curl -s http://127.0.0.1:8409/iptv/channels.m3u` — it should begin `#EXTM3U` |
| No channels listed | None created yet | Create one at `:8409` |
| Knob does nothing | No channels, or the Nano is disconnected | `tbctl channels`, `tbctl health` |
| Channels flicker at a boundary | Hysteresis too low for your pots | Raise `input.channel_hysteresis` to 0.35 |
| Video stutters, CPU pinned | Transcoding | Convert the library to H.264/AAC MP4; try `stream_mode = "hls-direct"` |
| Black screen, no standby image | mpv cannot open DRM | See [troubleshooting.md](troubleshooting.md#the-television-is-black) |
