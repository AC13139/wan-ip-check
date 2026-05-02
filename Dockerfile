# =============================================================================
# Multi-stage Dockerfile for wan-ip-check v2
# =============================================================================

# Stage 1: Build the Go binary (templates + static embedded via embed.FS)
# --platform=$BUILDPLATFORM pins the builder to the host arch so cross-arch
# images skip QEMU emulation. TARGETOS / TARGETARCH are populated by buildx
# and drive Go's native cross-compilation in the build step below.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY templates/ templates/
COPY static/ static/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT}" \
    -o wan-ip-check .

# Stage 2: Minimal runtime image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates iproute2 && \
    rm -rf /var/cache/apk/*
WORKDIR /app
COPY --from=builder /build/wan-ip-check .

EXPOSE 7373
VOLUME ["/app/data"]

ENTRYPOINT ["./wan-ip-check"]
