#!/usr/bin/env bash
#
# Timeblaster installer for Raspberry Pi OS Lite (Debian 13 "trixie", arm64).
#
#   sudo ./setup.sh
#
# The script is idempotent and non-destructive. Every step detects work that is
# already done and skips it, user-modified files are never overwritten, and a
# reboot is reported rather than performed. Rerunning it re-verifies the
# installation and reprints the status summary.
#
# Useful flags:
#   --skip-ersatztv     do not install or update ErsatzTV
#   --keep-console      leave the Debian login prompt on the television
#   --skip-packages     do not touch apt (for a rerun on a known-good system)
#   --skip-build        do not rebuild the Go binaries
#   --force-config      overwrite /etc/timeblaster/timeblaster.toml (backs it up)
#   --uninstall         stop and disable the services, leaving data in place
#   --dry-run           print what would happen, change nothing

set -o errexit
set -o nounset
set -o pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SERVICE_USER="timeblaster"
readonly CONFIG_DIR="/etc/timeblaster"
readonly CONFIG_FILE="${CONFIG_DIR}/timeblaster.toml"
readonly STATE_DIR="/var/lib/timeblaster"
readonly SOUNDS_DIR="${STATE_DIR}/alarm-sounds"
readonly SHARE_DIR="/usr/share/timeblaster"
readonly ASSETS_DIR="${SHARE_DIR}/assets"
readonly DOC_DIR="/usr/share/doc/timeblaster"
readonly BIN_DIR="/usr/local/bin"
readonly SYSTEMD_DIR="/etc/systemd/system"

readonly ERSATZTV_DIR="/opt/ersatztv"
readonly ERSATZTV_USER="ersatztv"
readonly ERSATZTV_STATE="/var/lib/ersatztv"
# ErsatzTV builds its own FFmpeg and checks the version at runtime; Debian's is
# older than it wants. Kept out of /usr/bin so nothing else on the system is
# shadowed by it.
readonly FFMPEG_DIR="/opt/ersatztv-ffmpeg"

# Minimum Go version able to build this module.
readonly GO_MIN_MAJOR=1
readonly GO_MIN_MINOR=25

KEEP_CONSOLE=0
# The VT the television shows, and the one the rescue login moves to.
CONSOLE_VT=1
RESCUE_VT=2
# Kernel messages are sent here: a VT the television never shows.
KERNEL_VT=3

SKIP_ERSATZTV=0
SKIP_PACKAGES=0
SKIP_BUILD=0
FORCE_CONFIG=0
UNINSTALL=0
DRY_RUN=0

# Resolved by ensure_go before the build.
GO_BIN=""

REBOOT_REQUIRED=0
declare -a WARNINGS=()
declare -a NOTES=()

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'
  C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'; C_BLUE=$'\033[36m'
else
  C_RESET=''; C_BOLD=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_BLUE=''
fi

step()  { printf '\n%s==>%s %s%s%s\n' "$C_BLUE" "$C_RESET" "$C_BOLD" "$*" "$C_RESET"; }
info()  { printf '    %s\n' "$*"; }
ok()    { printf '    %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
skip()  { printf '    %s·%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
warn()  { printf '    %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; WARNINGS+=("$*"); }
note()  { NOTES+=("$*"); }
die()   { printf '\n%sError:%s %s\n\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

# run executes a command, honouring --dry-run.
run() {
  if (( DRY_RUN )); then
    printf '    %s[dry-run]%s %s\n' "$C_YELLOW" "$C_RESET" "$*"
    return 0
  fi
  "$@"
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --skip-ersatztv) SKIP_ERSATZTV=1 ;;
      --keep-console)  KEEP_CONSOLE=1 ;;
      --skip-packages) SKIP_PACKAGES=1 ;;
      --skip-build)    SKIP_BUILD=1 ;;
      --force-config)  FORCE_CONFIG=1 ;;
      --uninstall)     UNINSTALL=1 ;;
      --dry-run)       DRY_RUN=1 ;;
      -h|--help)
        # Print the leading comment block, which is the usage text.
        awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "${BASH_SOURCE[0]}"
        exit 0
        ;;
      *)               die "unknown option: $1 (try --help)" ;;
    esac
    shift
  done
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------

preflight() {
  step "Checking the environment"

  if [[ $EUID -ne 0 ]]; then
    # --dry-run is allowed unprivileged so the plan can be reviewed before
    # handing the machine to a script.
    (( DRY_RUN )) || die "this script must run as root: sudo ./setup.sh"
    warn "not running as root; --dry-run output may be incomplete"
  fi

  [[ -r /etc/os-release ]] || die "/etc/os-release is missing; this does not look like a Debian system"
  # shellcheck disable=SC1091
  . /etc/os-release

  case "${ID:-}${ID_LIKE:-}" in
    *debian*|*raspbian*) ok "OS: ${PRETTY_NAME:-unknown}" ;;
    *) die "unsupported distribution: ${PRETTY_NAME:-unknown}. Timeblaster targets Raspberry Pi OS / Debian." ;;
  esac

  local arch
  arch="$(dpkg --print-architecture 2>/dev/null || uname -m)"
  case "$arch" in
    arm64|aarch64) ok "Architecture: ${arch}" ;;
    armhf|armv7l)
      warn "32-bit ARM detected (${arch}). Timeblaster targets arm64; ErsatzTV and mpv"
      warn "  will be slower and hardware decoding may be limited. Continuing."
      ;;
    amd64|x86_64)
      warn "x86-64 detected. This is fine for development, but the Raspberry Pi"
      warn "  specifics (DRM/KMS output, the Nano's udev rule) will not apply."
      ;;
    *) warn "unrecognised architecture ${arch}; continuing anyway" ;;
  esac

  if [[ -r /proc/device-tree/model ]]; then
    local model
    model="$(tr -d '\0' < /proc/device-tree/model)"
    ok "Hardware: ${model}"
    case "$model" in
      *"Raspberry Pi 5"*) ;;
      *"Raspberry Pi"*)   warn "Timeblaster targets the Raspberry Pi 5; ${model} may struggle with video." ;;
    esac
  fi

  # Free space: the Go toolchain and ErsatzTV together want a couple of gigabytes.
  local free_mb
  free_mb="$(df -Pm / | awk 'NR==2 {print $4}')"
  if (( free_mb < 2048 )); then
    warn "only ${free_mb} MB free on /; 2 GB or more is recommended"
  else
    ok "Free space: ${free_mb} MB"
  fi

  if [[ ! -d "${SCRIPT_DIR}/cmd/timeblasterd" ]]; then
    die "run this script from the Timeblaster source directory (cmd/timeblasterd not found)"
  fi
  ok "Source: ${SCRIPT_DIR}"

  if ! ping -c1 -W2 deb.debian.org >/dev/null 2>&1 && ! ping -c1 -W2 1.1.1.1 >/dev/null 2>&1; then
    warn "no network connectivity detected; package installation and the ErsatzTV"
    warn "  download will fail. Rerun once the Pi is online."
  fi
}

