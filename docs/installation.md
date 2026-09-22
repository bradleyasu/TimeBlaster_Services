# Installation

## What you need

* A Raspberry Pi 5 with **Raspberry Pi OS Lite (64-bit)** freshly imaged.
  Timeblaster targets Debian 13 "trixie", which is what current Raspberry Pi OS
  is based on; it uses NetworkManager, not the old `dhcpcd`/`wpa_supplicant`
  arrangement.
* Network access for the first install (packages, the Go toolchain, ErsatzTV).
* The hardware from [hardware.md](hardware.md), though the Nano and the USB
  speaker can be attached later.

## Install

```bash
# On the Pi
git clone <your-repo-url> timeblaster
cd timeblaster
sudo ./setup.sh
```

That is the whole procedure. The script is idempotent: rerun it any time to
upgrade, re-verify, or reprint the status summary.

### What setup.sh does

1. **Checks the environment** — root, Debian family, architecture, free space,
   network. It warns rather than failing on anything survivable.
2. **Installs packages** — `mpv`, `alsa-utils`, `avahi-daemon`, `libnss-mdns`,
   `network-manager`, `dnsmasq-base`, `sqlite3`, `curl`, `libicu-dev`. Package
   metadata is only refreshed if it is more than an hour old, so reruns are quick.
3. **Installs Go** if the packaged toolchain is too old to build the module.
4. **Creates the `timeblaster` system account** and adds it to `dialout` (serial),
   `audio` (the USB speaker) and `video`/`render` (DRM/KMS for mpv).
5. **Creates directories** — `/etc/timeblaster`, `/var/lib/timeblaster`,
   `/var/lib/timeblaster/alarm-sounds`, `/usr/share/timeblaster/assets`.
6. **Builds and installs** `timeblasterd`, `timeblaster-wifi` and `tbctl` into
   `/usr/local/bin`.
7. **Installs the configuration** — *without overwriting yours*. If
   `/etc/timeblaster/timeblaster.toml` exists and differs from the template, the
   new template is written as `timeblaster.toml.new` and the difference is
   reported.
8. **Installs assets** — the standby image, the udev rule that creates
   `/dev/timeblaster-nano`, and the Avahi service file.
9. **Sets the hostname** to `timeblaster` and configures mDNS.
10. **Makes the television look like a product**, not a Linux box:
    * quiets the boot — `console=tty3 quiet loglevel=3 logo.nologo
      vt.global_cursor_default=0 consoleblank=0 systemd.show_status=false`;
    * **disables the login prompt** on the VT the television shows, moving the
      local console login to Ctrl+Alt+F2;
    * installs a boot screen that paints **TIMEBLASTER — BOOTING, PLEASE STAND
      BY...** from early boot until mpv has the picture.
11. **Installs ErsatzTV** — downloads the current `linux-arm64` release into
    `/opt/ersatztv` and creates its own service with its own user. Skipped if the
    same version is already installed.
12. **Installs, enables and starts the systemd units**, then prints a summary.

It **never reboots by itself**. If the hostname or console options changed, it
says so and leaves the decision to you.

### Useful flags

```bash
sudo ./setup.sh --dry-run          # print the plan, change nothing
sudo ./setup.sh --keep-console     # leave the Debian login prompt on the TV
sudo ./setup.sh --skip-ersatztv    # alarm clock only, no television
sudo ./setup.sh --skip-packages    # reruns on a known-good system
sudo ./setup.sh --skip-build       # reinstall config and units, keep binaries
sudo ./setup.sh --force-config     # overwrite the config (backs up the old one)
sudo ./setup.sh --uninstall        # stop and disable the services, keep all data
```

## After installing

### 1. Reboot if asked

```bash
sudo reboot
```

The quiet-boot and login-prompt changes only take effect after this. On the next
boot the television should go from dark, to the Timeblaster boot screen, to the
standby screen — with no kernel messages and no login prompt at any point.

**Getting a local login afterwards.** The prompt moves off the television's
virtual terminal, so press **Ctrl+Alt+F2** for one. SSH and the serial console
are unchanged. Reverse it entirely with `sudo ./setup.sh --keep-console`, or:

```bash
sudo systemctl enable --now getty@tty1.service
```

### 2. Add alarm sounds

Two MP3s are the intended starting point, but any number works, and WAV, OGG,
FLAC, M4A and AAC are accepted too.

```bash
sudo cp alarm1.mp3 alarm2.mp3 /var/lib/timeblaster/alarm-sounds/
sudo chown timeblaster:timeblaster /var/lib/timeblaster/alarm-sounds/*.mp3
```

New files appear within five minutes, or immediately after
`sudo systemctl restart timeblaster.service`.

Until at least one file is present, alarms ring with a built-in generated tone.
It works and it is loud, but it is not pleasant.

### 3. Point Timeblaster at the right speaker

