#!/bin/sh

# Wait for /persist/status/uuid to be created by the nodeagent
while [ ! -f /persist/status/uuid ]; do
    sleep 1
done

# Read the UUID from /persist/status/uuid
UUID=$(cat /persist/status/uuid)

# Execute the original vmagent command with the resolved IP
exec /vmagent-prod "$@" -remoteWrite.url="http://localhost:8999/api/v2/edgedevice/id/$UUID/v1/remotewrite" -promscrape.config=/etc/vmagent.yml -remoteWrite.tmpDataPath=/persist/vmagent -remoteWrite.maxDiskUsagePerURL=100MiB