# ---------------------------------------------------------------------------
# Packages
# ---------------------------------------------------------------------------

PACKAGES=(
  mpv                 # television playback and alarm audio
  alsa-utils          # amixer, aplay: alarm speaker volume and diagnostics
  avahi-daemon        # publishes timeblaster.local
  libnss-mdns         # lets the Pi itself resolve .local names
  network-manager     # the networking stack on trixie; nmcli drives Wi-Fi setup
  dnsmasq-base        # NetworkManager's shared-mode DHCP/DNS for the setup AP
  sqlite3             # for inspecting the database by hand
  ca-certificates
  curl
  tar
  libicu-dev          # ErsatzTV's .NET runtime needs ICU
)

install_packages() {
  step "Installing system packages"

  if (( SKIP_PACKAGES )); then
    skip "skipped (--skip-packages)"
    return
  fi

  # Only refresh metadata when it is stale, so a rerun is quick.
  local stamp=/var/lib/apt/periodic/update-success-stamp
  local age=999999
  [[ -f "$stamp" ]] && age=$(( ($(date +%s) - $(stat -c %Y "$stamp")) / 60 ))
  if (( age > 60 )); then
    info "refreshing package metadata"
    run env DEBIAN_FRONTEND=noninteractive apt-get update -qq
  else
    skip "package metadata is ${age} minutes old; not refreshing"
  fi

  local missing=()
  for pkg in "${PACKAGES[@]}"; do
    if dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q "ok installed"; then
      continue
    fi
    missing+=("$pkg")
  done

  if (( ${#missing[@]} == 0 )); then
    skip "all ${#PACKAGES[@]} packages are already installed"
  else
    info "installing: ${missing[*]}"
    run env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}"
    ok "installed ${#missing[@]} package(s)"
  fi
}

# ensure_go finds a Go toolchain new enough to build the module, installing the
# official release if the packaged one is too old. Debian's golang lags, and a
# five-minute download beats a confusing build failure.
ensure_go() {
  local go_bin=""
  for candidate in /usr/local/go/bin/go "$(command -v go 2>/dev/null || true)"; do
    [[ -n "$candidate" && -x "$candidate" ]] || continue
    local ver major minor
    ver="$("$candidate" version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
    major="${ver%%.*}"; minor="${ver#*.}"; minor="${minor%%.*}"
    if [[ -n "$major" && -n "$minor" ]] &&
       { (( major > GO_MIN_MAJOR )) || { (( major == GO_MIN_MAJOR )) && (( minor >= GO_MIN_MINOR )); }; }; then
      go_bin="$candidate"
      ok "Go toolchain: ${ver} (${candidate})"
      break
    fi
  done

  if [[ -n "$go_bin" ]]; then
    GO_BIN="$go_bin"
    return
  fi

  local want="1.25.1"
  local arch
  case "$(dpkg --print-architecture)" in
    arm64) arch=arm64 ;;
    armhf) arch=armv6l ;;
    amd64) arch=amd64 ;;
    *)     die "no Go toolchain available for $(dpkg --print-architecture); install Go ${GO_MIN_MAJOR}.${GO_MIN_MINOR}+ manually" ;;
  esac

  info "installing Go ${want} for ${arch} (the packaged toolchain is too old)"
  local url="https://go.dev/dl/go${want}.linux-${arch}.tar.gz"
  local tmp
  tmp="$(mktemp -d)"
  if ! run curl -fsSL -o "${tmp}/go.tar.gz" "$url"; then
    rm -rf "$tmp"
    die "could not download ${url}. Install Go ${GO_MIN_MAJOR}.${GO_MIN_MINOR}+ manually and rerun with --skip-packages."
  fi
  run rm -rf /usr/local/go
  run tar -C /usr/local -xzf "${tmp}/go.tar.gz"
  rm -rf "$tmp"
  GO_BIN=/usr/local/go/bin/go
  ok "Go ${want} installed to /usr/local/go"
  note "Add Go to your PATH for interactive use: export PATH=\$PATH:/usr/local/go/bin"
}

# ---------------------------------------------------------------------------
# User, groups and directories
# ---------------------------------------------------------------------------

create_user() {
  step "Creating the service account"

  if id "$SERVICE_USER" >/dev/null 2>&1; then
    skip "user ${SERVICE_USER} already exists"
  else
    run useradd --system --no-create-home --home-dir "$STATE_DIR" \
      --shell /usr/sbin/nologin --comment "Timeblaster service account" "$SERVICE_USER"
    ok "created system user ${SERVICE_USER}"
  fi

  # dialout: the Nano's serial port. audio: the USB speaker.
  # video and render: DRM/KMS, so mpv can drive HDMI with no desktop session.
  local groups=(dialout audio video render)
  for g in "${groups[@]}"; do
    if ! getent group "$g" >/dev/null 2>&1; then
      warn "group ${g} does not exist on this system; skipping"
      continue
    fi
    if id -nG "$SERVICE_USER" 2>/dev/null | tr ' ' '\n' | grep -qx "$g"; then
      continue
    fi
    run usermod -aG "$g" "$SERVICE_USER"
    ok "added ${SERVICE_USER} to group ${g}"
  done
  info "groups: $(id -nG "$SERVICE_USER" 2>/dev/null || echo '(dry run)')"
}

create_directories() {
  step "Creating directories"

  local -a dirs=("$CONFIG_DIR" "$STATE_DIR" "$SOUNDS_DIR" "$SHARE_DIR" "$ASSETS_DIR" "$DOC_DIR")
  for d in "${dirs[@]}"; do
    if [[ -d "$d" ]]; then
      skip "${d} exists"
    else
      run install -d -m 0755 "$d"
      ok "created ${d}"
    fi
  done

  # State is private to the service account; configuration is world-readable so
  # an operator can check it without sudo, but only root may edit it.
  run chown -R "${SERVICE_USER}:${SERVICE_USER}" "$STATE_DIR"
  run chmod 0750 "$STATE_DIR"
  run chmod 0755 "$SOUNDS_DIR"
  run chown root:root "$CONFIG_DIR"
  run chmod 0755 "$CONFIG_DIR"
}

# ---------------------------------------------------------------------------
# Build and install
# ---------------------------------------------------------------------------

