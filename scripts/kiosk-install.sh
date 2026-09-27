#!/usr/bin/env bash
# Hearth display kiosk installer.
#
# Turns a Raspberry Pi (or any Debian-family Linux box with a desktop) into a
# wall screen for a Hearth display: on power-up it logs in, opens the display
# full-screen, and keeps it there. Run it as the user the screen logs in as,
# not root; it asks for sudo for the system settings.
#
#   curl -fsSL https://hearthcmd.com/kiosk.sh | bash -s -- http://<display-address>:8090
#
# hearthcmd.com/kiosk.sh is a small wrapper (in the website repo) that fetches
# this script from the latest CLI release's tag and runs it, the way install.sh
# does for the CLI itself.
#
# The display address is shown in the Hearth app (Hosts → the computer running
# the display). Run it again to change the address; --uninstall removes it.
#
# What it does (each step is safe to repeat):
#   - installs Chromium if it's missing
#   - installs ~/.local/bin/hearth-kiosk and starts it when you log in: it waits
#     for the display to answer, opens it full-screen, and reopens it after a
#     crash (Alt+F4 closes it and leaves you at the desktop)
#   - Raspberry Pi OS: logs in to the desktop automatically and turns off screen
#     blanking (raspi-config)
#   - turns off Wi-Fi power saving, which makes a Pi screen stutter
#
# The screen pairs itself the first time it opens: it shows a code to enter in
# the Hearth app. Its pairing lives in Chromium's profile, so it survives
# reboots; nothing here touches that profile.
#
# The steps are wrapped in main(), called on the last line, so a download cut
# short can't run half a script.

set -euo pipefail

main() {
  local url="" autologin=1 uninstall=0 arg
  for arg in "$@"; do
    case "$arg" in
      --no-autologin) autologin=0 ;;
      --uninstall) uninstall=1 ;;
      -h|--help) usage; exit 0 ;;
      -*) die "unknown option: $arg (see --help)" ;;
      *) [ -z "$url" ] || die "one display address, please (got '$url' and '$arg')"; url="$arg" ;;
    esac
  done

  preflight
  if [ "$uninstall" = 1 ]; then
    do_uninstall
    return
  fi
  [ -n "$url" ] || { usage >&2; exit 2; }
  url="$(normalize_url "$url")"

  say "Setting up this computer as a Hearth screen for $url"
  sudo -v || die "sudo is needed for the system settings"

  ensure_browser
  write_launcher "$url"
  write_autostart
  migrate_old_kiosk_script
  if have raspi-config; then
    configure_raspi "$autologin"
  else
    note "Not Raspberry Pi OS: turn on automatic desktop login and turn off screen blanking in your desktop's settings, so the screen comes back by itself after a power cut."
  fi
  wifi_power_save_off
  check_reachable "$url"
  finish
}

