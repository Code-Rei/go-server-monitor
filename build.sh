#!/bin/bash
# build.sh — Builds for Linux amd64 (for Docker or Linux servers)

set -e

echo -e "\n🔨 Building server-monitor (Linux / amd64) ..."

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-s -w" -trimpath -o server-monitor .

SIZE=$(du -sh server-monitor | cut -f1)
echo -e "✅ Built: server-monitor  ($SIZE)\n"
echo "Run it:  ./server-monitor"
echo "Open:    http://localhost:8266"
