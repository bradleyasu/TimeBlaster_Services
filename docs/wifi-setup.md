# Wi-Fi setup

## The one and only procedure

**Hold the Wi-Fi button for five seconds.**

That is the entire user interface for networking. It is the same procedure for
every situation:

| Situation | What you do |
| --- | --- |
| A brand-new Timeblaster | Hold the button for five seconds |
| You replaced your router | Hold the button for five seconds |
| You changed your Wi-Fi password | Hold the button for five seconds |
| You moved the Timeblaster to a new house | Hold the button for five seconds |
| The current network works but you want a different one | Hold the button for five seconds |

There is no second path, no hidden reset hole, and no "hold it for ten seconds
instead" variant.

## What Timeblaster deliberately does not do

**It never starts a setup access point on its own** because a network is
unreachable.

This is a considered decision. A device that spawns an open access point every
time the router reboots, or every time the Wi-Fi drops for thirty seconds, is
worse than one that waits: it broadcasts an open network in your house at
unpredictable times, and it does so precisely when you are least likely to be
watching. Setup mode happens when a person is standing in front of the device
holding a button, and at no other time.

Losing Wi-Fi also has no effect whatsoever on the alarm clock. Alarms are
scheduled locally, stored locally, and ring locally.

## The procedure in detail

```text
 Hold the Wi-Fi button for 5 seconds
              │
              ▼
 The display shows SETUP, the Wi-Fi LED lights,
 and "WI-FI SETUP" appears briefly on the television
              │
              ▼
 The Pi raises an access point:  TIMEBLASTER-SETUP
              │
              ▼
 Join it from your phone. The setup page opens by itself
 (it answers the captive-portal probes iOS and Android use)
              │
              ▼
 Choose your network, type the password, tap CONNECT
              │
              ▼
 The access point shuts down and the Pi tries to join
              │
        ┌─────┴─────┐
        ▼           ▼
    success      failure
        │           │
        │           └─► the previous network is restored,
        │               setup mode comes back, and the page
        │               explains what went wrong
        ▼
 The Pi joins your LAN. Reach it at http://timeblaster.local
```

### On the device

1. Hold the Wi-Fi button. After five seconds the 7-segment display shows `SETUP`
   and the Wi-Fi LED lights. If the television is on, a green **WI-FI SETUP**
   banner appears over the video.
2. The journal records it plainly:
   ```text
   Wi-Fi button held; entering setup mode held=5.001s ssid=TIMEBLASTER-SETUP
   entering Wi-Fi setup mode ssid=TIMEBLASTER-SETUP interface=wlan0 address=10.42.0.1/24 secured=false
   remembering the current network for rollback connection="Home Network"
   captive portal started address=http://10.42.0.1
   Wi-Fi setup mode is active ssid=TIMEBLASTER-SETUP portal=http://10.42.0.1 expires_at=…
   ```

### On your phone

3. Open Wi-Fi settings and join **TIMEBLASTER-SETUP**.
4. The setup page should open by itself. If it does not, browse to
   **http://10.42.0.1**.
5. Pick your network from the list, or tick *Enter a hidden network name* and
   type it.
6. Enter the password and tap **CONNECT**.

The page will stop responding at that point — the access point has to come down
before the radio can join your network. That is expected, and the page says so.

### Afterwards

7. Rejoin your normal Wi-Fi.
8. Open **http://timeblaster.local**.

Setup mode ends by itself after `wifi.setup_timeout` (default 15 minutes) if
nothing happens, so a forgotten access point does not stay up.

## Your existing credentials are safe

Timeblaster **never discards known-good credentials until the replacement is
proven to work.**

The mechanics: `nmcli device wifi connect` creates a profile named after the new
SSID without touching any other profile. The old network's credentials stay on
disk throughout. A new connection is only accepted once the interface reports
both **connected** *and* an actual IPv4 address — because "associated with the
access point" and "on a working network" are not the same thing, and DHCP failing
is a real outcome.

If either step fails, the helper brings the previous profile back up, restores
setup mode so you can try a different password without holding the button again,
and the page explains what happened:

| What went wrong | What the page says |
| --- | --- |
| Wrong password | "The password was not accepted. Check it and try again." |
| Network not in range | "That network could not be found. Move closer, or check that it is broadcasting." |
| Timed out | "The connection attempt timed out. The network may be out of range." |
| Joined but no DHCP | "Joined the network but no IP address was assigned. Check the router's DHCP settings." |

## Entering setup mode is not a factory reset