build_binaries() {
  step "Building Timeblaster"

  if (( SKIP_BUILD )); then
    skip "skipped (--skip-build)"
    return
  fi

  # Here rather than with the package install: --skip-packages means "do not
  # touch apt", and finding a Go toolchain is not an apt operation. Doing it
  # there meant --skip-packages left GO_BIN unset and the build died on it.
  [[ -n "$GO_BIN" ]] || ensure_go

  local version
  version="$(cd "$SCRIPT_DIR" && git describe --tags --always --dirty 2>/dev/null || echo dev)"
  info "version: ${version}"

  # Build as the invoking user where possible so the module cache does not end up
  # root-owned in their home directory.
  local build_env=(env "GOCACHE=/tmp/timeblaster-gocache" "GOPATH=/tmp/timeblaster-gopath" "CGO_ENABLED=0")

  for cmd in timeblasterd timeblaster-wifi tbctl; do
    info "building ${cmd}"
    run "${build_env[@]}" "$GO_BIN" build \
      -C "$SCRIPT_DIR" \
      -trimpath \
      -ldflags "-s -w -X main.version=${version}" \
      -o "/tmp/timeblaster-build-${cmd}" \
      "./cmd/${cmd}"
  done
  ok "built 3 binaries"

  for cmd in timeblasterd timeblaster-wifi tbctl; do
    run install -m 0755 -o root -g root "/tmp/timeblaster-build-${cmd}" "${BIN_DIR}/${cmd}"
    run rm -f "/tmp/timeblaster-build-${cmd}"
  done
  ok "installed to ${BIN_DIR}"
}

install_config() {
  step "Installing configuration"

  local src="${SCRIPT_DIR}/deploy/config/timeblaster.toml"
  [[ -f "$src" ]] || die "missing ${src}"

  if [[ ! -f "$CONFIG_FILE" ]]; then
    run install -m 0644 -o root -g root "$src" "$CONFIG_FILE"
    ok "installed ${CONFIG_FILE}"
    return
  fi

  if (( FORCE_CONFIG )); then
    local backup="${CONFIG_FILE}.$(date +%Y%m%d%H%M%S).bak"
    run cp -p "$CONFIG_FILE" "$backup"
    run install -m 0644 -o root -g root "$src" "$CONFIG_FILE"
    ok "replaced ${CONFIG_FILE} (previous copy saved as ${backup})"
    return
  fi

  # Never clobber the user's edits. Write the new template alongside and report
  # the difference so they can merge it deliberately.
  if cmp -s "$src" "$CONFIG_FILE"; then
    skip "${CONFIG_FILE} is already up to date"
    return
  fi

  run install -m 0644 -o root -g root "$src" "${CONFIG_FILE}.new"
  warn "${CONFIG_FILE} exists and differs from the shipped template."
  warn "  Your copy was left untouched. The new template is at ${CONFIG_FILE}.new"
  warn "  Compare with:  diff -u ${CONFIG_FILE} ${CONFIG_FILE}.new"
  note "Review ${CONFIG_FILE}.new for new configuration options."
}

install_assets() {
  step "Installing assets"

  local src="${SCRIPT_DIR}/deploy/assets/no-channel.png"
  if [[ ! -f "$src" || ! -f "${SCRIPT_DIR}/deploy/assets/booting.png" ]]; then
    info "generating the standby image"
    if command -v python3 >/dev/null 2>&1; then
      run python3 "${SCRIPT_DIR}/scripts/make-assets.py" \
        --deploy-assets "${SCRIPT_DIR}/deploy/assets" \
        --web-assets "${SCRIPT_DIR}/internal/web/static/assets"
    else
      warn "python3 is not available and ${src} is missing; the television will"
      warn "  show a blank screen instead of the standby image"
    fi
  fi

  for asset in no-channel booting; do
    local file="${SCRIPT_DIR}/deploy/assets/${asset}.png"
    if [[ -f "$file" ]]; then
      run install -m 0644 -o root -g root "$file" "${ASSETS_DIR}/${asset}.png"
      ok "installed ${ASSETS_DIR}/${asset}.png"
    fi
  done

  # Alarm sounds. Any MP3 shipped with the source is copied in, but an existing
  # file is never overwritten: the user may have replaced it deliberately.
  local installed=0
  shopt -s nullglob
  for f in "${SCRIPT_DIR}"/deploy/sounds/*.mp3; do
    local base
    base="$(basename "$f")"
    if [[ -f "${SOUNDS_DIR}/${base}" ]]; then
      skip "${base} already present"
      continue
    fi
    run install -m 0644 -o "$SERVICE_USER" -g "$SERVICE_USER" "$f" "${SOUNDS_DIR}/${base}"
    installed=$((installed + 1))
  done
  shopt -u nullglob
  (( installed > 0 )) && ok "installed ${installed} alarm sound(s)"

  local count
  count="$(find "$SOUNDS_DIR" -maxdepth 1 -type f -iname '*.mp3' 2>/dev/null | wc -l | tr -d ' ')"
  if [[ "$count" == "0" ]]; then
    warn "no alarm sounds in ${SOUNDS_DIR}."
    warn "  Timeblaster will fall back to a built-in generated tone, which works but"
    warn "  is not pleasant. Copy MP3s in, for example:"
    warn "    sudo cp alarm1.mp3 alarm2.mp3 ${SOUNDS_DIR}/"
    warn "    sudo chown ${SERVICE_USER}:${SERVICE_USER} ${SOUNDS_DIR}/*.mp3"
  else
    ok "${count} alarm sound(s) available"
  fi

  # Documentation, so `systemctl cat` and the unit files' Documentation= lines
  # point somewhere real on the device.
  if [[ -d "${SCRIPT_DIR}/docs" ]]; then
    run cp -r "${SCRIPT_DIR}/docs/." "$DOC_DIR/"
    run chmod -R a+r "$DOC_DIR"
    ok "installed documentation to ${DOC_DIR}"
  fi
}

install_udev() {
  step "Installing the udev rule for the Nano"

  local src="${SCRIPT_DIR}/deploy/udev/99-timeblaster.rules"
  local dst="/etc/udev/rules.d/99-timeblaster.rules"

  if [[ -f "$dst" ]] && cmp -s "$src" "$dst"; then
    skip "udev rule is already current"
    return
  fi
  run install -m 0644 -o root -g root "$src" "$dst"
  run udevadm control --reload-rules
  run udevadm trigger --subsystem-match=tty
  ok "installed ${dst} (creates /dev/timeblaster-nano)"
}

configure_hostname() {
  step "Configuring the hostname and mDNS"

  local want
  want="$(grep -E '^\s*hostname\s*=' "$CONFIG_FILE" 2>/dev/null | head -1 | sed -E 's/.*=\s*"([^"]*)".*/\1/')"
  want="${want:-timeblaster}"

  local current
  current="$(hostname)"
  if [[ "$current" == "$want" ]]; then
    skip "hostname is already ${want}"
  else
    run hostnamectl set-hostname "$want"
    # /etc/hosts must agree, or sudo becomes slow and some tools complain.
    if grep -qE "^127\.0\.1\.1" /etc/hosts; then
      run sed -i -E "s/^127\.0\.1\.1.*/127.0.1.1\t${want}/" /etc/hosts
    else
      run bash -c "printf '127.0.1.1\t%s\n' '${want}' >> /etc/hosts"
    fi
    ok "hostname set to ${want} (was ${current})"
    note "The hostname change takes full effect after a reboot."
    REBOOT_REQUIRED=1
  fi

  # Advertise the companion app itself, on whatever port it is configured for.
  local port
  port="$(grep -E '^\s*listen_address\s*=' "$CONFIG_FILE" 2>/dev/null | head -1 | sed -E 's/.*:([0-9]+)".*/\1/')"
  port="${port:-8080}"

  local avahi_src="${SCRIPT_DIR}/deploy/avahi/timeblaster.service"
  local avahi_dst="/etc/avahi/services/timeblaster.service"
  if [[ -f "$avahi_src" ]]; then
    run install -d -m 0755 /etc/avahi/services
    if (( DRY_RUN )); then
      info "[dry-run] would install ${avahi_dst} advertising port ${port}"
    else
      sed -E "s|<port>[0-9]+</port>|<port>${port}</port>|" "$avahi_src" > "$avahi_dst"
      chmod 0644 "$avahi_dst"
    fi
    ok "advertising _http._tcp on port ${port} via mDNS"
  fi

  if systemctl is-enabled avahi-daemon >/dev/null 2>&1; then
    skip "avahi-daemon is already enabled"
  else
    run systemctl enable --now avahi-daemon
    ok "enabled avahi-daemon"
  fi
  run systemctl reload-or-restart avahi-daemon
}

