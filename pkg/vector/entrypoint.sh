#!/bin/sh

mkdir -p /persist/vector
# vector -w 2>&1 | tee -a /persist/vector/vector.log
vector -w >> /persist/vector/stdout.log 2>> /persist/vector/stderr.log