It does not touch your alarms, Timeblaster settings, ErsatzTV configuration,
channels, or sound files. It changes exactly one thing: which Wi-Fi network the
Pi is on.

Factory reset is a separate, deliberate procedure — see
[installation.md](installation.md#factory-reset).

## The privilege split

Networking is the one thing on this device that needs root, so it is isolated in
its own process.

```text
┌──────────────────────────┐        ┌───────────────────────────────┐
│ timeblasterd             │        │ timeblaster-wifi              │
│ user: timeblaster        │  unix  │ user: root                    │
│                          │ socket │                               │
│ "please enter setup mode"│───────►│ nmcli connection add/modify/up│
│                          │        │ nmcli device wifi connect     │
│ cannot run any command   │        │ captive portal on port 80     │
│ cannot change the network│        │                               │
└──────────────────────────┘        └───────────────────────────────┘
```

* The main daemon speaks a small RPC with exactly six operations. It cannot run an
  arbitrary command, change a network, or bind a privileged port.
* The socket at `/run/timeblaster/wifi.sock` is mode `0660`, owned `root:timeblaster`.
* Every SSID and passphrase is validated on **both** sides against the 802.11
  limits, and control characters are rejected outright, so nothing can smuggle a
  newline into an `nmcli` argument. These values arrive from a web form on an open
  access point, so this is a security boundary rather than a nicety.
* Passphrases are never logged. The request-logging path replaces them with
  `[redacted]` before anything reaches the journal.

## The captive portal

The setup page is served by the privileged helper on `10.42.0.1:80`, and only
while setup mode is active.

* It answers the probe URLs iOS, Android and Windows use to detect a captive
  portal, which is what makes the page pop up by itself.
* It is a single self-contained document with **no external resources** — a phone
  joined to the setup network has no route to the internet, so a page that pulled
  a font or a script from a CDN would render broken.
* It exposes exactly three operations: list networks, join one, report status.
  There is no way to reach a shell, a file, or any other part of the system.
* DHCP and DNS come from NetworkManager's shared mode (`ipv4.method shared`),
  which runs its own dnsmasq. No second daemon to install or configure.

## Configuration

```toml
[wifi]
interface = "wlan0"
setup_ssid = "TIMEBLASTER-SETUP"

# Empty means an open network — the friendlier default for a device whose whole
# point is being easy to set up, and it is only live while you are standing in
# front of it. Set an 8-63 character passphrase to secure it.
setup_passphrase = ""

setup_address = "10.42.0.1/24"
setup_timeout = "15m0s"
connect_timeout = "45s"
validate_timeout = "30s"

[input]
# How long the button must be held. Five seconds is long enough to be deliberate
# and short enough not to feel broken.
wifi_hold_duration = "5s"
```

## Alternatives to the button

The button is the intended path, but two others exist for when it is impractical.

### From the companion app

**SYSTEM → START WI-FI SETUP**. Useful when the Timeblaster is already on the
network and you want to move it to a different one. It disconnects the page you
are using, so it asks for confirmation first.

### Over SSH

```bash
nmcli device wifi list
sudo nmcli device wifi connect "Your Network" password "your-password"
```

This bypasses Timeblaster entirely and is the fastest route when you already have
a shell.

## Diagnosing

```bash
# What does Timeblaster think?
tbctl wifi
tbctl health

# What does NetworkManager think?
nmcli device status
nmcli connection show
nmcli -t -f GENERAL.STATE,GENERAL.CONNECTION,IP4.ADDRESS device show wlan0

# The helper's own log
journalctl -u timeblaster-wifi.service -f

# Is the helper reachable at all?
ls -l /run/timeblaster/wifi.sock       # srw-rw---- root timeblaster
```

### Common problems

| Symptom | Cause | Fix |
| --- | --- | --- |
| Holding the button does nothing | The Nano is not connected | `tbctl health` → `nano`; check the USB cable |
| `wifi_helper: down` | The helper is not running | `systemctl status timeblaster-wifi.service` |
| TIMEBLASTER-SETUP never appears | The Wi-Fi radio is blocked or busy | `rfkill list`; `nmcli radio wifi on` |
| The setup page does not open | Captive-portal detection was skipped | Browse to `http://10.42.0.1` directly |
| The connection always fails | Wrong password, or 5 GHz-only network | The setup AP uses 2.4 GHz; the client radio supports both |
| `timeblaster.local` does not resolve | mDNS is unavailable on your network | Use the IP address; `nmcli device show wlan0` prints it |