configure_console() {
  step "Making the television look like a product, not a Linux box"

  # Three separate things show on HDMI before mpv gets the display, and each
  # needs its own fix:
  #
  #   1. kernel messages scrolling past        -> quiet, loglevel, logo.nologo
  #   2. systemd's [ OK ] lines                -> systemd.show_status=false
  #   3. the Debian login prompt               -> disable the getty on that VT
  #
  # The third is the one that actually matters: without it the television shows
  # a login prompt forever if the daemon ever fails to start.

  local cmdline=""
  for candidate in /boot/firmware/cmdline.txt /boot/cmdline.txt; do
    [[ -f "$candidate" ]] && { cmdline="$candidate"; break; }
  done

  if [[ -z "$cmdline" ]]; then
    skip "no cmdline.txt found; not a Raspberry Pi boot layout"
  else
    # console=tty3 moves kernel output to a VT the television never shows, so
    # anything that does slip past `quiet` lands out of sight.
    local -a opts=(
      "quiet"
      "loglevel=3"
      "logo.nologo"
      "vt.global_cursor_default=0"
      "consoleblank=0"
      "systemd.show_status=false"
    )

    local added=0

    # console= is special: the image ships with console=tty1, which is the VT the
    # television shows. It has to be rewritten rather than skipped as
    # "already present", and the serial console alongside it must be left alone.
    if grep -qE '(^| )console=tty1( |$)' "$cmdline"; then
      if (( DRY_RUN )); then
        info "[dry-run] would rewrite console=tty1 to console=tty${KERNEL_VT} in ${cmdline}"
      else
        sed -i "s/\(^\| \)console=tty1\( \|\$\)/\1console=tty${KERNEL_VT}\2/" "$cmdline"
      fi
      added=$((added + 1))
    elif ! grep -qE '(^| )console=tty[0-9]' "$cmdline"; then
      if (( DRY_RUN )); then
        info "[dry-run] would append console=tty${KERNEL_VT} to ${cmdline}"
      else
        sed -i "1s|\$| console=tty${KERNEL_VT}|" "$cmdline"
      fi
      added=$((added + 1))
    fi

    for opt in "${opts[@]}"; do
      local key="${opt%%=*}"
      if [[ "$opt" == *=* ]] && grep -qE "(^| )${key}=" "$cmdline"; then
        continue
      fi
      if [[ "$opt" != *=* ]] && grep -qw -- "$opt" "$cmdline"; then
        continue
      fi
      if (( DRY_RUN )); then
        info "[dry-run] would append ${opt} to ${cmdline}"
      else
        # cmdline.txt must remain a single line.
        sed -i "1s|\$| ${opt}|" "$cmdline"
      fi
      added=$((added + 1))
    done

    if (( added > 0 )); then
      ok "added ${added} kernel option(s) to ${cmdline}"
      note "The quiet-boot changes take effect after a reboot."
      REBOOT_REQUIRED=1
    else
      skip "${cmdline} already has the quiet-boot options"
    fi
  fi

  configure_getty
  install_splash
}

# configure_getty takes the login prompt off the television.
#
# The getty on tty1 is disabled and one on tty2 is enabled in its place, so the
# screen stays clean but a keyboard is still a way in: Ctrl+Alt+F2 reaches a
# login. SSH and the serial console are untouched.
configure_getty() {
  if (( KEEP_CONSOLE )); then
    skip "leaving the console login in place (--keep-console)"
    return
  fi

  if systemctl is-enabled "getty@tty${CONSOLE_VT}.service" >/dev/null 2>&1; then
    run systemctl disable "getty@tty${CONSOLE_VT}.service"
    ok "disabled the login prompt on tty${CONSOLE_VT} (the television's VT)"
    REBOOT_REQUIRED=1
  else
    skip "no login prompt enabled on tty${CONSOLE_VT}"
  fi
  # Stopping it as well means the change is visible without waiting for a boot.
  if systemctl is-active --quiet "getty@tty${CONSOLE_VT}.service" 2>/dev/null; then
    run systemctl stop "getty@tty${CONSOLE_VT}.service"
  fi

  # The escape hatch. Without this, a Pi with no network and no serial console
  # would have no way in at all.
  if systemctl is-enabled "getty@tty${RESCUE_VT}.service" >/dev/null 2>&1; then
    skip "rescue login already available on tty${RESCUE_VT}"
  else
    run systemctl enable "getty@tty${RESCUE_VT}.service"
    ok "enabled a rescue login on tty${RESCUE_VT} (reach it with Ctrl+Alt+F${RESCUE_VT})"
  fi
  note "Local login moved to Ctrl+Alt+F${RESCUE_VT}; SSH is unaffected."
}

