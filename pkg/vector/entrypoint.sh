#!/bin/sh
set -eu

export VECTOR_LOG="vector=info,vector::sources::util::unix_stream=warn"
export VECTOR_LOG_FORMAT="text"
export VECTOR_WATCH_CONFIG="true"
export ALLOCATION_TRACING="true"

DEFAULT_VECTOR_CONFIG="/etc/vector/vector.yaml"
LIVE_CONFIG="/persist/vector/config/vector.yaml"
CONFIG_CANDIDATE=${LIVE_CONFIG}.new
PIDFILE=/var/run/vector.pid

# where to log everything
LOGFILE=/persist/vector/all.log
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

if [ ! -f "$LIVE_CONFIG" ]; then
  echo "No Vector config found at $LIVE_CONFIG"
  echo "Copying default config from $DEFAULT_VECTOR_CONFIG"
  mkdir -p "$(dirname "$LIVE_CONFIG")"
  cp "$DEFAULT_VECTOR_CONFIG" "$LIVE_CONFIG"
fi

# --- launch & watch --------------------------------------------------------

echo "Starting Vector with $LIVE_CONFIG"
vector --config-yaml "$LIVE_CONFIG" &
echo $! > "$PIDFILE"

echo "Watching for new config at $CONFIG_CANDIDATE"
inotifywait -m -e close_write "$(dirname "$CONFIG_CANDIDATE")" |
while read _ _ changed; do
  [ "$changed" != "$(basename "$CONFIG_CANDIDATE")" ] && continue

  echo "Detected new candidate config…"
  echo "Validating $CONFIG_CANDIDATE"
  if vector validate --config-yaml "$CONFIG_CANDIDATE"; then
    echo "✅ Candidate is valid — promoting to live config"
    # atomic swap
    mv "$CONFIG_CANDIDATE" "$LIVE_CONFIG"
  else
    echo "❌ Candidate invalid — discarding $CONFIG_CANDIDATE"
    rm "$CONFIG_CANDIDATE"
  fi
done