This is the step most worth getting right: the alarm must come out of the bedside
speaker, not the television.

```bash
cat /proc/asound/cards
```

```text
 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0
 1 [vc4hdmi1       ]: vc4-hdmi - vc4-hdmi-1
 2 [Device         ]: USB-Audio - USB Audio Device
```

The default configuration picks the first USB audio card automatically, which is
usually right. To be explicit, use the **card id** (the name in brackets) rather
than the number, because numbers move between reboots:

```toml
[audio]
alarm_device = "Device"          # or "hw:CARD=Device,DEV=0", or a substring like "USB"
```

Then:

```bash
sudo timeblasterd --check-config
sudo systemctl restart timeblaster.service
tbctl health                     # alarm_audio should read "ok"
tbctl play alarm1                # listen: it must come from the speaker
```

See [audio.md](audio.md) for the full story.

### 4. Set up ErsatzTV

Open `http://timeblaster.local:8409`, add your media folders and create channels.
See [ersatztv.md](ersatztv.md) for the media format that avoids transcoding.

Timeblaster picks up channel changes within a minute, or immediately via
`tbctl` → the companion app's **REFRESH** button.

### 5. Open the companion app

`http://timeblaster.local` (or `:8080` — the default port).

On an iPhone: open it in Safari, tap **Share → Add to Home Screen**. It then
launches full-screen like an app.

### 6. Set up Wi-Fi if the Pi is not already on your network

Hold the Wi-Fi button for five seconds. See [wifi-setup.md](wifi-setup.md).

## Verifying the installation

```bash
tbctl health                    # component summary; exits non-zero when degraded
tbctl alarms                    # the alarm list
tbctl channels                  # ErsatzTV channels
tbctl ports                     # serial ports, to confirm the Nano is seen
systemctl status timeblaster.service
journalctl -u timeblaster.service -n 50
```

A healthy freshly installed device reports:

```text
status   OK

  alarm          ok
  alarm_audio    ok
  alarm_sounds   ok
  ersatztv       ok
  nano           ok
  network        ok
  tv_player      ok
  web            ok
  wifi_helper    ok
```

`degraded` with `nano: down` simply means the Arduino is not plugged in yet.
Everything else keeps working.

## Upgrading

```bash
cd timeblaster
git pull
sudo ./setup.sh
```

Your configuration, alarms, settings and sounds are all preserved. If the
configuration template gained new options, the script tells you and leaves
`timeblaster.toml.new` for you to compare.

## Reinstalling and restoring

### Reinstalling the software, keeping the data

```bash
sudo ./setup.sh --force-config     # only if you also want the stock config back
```

`/var/lib/timeblaster` is never touched by the installer.

### Backing up

Everything that matters is two paths:

```bash
sudo tar czf timeblaster-backup.tar.gz \
  /etc/timeblaster \
  /var/lib/timeblaster
```

That covers alarms, settings, and sound files. ErsatzTV's own data lives
separately in `/var/lib/ersatztv`.

### Restoring onto a fresh card

```bash
sudo ./setup.sh
sudo systemctl stop timeblaster.service
sudo tar xzf timeblaster-backup.tar.gz -C /
sudo chown -R timeblaster:timeblaster /var/lib/timeblaster
sudo systemctl start timeblaster.service
```

### Factory reset

There is deliberately **no factory-reset button**, and entering Wi-Fi setup mode
is emphatically not one. To start over:

```bash
sudo systemctl stop timeblaster.service
sudo rm /var/lib/timeblaster/timeblaster.db      # alarms and settings
sudo systemctl start timeblaster.service
```

Sound files and Wi-Fi credentials are untouched by that. To also forget the
network, use `nmcli connection delete <name>`.

## Uninstalling

```bash
sudo ./setup.sh --uninstall
```

Stops and disables the services and leaves everything else in place. To remove
the rest:

```bash
sudo rm -rf /etc/timeblaster /var/lib/timeblaster /usr/share/timeblaster
sudo rm -f /usr/local/bin/{timeblasterd,timeblaster-wifi,tbctl}
sudo rm -f /etc/systemd/system/timeblaster*.service
sudo rm -f /etc/udev/rules.d/99-timeblaster.rules
sudo rm -f /etc/avahi/services/timeblaster.service
sudo systemctl daemon-reload
sudo userdel timeblaster
```

## Installing without the script

If you would rather do it by hand, the script is a readable checklist. The parts
that are easy to get wrong:

* The service account needs `dialout`, `audio`, `video` **and** `render`. Missing
  `render` gives a confusing mpv failure on the Pi 5.
* `/dev/ttyACM0` is not stable. Use the udev rule, or `/dev/serial/by-id/*`.
* `ipv4.method shared` on the setup access point is what gives you DHCP and DNS
  with no second daemon to configure.
* ErsatzTV needs `libicu-dev`; without it the .NET runtime fails to start with an
  unhelpful message.