# install_splash installs the boot screen and its shared drawing module.
install_splash() {
  local lib_dir="/usr/local/lib/timeblaster"

  run install -d -m 0755 "$lib_dir"
  run install -m 0644 -o root -g root "${SCRIPT_DIR}/scripts/tbdisplay.py" "${lib_dir}/tbdisplay.py"
  run install -m 0755 -o root -g root "${SCRIPT_DIR}/scripts/timeblaster-splash" "${BIN_DIR}/timeblaster-splash"
  ok "installed the boot screen to ${BIN_DIR}/timeblaster-splash"

  local src="${SCRIPT_DIR}/deploy/systemd/timeblaster-splash.service"
  local dst="${SYSTEMD_DIR}/timeblaster-splash.service"
  if [[ -f "$dst" ]] && cmp -s "$src" "$dst"; then
    skip "timeblaster-splash.service is already current"
  else
    run install -m 0644 -o root -g root "$src" "$dst"
    run systemctl daemon-reload
    ok "installed timeblaster-splash.service"
  fi

  if systemctl is-enabled timeblaster-splash.service >/dev/null 2>&1; then
    skip "timeblaster-splash.service is already enabled"
  else
    run systemctl enable timeblaster-splash.service
    ok "enabled the boot screen"
  fi

  # Report whether it can actually paint, since a missing framebuffer is the
  # one thing that silently turns this into a black screen.
  if (( DRY_RUN )); then
    return
  fi
  if [[ -e /dev/fb0 ]]; then
    local geom
    geom="$("${BIN_DIR}/timeblaster-splash" --check 2>/dev/null | head -1 || true)"
    ok "boot screen target: ${geom:-/dev/fb0}"
  else
    warn "no /dev/fb0, so the boot screen cannot be painted. The television will"
    warn "  simply stay dark until mpv starts, which is harmless. On Raspberry Pi"
    warn "  OS this usually means fbdev emulation is off in the KMS overlay."
  fi
}

# ---------------------------------------------------------------------------
# ErsatzTV
# ---------------------------------------------------------------------------

