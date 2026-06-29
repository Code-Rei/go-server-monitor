# ── Stage 1: Build ───────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Cache deps separately so layer is reused when only source changes
COPY go.mod go.sum ./
RUN go mod download

# Copy source + embedded static files
COPY *.go      ./
COPY static/   ./static/

# Build a static, stripped binary for Linux amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o server-monitor .

# ── Stage 2: Runtime ─────────────────────────────────────────
# Using alpine (not scratch) so lm-sensors / ca-certs are available
FROM alpine:3.19

LABEL maintainer="ServerMonitor"
LABEL description="Lightweight server monitoring — Go binary, ~15 MB image"
LABEL version="2.0.0"

RUN apk add --no-cache \
    ca-certificates \
    lm-sensors \
    tzdata

# Non-root user
RUN addgroup -S monitor && adduser -S monitor -G monitor

WORKDIR /app
COPY --from=builder /build/server-monitor .

RUN chown monitor:monitor server-monitor
USER monitor

ENV PORT=8266
EXPOSE 8266

HEALTHCHECK --interval=15s --timeout=5s --start-period=8s --retries=3 \
    CMD wget -qO- http://localhost:8266/metrics || exit 1

ENTRYPOINT ["./server-monitor"]
