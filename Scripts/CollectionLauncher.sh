#!/bin/bash
VERSION="1.1.0"
printf '\033[?25l' > /dev/tty 2>/dev/null || true
BASE="/media/fat/Scripts/.config/CollectionLauncher"
TMP="$BASE/tmp"
BIN="$BASE/collection_launcher"
HANDOFF_MARKER="$TMP/service_handoff"
SAM_SCRIPT="/media/fat/Scripts/MiSTer_SAM_on.sh"
BGM_SCRIPT="/media/fat/Scripts/bgm.sh"
BGM_SOCK="/tmp/bgm.sock"

SAM_WAS_ENABLED=0
BGM_WAS_RUNNING=0
SERVICES_PREPARED=0
CHILD_PID=""

mkdir -p "$TMP"
rm -f "$HANDOFF_MARKER"
printf '%s wrapper start version=%s args=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$VERSION" "$*" >> "$TMP/CollectionLauncher.log"

case "$1" in
  --version|-v)
    echo "CollectionLauncher v$VERSION"
    printf '\033[?25h' > /dev/tty 2>/dev/null || true
    exit 0
    ;;
esac

if [ ! -x "$BIN" ]; then
  printf '%s wrapper error missing executable=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$BIN" >> "$TMP/CollectionLauncher.log"
  echo "CollectionLauncher v$VERSION"
  echo "Missing executable: $BIN"
  printf '\033[?25h' > /dev/tty 2>/dev/null || true
  exit 1
fi

sam_autoplay_running() {
  ps 2>/dev/null | grep -q '[M]iSTer_SAM_MCP.py'
}

prepare_background_services() {
  # CollectionLauncher is a native Linux frontend running over the menu core.
  # Prevent SAM autoplay and BGM from taking over while the frontend is active.
  if [ -f "$SAM_SCRIPT" ] && sam_autoplay_running; then
    SAM_WAS_ENABLED=1
    "$SAM_SCRIPT" disable >/dev/null 2>&1 || true
    printf '%s wrapper disabled SAM autoplay\n' "$(date '+%Y-%m-%d %H:%M:%S')" >> "$TMP/CollectionLauncher.log"
  fi

  if [ -f "$BGM_SCRIPT" ] && [ -S "$BGM_SOCK" ]; then
    BGM_WAS_RUNNING=1
    "$BGM_SCRIPT" stop >/dev/null 2>&1 || true
    printf '%s wrapper stopped BGM\n' "$(date '+%Y-%m-%d %H:%M:%S')" >> "$TMP/CollectionLauncher.log"
  fi

  SERVICES_PREPARED=1
}

wait_for_core_handoff() {
  [ -f "$HANDOFF_MARKER" ] || return

  # CollectionLauncher intentionally exits immediately after load_core is
  # accepted. Keep SAM/BGM suppressed until MiSTer has actually left MENU so
  # neither service can race the new core during that short handoff window.
  i=0
  while [ "$i" -lt 100 ]; do
    core=""
    if [ -f /tmp/CORENAME ]; then
      core="$(tr -d '\r\n' < /tmp/CORENAME 2>/dev/null)"
    fi
    if [ -n "$core" ] && [ "$core" != "MENU" ]; then
      printf '%s wrapper core handoff complete core=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$core" >> "$TMP/CollectionLauncher.log"
      return
    fi
    sleep 0.1
    i=$((i + 1))
  done

  printf '%s wrapper core handoff wait timed out; restoring services\n' "$(date '+%Y-%m-%d %H:%M:%S')" >> "$TMP/CollectionLauncher.log"
}

restore_background_services() {
  [ "$SERVICES_PREPARED" = "1" ] || return
  SERVICES_PREPARED=0

  wait_for_core_handoff
  rm -f "$HANDOFF_MARKER"

  if [ "$SAM_WAS_ENABLED" = "1" ] && [ -f "$SAM_SCRIPT" ]; then
    "$SAM_SCRIPT" enable >/dev/null 2>&1 || true
    printf '%s wrapper restored SAM autoplay\n' "$(date '+%Y-%m-%d %H:%M:%S')" >> "$TMP/CollectionLauncher.log"
  fi

  if [ "$BGM_WAS_RUNNING" = "1" ] && [ -f "$BGM_SCRIPT" ]; then
    "$BGM_SCRIPT" exec >/dev/null 2>&1 &
    printf '%s wrapper restored BGM\n' "$(date '+%Y-%m-%d %H:%M:%S')" >> "$TMP/CollectionLauncher.log"
  fi
}

cleanup() {
  restore_background_services
  printf '\033[?25h' > /dev/tty 2>/dev/null || true
}

handle_int() {
  if [ -n "$CHILD_PID" ]; then
    kill -INT "$CHILD_PID" 2>/dev/null || true
    wait "$CHILD_PID" 2>/dev/null || true
    CHILD_PID=""
  fi
  exit 130
}

handle_term() {
  if [ -n "$CHILD_PID" ]; then
    kill -TERM "$CHILD_PID" 2>/dev/null || true
    wait "$CHILD_PID" 2>/dev/null || true
    CHILD_PID=""
  fi
  exit 143
}

trap cleanup EXIT
trap handle_int INT
trap handle_term TERM

prepare_background_services

"$BIN" "$@" &
CHILD_PID=$!
wait "$CHILD_PID"
RC=$?
CHILD_PID=""
exit "$RC"
