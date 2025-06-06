#!/bin/sh

mkdir -p /persist/vector

export VECTOR_LOG="vector=info,vector::sources::util::unix_stream=warn"
export VECTOR_LOG_FORMAT="json"

# vector -w 2>&1 | tee -a /persist/vector/vector.log
vector -w >> /persist/vector/stdout.log 2>> /persist/vector/stderr.log
