# -------------------------------------------------------------------------------
# Oracle Watchdog - Container Image
#
# Author: Alex Freidah
#
# Multi-stage build produces a minimal scratch container. Monitors Oracle Cloud
# free-tier instances via Consul sessions and triggers OCI restart cycles.
# -------------------------------------------------------------------------------

FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine AS builder

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

# --- Install build dependencies ---
RUN apk add --no-cache git ca-certificates

# --- Copy go module files and download dependencies ---
COPY go.mod go.sum ./
RUN go mod download

# --- Copy source code ---
COPY cmd/ cmd/
COPY internal/ internal/

# --- Build binary (native cross-compilation, no QEMU needed) ---
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X github.com/afreidah/oracle-watchdog/internal/tracing.Version=${VERSION}" \
    -o oracle-watchdog ./cmd/watchdog

# -------------------------------------------------------------------------
# Runtime Image
# -------------------------------------------------------------------------

FROM alpine:3.21

ARG VERSION=dev

LABEL org.opencontainers.image.title="oracle-watchdog" \
      org.opencontainers.image.description="Distributed monitoring and recovery for Oracle Cloud free-tier instances" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/afreidah/oracle-watchdog"

RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 appuser

COPY --from=builder /build/oracle-watchdog /usr/local/bin/

USER appuser

EXPOSE 9104 9105

ENTRYPOINT ["oracle-watchdog"]
