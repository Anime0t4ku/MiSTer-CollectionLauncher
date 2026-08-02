#!/bin/bash
VERSION="0.5.0"
BASE="/media/fat/Scripts/.config/CollectionLauncher"
TMP="$BASE/tmp"
mkdir -p "$TMP"
printf '%s wrapper start version=%s args=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$VERSION" "$*" >> "$TMP/CollectionLauncher.log"

case "$1" in
  --version|-v)
    echo "CollectionLauncher v$VERSION"
    exit 0
    ;;
esac

if [ ! -x "$BASE/collection_launcher" ]; then
  printf '%s wrapper error missing executable=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$BASE/collection_launcher" >> "$TMP/CollectionLauncher.log"
  echo "CollectionLauncher v$VERSION"
  echo "Missing executable: $BASE/collection_launcher"
  exit 1
fi

exec "$BASE/collection_launcher" "$@"
