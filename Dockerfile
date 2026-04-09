# =============================================================================
# Multi-stage Dockerfile for WAN IP Check (Go)
# =============================================================================

# Stage 1: Build the Go binary
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o wan-ip-check .

# Stage 2: Minimal runtime image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates iproute2 && \
    rm -rf /var/cache/apk/*
WORKDIR /app
COPY --from=builder /build/wan-ip-check .
ENTRYPOINT ["./wan-ip-check"]