install_ersatztv() {
  step "Installing ErsatzTV"

  if (( SKIP_ERSATZTV )); then
    skip "skipped (--skip-ersatztv)"
    return
  fi

  local arch asset_arch
  arch="$(dpkg --print-architecture)"
  case "$arch" in
    arm64) asset_arch="linux-arm64" ;;
    armhf) asset_arch="linux-arm" ;;
    amd64) asset_arch="linux-x64" ;;
    *) warn "no ErsatzTV build for ${arch}; skipping"; return ;;
  esac

  # The release tag and asset name are read from GitHub rather than pinned, so a
  # fresh install gets a current build. jq is not assumed to be present.
  local api="https://api.github.com/repos/ErsatzTV/ErsatzTV/releases/latest"
  local tag url
  tag="$(curl -fsSL "$api" 2>/dev/null | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name":\s*"([^"]+)".*/\1/' || true)"

  if [[ -z "$tag" ]]; then
    warn "could not reach the ErsatzTV release API. Skipping ErsatzTV."
    warn "  Install it later by rerunning this script, or manually from"
    warn "  https://github.com/ErsatzTV/ErsatzTV/releases"
    return
  fi

  local installed_version=""
  [[ -f "${ERSATZTV_DIR}/.version" ]] && installed_version="$(cat "${ERSATZTV_DIR}/.version")"
  # The binary has to be there too, not just the stamp. A version file alone
  # once let a broken install -- the archive extracted one level too deep --
  # survive every rerun, because the stamp said the work was already done.
  if [[ "$installed_version" == "$tag" && -x "${ERSATZTV_DIR}/ErsatzTV" ]]; then
    skip "ErsatzTV ${tag} is already installed"
    ensure_ersatztv_service
    return
  fi
  if [[ "$installed_version" == "$tag" ]]; then
    info "ErsatzTV ${tag} is stamped but its binary is missing; reinstalling"
  fi

  url="$(curl -fsSL "$api" 2>/dev/null | grep '"browser_download_url"' \
        | grep -- "-${asset_arch}.tar.gz" | head -1 \
        | sed -E 's/.*"browser_download_url":\s*"([^"]+)".*/\1/' || true)"
  if [[ -z "$url" ]]; then
    warn "no ${asset_arch} asset in ErsatzTV release ${tag}; skipping"
    return
  fi

  info "downloading ErsatzTV ${tag} (${asset_arch})"
  local tmp
  tmp="$(mktemp -d)"
  if ! run curl -fsSL --retry 3 -o "${tmp}/ersatztv.tar.gz" "$url"; then
    rm -rf "$tmp"
    warn "the ErsatzTV download failed; skipping. Timeblaster works without it."
    return
  fi

  if ! id "$ERSATZTV_USER" >/dev/null 2>&1; then
    run useradd --system --no-create-home --home-dir "$ERSATZTV_STATE" \
      --shell /usr/sbin/nologin --comment "ErsatzTV service account" "$ERSATZTV_USER"
    ok "created system user ${ERSATZTV_USER}"
  fi

  # Stop the service before replacing its files, and leave the data directory
  # alone: it holds the user's channels and schedules.
  if systemctl is-active --quiet ersatztv.service 2>/dev/null; then
    run systemctl stop ersatztv.service
  fi

  run install -d -m 0755 "$ERSATZTV_DIR"
  run install -d -m 0750 -o "$ERSATZTV_USER" -g "$ERSATZTV_USER" "$ERSATZTV_STATE"
  run install -d -m 0750 -o "$ERSATZTV_USER" -g "$ERSATZTV_USER" "${ERSATZTV_STATE}/transcode"

  # The release tarball wraps everything in a versioned directory
  # (ErsatzTV-Legacy-vX.Y.Z-linux-arm64/), so extracting it straight into
  # /opt/ersatztv leaves the binary one level too deep and the unit fails with
  # status 203/EXEC. Extract to a staging directory, find where the binary
  # actually landed, and flatten from there -- which also copes if a future
  # release drops the wrapper.
  run tar -xzf "${tmp}/ersatztv.tar.gz" -C "$tmp"

  if (( DRY_RUN )); then
    info "[dry-run] would flatten the release into ${ERSATZTV_DIR}"
  else
    local payload=""
    if [[ -f "${tmp}/ErsatzTV" ]]; then
      payload="$tmp"
    else
      payload="$(find "$tmp" -mindepth 2 -maxdepth 3 -name ErsatzTV -type f -print -quit 2>/dev/null)"
      payload="${payload%/ErsatzTV}"
    fi

    if [[ -z "$payload" || ! -f "${payload}/ErsatzTV" ]]; then
      rm -rf "$tmp"
      warn "the ErsatzTV archive did not contain an ErsatzTV binary; skipping."
      warn "  Timeblaster works without it; the television shows the standby screen."
      return
    fi

    # Replace the contents rather than the directory, so a previous install's
    # stray files do not linger and shadow the new ones.
    find "$ERSATZTV_DIR" -mindepth 1 -maxdepth 1 ! -name '.version' -exec rm -rf {} +
    cp -a "${payload}/." "${ERSATZTV_DIR}/"
  fi
  rm -rf "$tmp"

  run chown -R root:root "$ERSATZTV_DIR"
  run chmod -R a+rX "$ERSATZTV_DIR"
  if [[ -f "${ERSATZTV_DIR}/ErsatzTV" ]]; then
    run chmod 0755 "${ERSATZTV_DIR}/ErsatzTV"
    [[ -f "${ERSATZTV_DIR}/ErsatzTV.Scanner" ]] && run chmod 0755 "${ERSATZTV_DIR}/ErsatzTV.Scanner"
  fi
  (( DRY_RUN )) || printf '%s\n' "$tag" > "${ERSATZTV_DIR}/.version"
  ok "installed ErsatzTV ${tag} to ${ERSATZTV_DIR}"

  ensure_ersatztv_service
}

ensure_ersatztv_service() {
  local src="${SCRIPT_DIR}/deploy/systemd/ersatztv.service"
  local dst="${SYSTEMD_DIR}/ersatztv.service"
  if [[ -f "$dst" ]] && cmp -s "$src" "$dst"; then
    skip "ersatztv.service is already current"
  else
    run install -m 0644 -o root -g root "$src" "$dst"
    ok "installed ersatztv.service"
  fi
}

# install_ffmpeg installs the FFmpeg build ErsatzTV expects.
#
# ErsatzTV's own health page rejects the FFmpeg that ships with Debian --
# "version 7.1.5 is too old; please install 8.1.2" -- because it relies on
# features and fixes in its own build. Rather than shadowing the system FFmpeg,
# this goes in its own directory and ersatztv.service puts it first on PATH, so
# nothing else on the machine is affected.
install_ffmpeg() {
  step "Installing FFmpeg for ErsatzTV"

  if (( SKIP_ERSATZTV )); then
    skip "skipped (--skip-ersatztv)"
    return
  fi

  # Fetching the build and telling ErsatzTV about it are separate concerns, and
  # the second must happen even when the first has nothing to do. Folding them
  # into one function meant "already installed" returned early and silently
  # skipped the configuration -- which is why a correctly installed FFmpeg sat
  # there unused.
  fetch_ffmpeg
  point_ersatztv_at_ffmpeg
}

fetch_ffmpeg() {
  local arch asset_arch
  arch="$(dpkg --print-architecture)"
  case "$arch" in
    arm64) asset_arch="linuxarm64" ;;
    amd64) asset_arch="linux64" ;;
    *)
      warn "no ErsatzTV FFmpeg build for ${arch}; leaving the system FFmpeg in place."
      warn "  ErsatzTV may report it as too old."
      return
      ;;
  esac

  local api="https://api.github.com/repos/ErsatzTV/ErsatzTV-ffmpeg/releases/latest"
  local tag
  tag="$(curl -fsSL "$api" 2>/dev/null | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name":\s*"([^"]+)".*/\1/' || true)"
  if [[ -z "$tag" ]]; then
    warn "could not reach the ErsatzTV FFmpeg release API; leaving the system FFmpeg."
    return
  fi

  local installed=""
  [[ -f "${FFMPEG_DIR}/.version" ]] && installed="$(cat "${FFMPEG_DIR}/.version")"
  if [[ "$installed" == "$tag" && -x "${FFMPEG_DIR}/bin/ffmpeg" ]]; then
    skip "ErsatzTV FFmpeg ${tag} is already installed"
    return
  fi

  local url
  url="$(curl -fsSL "$api" 2>/dev/null | grep '"browser_download_url"' \
        | grep -- "-${asset_arch}-" | head -1 \
        | sed -E 's/.*"browser_download_url":\s*"([^"]+)".*/\1/' || true)"
  if [[ -z "$url" ]]; then
    warn "no ${asset_arch} asset in ErsatzTV FFmpeg release ${tag}; skipping"
    return
  fi

  info "downloading FFmpeg ${tag} (${asset_arch}); this is around 100 MB"
  local tmp
  tmp="$(mktemp -d)"
  if ! run curl -fsSL --retry 3 -o "${tmp}/ffmpeg.tar.xz" "$url"; then
    rm -rf "$tmp"
    warn "the FFmpeg download failed; leaving the system FFmpeg in place."
    return
  fi

  if (( DRY_RUN )); then
    info "[dry-run] would install FFmpeg ${tag} to ${FFMPEG_DIR}"
    rm -rf "$tmp"
    return
  fi

  # Same versioned-wrapper layout as the ErsatzTV archive, with the binaries
  # under bin/, so find them rather than assuming a path.
  run tar -xJf "${tmp}/ffmpeg.tar.xz" -C "$tmp"
  local payload
  payload="$(find "$tmp" -mindepth 2 -maxdepth 4 -type f -name ffmpeg -perm -u+x -print -quit 2>/dev/null)"
  payload="${payload%/bin/ffmpeg}"

  if [[ -z "$payload" || ! -x "${payload}/bin/ffmpeg" ]]; then
    rm -rf "$tmp"
    warn "the FFmpeg archive did not contain the expected bin/ffmpeg; skipping"
    return
  fi

  run install -d -m 0755 "${FFMPEG_DIR}/bin"
  run install -m 0755 -o root -g root "${payload}/bin/ffmpeg" "${FFMPEG_DIR}/bin/ffmpeg"
  run install -m 0755 -o root -g root "${payload}/bin/ffprobe" "${FFMPEG_DIR}/bin/ffprobe"
  [[ -f "${payload}/LICENSE.txt" ]] && \
    run install -m 0644 -o root -g root "${payload}/LICENSE.txt" "${FFMPEG_DIR}/LICENSE.txt"
  rm -rf "$tmp"

  printf '%s\n' "$tag" > "${FFMPEG_DIR}/.version"

  local got
  got="$("${FFMPEG_DIR}/bin/ffmpeg" -version 2>/dev/null | head -1 | awk '{print $3}')"
  ok "installed FFmpeg ${got:-$tag} to ${FFMPEG_DIR}/bin"
  note "ErsatzTV uses ${FFMPEG_DIR}/bin; the system FFmpeg is untouched."
}

# point_ersatztv_at_ffmpeg updates the paths ErsatzTV has already recorded.
#
# ErsatzTV resolves ffmpeg and ffprobe from PATH once, on its very first run,
# and stores the result in its own settings. Putting our build first on PATH
# therefore only helps a fresh install; one that has already started keeps
# pointing at /usr/bin and keeps reporting the version as too old.
#
# The same two values are editable in the web UI under Settings, so this is
# doing by hand what the user would otherwise have to. It is deliberately
# cautious: it backs the database up, only touches those two keys, and only
# when they do not already point at our build.
point_ersatztv_at_ffmpeg() {
  local db="/var/lib/ersatztv/.local/share/ersatztv/ersatztv.sqlite3"

  # Never point it at something that is not there.
  if [[ ! -x "${FFMPEG_DIR}/bin/ffmpeg" ]]; then
    skip "no FFmpeg at ${FFMPEG_DIR}/bin; leaving ErsatzTV's setting alone"
    return
  fi
  if [[ ! -f "$db" ]]; then
    # A fresh install has no database yet and will read PATH on first run.
    skip "ErsatzTV has no settings database yet; it will pick up FFmpeg on first run"
    return
  fi
  if ! command -v sqlite3 >/dev/null 2>&1; then
    warn "sqlite3 is not available, so ErsatzTV's recorded FFmpeg path cannot be updated."
    warn "  Set it by hand at Settings -> FFmpeg: ${FFMPEG_DIR}/bin/ffmpeg"
    return
  fi

  local current
  current="$(sqlite3 "$db" \
    "SELECT Value FROM ConfigElement WHERE Key = 'ffmpeg.ffmpeg_path';" 2>/dev/null || true)"

  if [[ "$current" == "${FFMPEG_DIR}/bin/ffmpeg" ]]; then
    skip "ErsatzTV already points at ${FFMPEG_DIR}/bin"
    return
  fi
  if [[ -z "$current" ]]; then
    skip "ErsatzTV has not recorded an FFmpeg path; it will pick ours up from PATH"
    return
  fi

  if (( DRY_RUN )); then
    info "[dry-run] would repoint ErsatzTV from ${current} to ${FFMPEG_DIR}/bin/ffmpeg"
    return
  fi

  # Stop it first so nothing is writing while we are.
  local was_active=0
  if systemctl is-active --quiet ersatztv.service 2>/dev/null; then
    was_active=1
    systemctl stop ersatztv.service
  fi

  local backup="${db}.$(date +%Y%m%d%H%M%S).bak"
  cp -p "$db" "$backup"

  if sqlite3 "$db" \
      "UPDATE ConfigElement SET Value = '${FFMPEG_DIR}/bin/ffmpeg'  WHERE Key = 'ffmpeg.ffmpeg_path';
       UPDATE ConfigElement SET Value = '${FFMPEG_DIR}/bin/ffprobe' WHERE Key = 'ffmpeg.ffprobe_path';" 2>/dev/null; then
    ok "repointed ErsatzTV from ${current} to ${FFMPEG_DIR}/bin"
    info "previous settings saved as ${backup}"
  else
    warn "could not update ErsatzTV's recorded FFmpeg path; restoring the backup."
    warn "  Set it by hand at Settings -> FFmpeg: ${FFMPEG_DIR}/bin/ffmpeg"
    cp -p "$backup" "$db"
  fi

  (( was_active )) && systemctl start ersatztv.service
  return 0
}

# ---------------------------------------------------------------------------
# systemd
# ---------------------------------------------------------------------------

install_services() {
  step "Installing systemd units"

  local changed=0
  for unit in timeblaster.service timeblaster-wifi.service; do
    local src="${SCRIPT_DIR}/deploy/systemd/${unit}"
    local dst="${SYSTEMD_DIR}/${unit}"
    [[ -f "$src" ]] || die "missing ${src}"

    if [[ -f "$dst" ]] && cmp -s "$src" "$dst"; then
      skip "${unit} is already current"
      continue
    fi
    run install -m 0644 -o root -g root "$src" "$dst"
    ok "installed ${unit}"
    changed=1
  done

  # Unconditionally. Gating this was a bug factory: a unit could be correct on
  # disk while systemd still ran a stale copy, which is exactly what happened
  # when ersatztv.service was rewritten by a run that then decided nothing had
  # changed. A reload costs a few hundred milliseconds and is idempotent, so
  # there is nothing to gain by being clever about it.
  run systemctl daemon-reload
  ok "reloaded systemd"

  for unit in timeblaster-wifi.service timeblaster.service; do
    if systemctl is-enabled "$unit" >/dev/null 2>&1; then
      skip "${unit} is already enabled"
    else
      run systemctl enable "$unit"
      ok "enabled ${unit}"
    fi
  done

  if [[ -f "${SYSTEMD_DIR}/ersatztv.service" ]]; then
    if systemctl is-enabled ersatztv.service >/dev/null 2>&1; then
      skip "ersatztv.service is already enabled"
    else
      run systemctl enable ersatztv.service
      ok "enabled ersatztv.service"
    fi
  fi
}

start_services() {
  step "Starting services"

  if (( DRY_RUN )); then
    info "[dry-run] would validate the configuration and (re)start the services"
    return
  fi

  # Validate before restarting: a bad config file should be reported here rather
  # than as a service that will not come up.
  if ! "${BIN_DIR}/timeblasterd" --config "$CONFIG_FILE" --check-config; then
    die "the configuration at ${CONFIG_FILE} is invalid; the services were not restarted"
  fi

  for unit in timeblaster-wifi.service timeblaster.service; do
    run systemctl restart "$unit"
  done
  [[ -f "${SYSTEMD_DIR}/ersatztv.service" ]] && run systemctl restart ersatztv.service || true

  # Give them a moment to fail loudly if they are going to.
  sleep 3

  local failed=0
  for unit in timeblaster.service timeblaster-wifi.service; do
    if systemctl is-active --quiet "$unit"; then
      ok "${unit} is running"
    else
      warn "${unit} did not start. Check: journalctl -u ${unit} -n 50 --no-pager"
      failed=1
    fi
  done

  if [[ -f "${SYSTEMD_DIR}/ersatztv.service" ]]; then
    if systemctl is-active --quiet ersatztv.service; then
      ok "ersatztv.service is running"
    else
      warn "ersatztv.service did not start. Timeblaster works without it; the"
      warn "  television will show the standby image. Check: journalctl -u ersatztv.service"
    fi
  fi

  return $failed
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------

do_uninstall() {
  step "Uninstalling Timeblaster services"
  info "Configuration, alarms and sounds are left in place."

  for unit in timeblaster.service timeblaster-wifi.service; do
    if systemctl is-active --quiet "$unit" 2>/dev/null; then
      run systemctl stop "$unit"
      ok "stopped ${unit}"
    fi
    if systemctl is-enabled "$unit" >/dev/null 2>&1; then
      run systemctl disable "$unit"
      ok "disabled ${unit}"
    fi
  done
  run systemctl daemon-reload

  printf '\n%sTimeblaster services are stopped and disabled.%s\n\n' "$C_BOLD" "$C_RESET"
  printf 'Left in place (delete by hand if you really mean to):\n'
  printf '  %s   alarms, settings and sounds\n' "$STATE_DIR"
  printf '  %s   configuration\n' "$CONFIG_DIR"
  printf '  %s/{timeblasterd,timeblaster-wifi,tbctl}\n\n' "$BIN_DIR"
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

print_summary() {
  local host
  host="$(hostname)"
  local port
  port="$(grep -E '^\s*listen_address\s*=' "$CONFIG_FILE" 2>/dev/null | head -1 | sed -E 's/.*:([0-9]+)".*/\1/')"
  port="${port:-8080}"
  local url="http://${host}.local"
  [[ "$port" != "80" ]] && url="${url}:${port}"

  printf '\n'
  printf '%s────────────────────────────────────────────────────────────%s\n' "$C_BOLD" "$C_RESET"
  printf '%s  Timeblaster installation summary%s\n' "$C_BOLD" "$C_RESET"
  printf '%s────────────────────────────────────────────────────────────%s\n\n' "$C_BOLD" "$C_RESET"

  printf '  Companion app   %s\n' "$url"
  printf '  ErsatzTV        http://%s.local:8409\n' "$host"
  printf '  Configuration   %s\n' "$CONFIG_FILE"
  printf '  Alarm sounds    %s\n' "$SOUNDS_DIR"
  printf '  Database        %s/timeblaster.db\n\n' "$STATE_DIR"

  printf '  %sServices%s\n' "$C_BOLD" "$C_RESET"
  for unit in timeblaster.service timeblaster-wifi.service ersatztv.service avahi-daemon.service; do
    if ! systemctl list-unit-files "$unit" >/dev/null 2>&1 || \
       ! systemctl cat "$unit" >/dev/null 2>&1; then
      printf '    %-28s not installed\n' "$unit"
      continue
    fi
    local active enabled
    active="$(systemctl is-active "$unit" 2>/dev/null || true)"
    enabled="$(systemctl is-enabled "$unit" 2>/dev/null || true)"
    local mark="$C_GREEN✓$C_RESET"
    [[ "$active" == "active" ]] || mark="$C_YELLOW!$C_RESET"
    printf '    %b %-26s %s / %s\n' "$mark" "$unit" "$active" "$enabled"
  done

  printf '\n  %sHardware%s\n' "$C_BOLD" "$C_RESET"
  if [[ -e /dev/timeblaster-nano ]]; then
    printf '    %b Nano          /dev/timeblaster-nano\n' "$C_GREEN✓$C_RESET"
  elif compgen -G "/dev/serial/by-id/*" >/dev/null; then
    printf '    %b Nano          not at /dev/timeblaster-nano, but serial devices exist:\n' "$C_YELLOW!$C_RESET"
    for d in /dev/serial/by-id/*; do printf '                    %s\n' "$d"; done
  else
    printf '    %b Nano          not detected (plug in the Arduino and rerun, or check `tbctl ports`)\n' "$C_YELLOW!$C_RESET"
  fi

  if command -v aplay >/dev/null 2>&1; then
    local usb_card
    usb_card="$(grep -i usb /proc/asound/cards 2>/dev/null | head -1 | sed -E 's/^\s*([0-9]+)\s*\[([^]]+)\].*/\1 [\2]/' || true)"
    if [[ -n "$usb_card" ]]; then
      printf '    %b USB audio     card %s\n' "$C_GREEN✓$C_RESET" "$usb_card"
    else
      printf '    %b USB audio     no USB sound card found; alarms will be silent.\n' "$C_YELLOW!$C_RESET"
      printf '                    Plug in the speaker, then: cat /proc/asound/cards\n'
      printf '                    and set audio.alarm_device in %s\n' "$CONFIG_FILE"
    fi
  fi

  if (( ${#WARNINGS[@]} > 0 )); then
    printf '\n  %sWarnings%s\n' "$C_YELLOW" "$C_RESET"
    for w in "${WARNINGS[@]}"; do printf '    · %s\n' "$w"; done
  fi

  if (( ${#NOTES[@]} > 0 )); then
    printf '\n  %sNotes%s\n' "$C_BOLD" "$C_RESET"
    for n in "${NOTES[@]}"; do printf '    · %s\n' "$n"; done
  fi

  printf '\n  %sNext steps%s\n' "$C_BOLD" "$C_RESET"
  printf '    1. Copy alarm MP3s into %s\n' "$SOUNDS_DIR"
  printf '    2. Add media and create channels at http://%s.local:8409\n' "$host"
  printf '    3. Open %s and create an alarm\n' "$url"
  printf '    4. Check everything:  tbctl health\n'
  printf '    5. Watch the logs:    journalctl -u timeblaster.service -f\n'

  printf '\n  To reconfigure Wi-Fi at any time, hold the Wi-Fi button for five seconds.\n'

  if (( REBOOT_REQUIRED )); then
    printf '\n  %s⚠ A reboot is required for the hostname and console changes.%s\n' "$C_YELLOW" "$C_RESET"
    printf '    This script does not reboot by itself. When you are ready:  sudo reboot\n'
  fi

  printf '\n'
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  parse_args "$@"

  printf '%s\n' "$C_BOLD"
  printf 'Timeblaster setup\n'
  printf '%s\n' "$C_RESET"
  (( DRY_RUN )) && printf '%sDry run: no changes will be made.%s\n' "$C_YELLOW" "$C_RESET"

  if (( UNINSTALL )); then
    [[ $EUID -eq 0 ]] || die "this script must run as root: sudo ./setup.sh --uninstall"
    do_uninstall
    exit 0
  fi

  preflight
  install_packages
  create_user
  create_directories
  build_binaries
  install_config
  install_assets
  install_udev
  configure_hostname
  configure_console
  install_ersatztv
  install_ffmpeg
  install_services

  local start_failed=0
  start_services || start_failed=1

  print_summary

  if (( start_failed )); then
    printf '%sSome services did not start. See the warnings above.%s\n\n' "$C_YELLOW" "$C_RESET"
    exit 1
  fi
  printf '%sDone.%s\n\n' "$C_GREEN" "$C_RESET"
}

main "$@"
