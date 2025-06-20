#!/bin/sh

mkdir -p /persist/vector/config
cp /etc/vector/vector.yaml /persist/vector/config/vector.yaml

export VECTOR_LOG="vector=info,vector::sources::util::unix_stream=warn"
export VECTOR_LOG_FORMAT="text"
export VECTOR_WATCH_CONFIG="true"
export VECTOR_CONFIG="/persist/vector/config/vector.yaml"
export ALLOCATION_TRACING="true"

while true; do
    # vector 2>&1 | tee -a /persist/vector/vector.log
    vector >> /persist/vector/stdout.log 2>> /persist/vector/stderr.log
    exit_code=$?

    if [ $exit_code -ne 0 ]; then
        echo "Vector exited with code $exit_code, restoring backup config" >> /persist/vector/stderr.log
        if [ -f /persist/vector/config/vector.yaml.bak ]; then
            cp /persist/vector/config/vector.yaml.bak /persist/vector/config/vector.yaml
            echo "Backup config restored, restarting vector" >> /persist/vector/stderr.log
        else
            echo "No backup config found at /persist/vector/config/vector.yaml.bak" >> /persist/vector/stderr.log
            break
        fi
        sleep 1
    else
        echo "Vector exited normally" >> /persist/vector/stdout.log
        break
    fi
done