usage() {
  cat <<'EOF'
Usage: kiosk-install.sh <display-address> [--no-autologin]
       kiosk-install.sh --uninstall

  <display-address>  the address the Hearth app shows for the display,
                     e.g. http://192.168.1.20:8090
  --no-autologin     leave the login settings alone
  --uninstall        stop opening the display on login and remove what this
                     installed (the screen's pairing is kept in Chromium)
EOF
}

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

LAUNCHER="$HOME/.local/bin/hearth-kiosk"
CONF_DIR="$HOME/.config/hearth-kiosk"
AUTOSTART="$HOME/.config/autostart/hearth-kiosk.desktop"
WIFI_UNIT="/etc/systemd/system/hearth-kiosk-wifi-powersave.service"
WIFI_HELPER="/usr/local/sbin/hearth-kiosk-wifi-powersave"
NM_CONF="/etc/NetworkManager/conf.d/hearth-kiosk-wifi-powersave.conf"

preflight() {
  [ "$(uname -s)" = Linux ] || die "this sets up a Linux screen (Raspberry Pi OS, Debian, Ubuntu)"
  [ "$(id -u)" -ne 0 ] || die "run this as the user the screen logs in as, not as root (it uses sudo where it needs to)"
  have apt-get || die "this needs a Debian-family system (Raspberry Pi OS, Debian, Ubuntu)"
  have sudo || die "sudo is needed"
  have curl || die "curl is needed"
}

# normalize_url accepts only http(s)://host[:port] (an optional trailing slash is
# dropped). The address is stored in a config file and passed to the browser as
# one argument, never evaluated, but a strict shape still keeps a typo from
# turning into something surprising.
normalize_url() {
  local u="${1%/}"
  [[ "$u" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$ ]] ||
    die "'$1' doesn't look like a display address (expected e.g. http://192.168.1.20:8090)"
  printf '%s' "$u"
}

ensure_browser() {
  if have chromium-browser || have chromium; then
    return
  fi
  say "Installing Chromium"
  sudo apt-get update -qq
  # Raspberry Pi OS Bullseye calls it chromium-browser; Bookworm and Debian call
  # it chromium.
  sudo apt-get install -y chromium-browser 2>/dev/null || sudo apt-get install -y chromium ||
    die "couldn't install Chromium"
}

write_launcher() {
  say "Installing the screen launcher ($LAUNCHER)"
  mkdir -p "$(dirname "$LAUNCHER")" "$CONF_DIR"
  printf '%s\n' "$1" >"$CONF_DIR/url"
  cat >"$LAUNCHER" <<'LAUNCHER'
#!/usr/bin/env bash
# Opens the Hearth display full-screen and keeps it open. Installed by the Hearth
# kiosk installer, which starts it on login; the address is in
# ~/.config/hearth-kiosk/url.
set -u
URL="$(head -n1 "$HOME/.config/hearth-kiosk/url" 2>/dev/null || true)"
[ -n "$URL" ] || { echo "hearth-kiosk: no display address in ~/.config/hearth-kiosk/url" >&2; exit 1; }

# One at a time: login can start this more than once (e.g. an older autostart
# entry), and two copies would fight over the browser.
exec 9>"${XDG_RUNTIME_DIR:-/tmp}/hearth-kiosk.lock"
flock -n 9 || exit 0

BROWSER="$(command -v chromium-browser || command -v chromium)"
PREFS="$HOME/.config/chromium/Default/Preferences"

# X11 blanks the screen by itself unless told not to (Wayland is handled by the
# installer's raspi-config step).
if [ "${XDG_SESSION_TYPE:-}" = x11 ] && command -v xset >/dev/null; then
  xset s off; xset -dpms; xset s noblank
fi

while true; do
  # Wait until the display answers. After a power cut this computer often boots
  # before the one serving the display, and a page that never loaded can't
  # retry by itself: Chromium's error page doesn't.
  until curl -fsS -o /dev/null --max-time 5 "$URL/"; do sleep 5; done

  # Clear Chromium's "didn't shut down cleanly" state, so a power cut doesn't
  # leave a restore prompt nobody is there to dismiss.
  [ -f "$PREFS" ] && sed -i 's/"exited_cleanly":false/"exited_cleanly":true/; s/"exit_type":"[^"]*"/"exit_type":"Normal"/' "$PREFS"

  # Never --incognito: the screen's pairing lives in this profile.
  "$BROWSER" \
    --kiosk --noerrdialogs --disable-infobars --no-first-run \
    --disable-session-crashed-bubble --disable-features=Translate \
    --check-for-update-interval=31536000 \
    "$URL" && break   # closed on purpose (Alt+F4): stay at the desktop

  sleep 5             # crashed or killed: wait for the display again, reopen
done
LAUNCHER
  chmod 0755 "$LAUNCHER"
}

write_autostart() {
  say "Starting it when you log in"
  mkdir -p "$(dirname "$AUTOSTART")"
  cat >"$AUTOSTART" <<EOF
[Desktop Entry]
Type=Application
Name=Hearth display
Comment=Opens the Hearth display full-screen
Exec=$LAUNCHER
X-GNOME-Autostart-enabled=true
EOF
}

# The setup guide used to have people write ~/kiosk.sh and start it from their
# desktop's autostart. Point it at the launcher, so whichever entry starts it
# runs the same thing (the launcher's lock keeps it to one copy).
migrate_old_kiosk_script() {
  local old="$HOME/kiosk.sh"
  [ -f "$old" ] || return 0
  grep -q 'hearth-kiosk' "$old" 2>/dev/null && return 0
  cp "$old" "$old.bak"
  printf '#!/usr/bin/env bash\n# Replaced by the Hearth kiosk installer (your old script is ~/kiosk.sh.bak).\nexec %s\n' "$LAUNCHER" >"$old"
  chmod 0755 "$old"
  note "Your old ~/kiosk.sh now starts the new launcher (saved as ~/kiosk.sh.bak)."
}

configure_raspi() {
  if [ "$1" = 1 ]; then
    say "Logging in to the desktop automatically"
    sudo raspi-config nonint do_boot_behaviour B4 || warn "couldn't turn on desktop autologin; set it in raspi-config → System Options → Boot / Auto Login"
  fi
  say "Turning off screen blanking"
  # raspi-config's nonint toggles take 0 = on, 1 = off.
  sudo raspi-config nonint do_blanking 1 || warn "couldn't turn off screen blanking; set it in raspi-config → Display Options → Screen Blanking"
}

wifi_power_save_off() {
  local ifaces=() i
  for i in /sys/class/net/*; do
    [ -d "$i/wireless" ] && ifaces+=("$(basename "$i")")
  done
  [ "${#ifaces[@]}" -gt 0 ] || return 0
  say "Turning off Wi-Fi power saving"
  have iw || sudo apt-get install -y iw >/dev/null || { warn "couldn't install iw; Wi-Fi power saving left on"; return 0; }
  for i in "${ifaces[@]}"; do sudo iw dev "$i" set power_save off 2>/dev/null || true; done
  if systemctl is-active --quiet NetworkManager 2>/dev/null; then
    # NetworkManager turns it back on at every reconnect unless told otherwise
    # (2 = disable).
    printf '[connection]\nwifi.powersave = 2\n' | sudo tee "$NM_CONF" >/dev/null
  fi
  # A script, not an inline ExecStart loop: systemd expands $ in ExecStart itself.
  sudo tee "$WIFI_HELPER" >/dev/null <<'HELPER'
#!/bin/sh
# Turns off Wi-Fi power saving on every wireless interface (Hearth display).
for i in /sys/class/net/*; do
  [ -d "$i/wireless" ] && iw dev "$(basename "$i")" set power_save off
done
exit 0
HELPER
  sudo chmod 0755 "$WIFI_HELPER"
  sudo tee "$WIFI_UNIT" >/dev/null <<EOF
[Unit]
Description=Turn off Wi-Fi power saving for the Hearth display
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ExecStart=$WIFI_HELPER

[Install]
WantedBy=multi-user.target
EOF
  sudo systemctl daemon-reload
  sudo systemctl enable --quiet hearth-kiosk-wifi-powersave.service
}

check_reachable() {
  if curl -fsS -o /dev/null --max-time 5 "$1/"; then
    note "The display at $1 answered."
  else
    warn "the display at $1 didn't answer just now. The screen will wait for it, but check the address in the Hearth app (Hosts) and that the computer running the display is on."
  fi
}

finish() {
  say "Done."
  note "Restart to start the screen:  sudo reboot"
  note "The first time, it shows a code: enter it in the Hearth app (Devices → Displays)."
  note "To change the address, run this again with the new one. To undo: --uninstall"
}

do_uninstall() {
  say "Removing the Hearth screen setup"
  pkill -f "$LAUNCHER" 2>/dev/null || true
  rm -f "$LAUNCHER" "$AUTOSTART"
  rm -rf "$CONF_DIR"
  if [ -f "$HOME/kiosk.sh.bak" ] && grep -q 'hearth-kiosk' "$HOME/kiosk.sh" 2>/dev/null; then
    mv "$HOME/kiosk.sh.bak" "$HOME/kiosk.sh"
    note "Restored your old ~/kiosk.sh."
  fi
  if [ -f "$WIFI_UNIT" ] || [ -f "$NM_CONF" ]; then
    sudo systemctl disable --quiet hearth-kiosk-wifi-powersave.service 2>/dev/null || true
    sudo rm -f "$WIFI_UNIT" "$WIFI_HELPER" "$NM_CONF"
    sudo systemctl daemon-reload
  fi
  note "Left as they were: automatic login, screen blanking, and Chromium (with the screen's pairing)."
}

main "$@"
