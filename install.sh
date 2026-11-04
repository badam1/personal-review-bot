#!/usr/bin/env bash
# Build personal-review-bot into ~/.local/bin and (re)start it as a background service: a launchd agent on macOS,
# a systemd user service on Linux. Safe to re-run after every change.
#   ./install.sh           build + (re)start the service
#   ./install.sh --build   build only
set -euo pipefail
cd "$(dirname "$0")"
[ -d /opt/homebrew/bin ] && PATH="$PATH:/opt/homebrew/bin" # Homebrew's go on Apple silicon

bin="$HOME/.local/bin/personal-review-bot"
mkdir -p "$(dirname "$bin")"
go build -o "$bin" .
echo "🔨 built $bin"
[ "${1:-}" = "--build" ] && exit 0

# The service gets the PATH you install from, so it finds gh, git and claude the way your shell does.
service_path="$(dirname "$bin"):$PATH"
escape() { printf '%s' "$1" | sed -e 's/[\\|&]/\\&/g'; }
render() { sed -e "s|__HOME__|$(escape "$HOME")|g" -e "s|__PATH__|$(escape "$service_path")|g" "$1" > "$2"; }

case "$(uname -s)" in
Darwin)
  label=io.github.badam1.personal-review-bot
  plist="$HOME/Library/LaunchAgents/$label.plist"
  mkdir -p "$(dirname "$plist")" "$HOME/Library/Logs"
  service_path="$(printf '%s' "$service_path" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')" # plist is XML
  render "launchd/$label.plist" "$plist"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  # bootout returns before the job is gone; bootstrapping too early fails with "Bootstrap failed: 5" and leaves it down
  for _ in $(seq 50); do
    launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 || break
    sleep 0.2
  done
  launchctl bootstrap "gui/$(id -u)" "$plist"
  log="~/Library/Logs/personal-review-bot.log"
  ;;
Linux)
  unit="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/personal-review-bot.service"
  mkdir -p "$(dirname "$unit")"
  render systemd/personal-review-bot.service "$unit"
  systemctl --user daemon-reload
  systemctl --user enable --quiet personal-review-bot.service
  systemctl --user restart personal-review-bot.service
  log="journalctl --user -u personal-review-bot"
  ;;
*)
  echo "No service support for $(uname -s) yet: run '$bin daemon' yourself." >&2
  exit 1
  ;;
esac
echo "🤖 personal-review-bot running · page http://localhost:12346 · log: $log · TUI: personal-review-bot"
