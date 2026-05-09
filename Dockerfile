# ==============================================================
# WhatsApp Sniper Bot — Go + whatsmeow
# Multi-stage build producing a static ~14 MB scratch image.
# ==============================================================

# --------------------------------------------------------------
# Stage 1 — Build
# --------------------------------------------------------------
FROM golang:1.26-alpine AS builder

# git is needed by `go mod download` for some module paths; ca-certs
# are copied into the runtime image below.
RUN apk add --no-cache ca-certificates git tzdata

WORKDIR /src

# Cache dependencies (go.sum is generated on first `go mod tidy`
# locally; if absent, the Docker build will fall back to resolving
# during `go mod download`).
COPY go.mod ./
COPY go.sum* ./
RUN go mod download

# Bring in the full source and compile a fully static binary.
COPY . .

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

RUN go build \
        -trimpath \
        -ldflags="-s -w -extldflags=-static" \
        -o /out/sniper \
        ./cmd/sniper

# --------------------------------------------------------------
# Stage 2 — Runtime (scratch = no shell, no libc, no attack surface)
# --------------------------------------------------------------
FROM scratch

# Root TLS certificates are required for Postgres (Neon) and the
# WhatsApp WebSocket endpoint.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Timezone database for log timestamps and any scheduling logic.
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=builder /out/sniper /sniper

ENV TZ=Asia/Riyadh

# Render injects $PORT (typically 10000) — the binary binds to it.
EXPOSE 10000

ENTRYPOINT ["/sniper"]
