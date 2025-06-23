#!/bin/sh
set -eu

export VECTOR_LOG="vector=info,vector::sources::util::unix_stream=warn"
export VECTOR_LOG_FORMAT="text"
export VECTOR_WATCH_CONFIG="false" # we handle config reload ourselves through SIGHUP
export VECTOR_CONFIG="/persist/vector/config/vector.yaml"
export ALLOCATION_TRACING="true"

DEFAULT_VECTOR_CONFIG="/etc/vector/vector.yaml"
BACKUP=${VECTOR_CONFIG}.bak
PIDFILE=/var/run/vector.pid

# where to log everything
LOGFILE=/persist/vector/internal.log
PIPE=/tmp/vector-logpipe

# --- Logging setup ---------------------------------------------------------

# ensure target dir exists
mkdir -p "$(dirname "$LOGFILE")"

# recreate pipe
[ -p "$PIPE" ] && rm "$PIPE"
mkfifo "$PIPE"

# start tee in background to write to file and stdout
tee -a "$LOGFILE" < "$PIPE" &
# redirect ALL stdout+stderr into pipe
exec > "$PIPE" 2>&1

# now all output from here on will be duplicated to console AND $LOGFILE

# --- pre‐req check ---------------------------------------------------------

if ! command -v inotifywait >/dev/null 2>&1; then
  echo "ERROR: inotifywait not found. Install with:"
  echo "  apk add --no-cache inotify-tools"
  exit 1
fi

# --- initial setup --------------------------------------------------------

if [ ! -f "$VECTOR_CONFIG" ]; then
  echo "No Vector config found at $VECTOR_CONFIG"
  echo "Copying default config from $DEFAULT_VECTOR_CONFIG"
  mkdir -p "$(dirname "$VECTOR_CONFIG")"
  cp "$DEFAULT_VECTOR_CONFIG" "$VECTOR_CONFIG"
fi

# --- initial backup --------------------------------------------------------

if [ ! -f "$BACKUP" ]; then
  echo "Creating initial backup of config…"
  cp "$VECTOR_CONFIG" "$BACKUP"
fi

# --- validate & reload fn -------------------------------------------------

validate_and_reload() {
  echo "Validating new Vector config…"
  if vector validate --config "$VECTOR_CONFIG"; then
    echo "✅ Config is valid — backing up"
    cp "$VECTOR_CONFIG" "$BACKUP"
    kill -HUP "$(cat "$PIDFILE")"
  else
    echo "❌ Config invalid — restoring last good"
    cp "$BACKUP" "$VECTOR_CONFIG"
    kill -HUP "$(cat "$PIDFILE")"
  fi
}

# --- launch & watch --------------------------------------------------------

echo "Starting Vector…"
vector &
echo $! > "$PIDFILE"

echo "Watching $VECTOR_CONFIG for changes…"
inotifywait -m -e close_write "$(dirname "$VECTOR_CONFIG")" |
while read _ _ changed; do
  if [ "$changed" = "$(basename "$VECTOR_CONFIG")" ]; then
    validate_and_reload
  fi
done
