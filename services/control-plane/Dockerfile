# Admin Portal build stage. This is the reviewed node:26-alpine multi-platform
# index at sha256:0b36e8c136b94cd4fcf02188228e76c31ad5872eef3fec8cbd2eee500cfd9e80;
# the named admin_ui context keeps the primary Go context narrow.
# The `node:26-alpine` tag component is load-bearing, not decorative: a bare
# `FROM node@sha256:` gives Dependabot no variant to anchor on, so it resolves
# the default tag (`node:latest`, currently Debian-based `node:26`). PR #2800
# did exactly that -- swapping this stage to a 443 MB Debian image with nothing
# in the diff to show it. Keep tag+digest together on every bump.
FROM node:26-alpine@sha256:0b36e8c136b94cd4fcf02188228e76c31ad5872eef3fec8cbd2eee500cfd9e80 AS admin-ui-builder

WORKDIR /src
COPY --from=admin_ui package.json package-lock.json ./
# Cache mounts (here and in the Go stage) persist on the builder between builds
# and never enter a layer. A fresh builder starts them empty, so the cache-cold
# CI job (ADR-0006/0016) still downloads and compiles everything.
RUN --mount=type=cache,target=/root/.npm \
    npm ci --ignore-scripts
COPY --from=admin_ui . .
RUN npm run build && test -f dist/index.html

# Go build stage
# Pin the patch tag and reviewed digest to go.mod's `toolchain` directive
# (or its `go` directive when no toolchain is declared). The builder supplies
# the shipped stdlib directly, without a GOTOOLCHAIN build-time download.
# The `go` directive separately controls the module's compatibility defaults.
# scripts/check-go-toolchain.py enforces alignment with Dockerfile.ops-agent.
FROM golang:1.27.3-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
# Dockerfile.ops-agent mounts the same two cache targets, so builds of either
# image on one builder share compiled packages.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code
COPY . .

# Build the application and tools in one invocation so shared packages compile
# once. No -a: Go's build cache keys on CGO_ENABLED, so forcing a full rebuild
# buys nothing. -trimpath and -s -w match Dockerfile.ops-agent.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ \
      ./cmd/server ./cmd/migrate-media ./cmd/mfa-rekey

# Final stage. Tracks the supported Alpine line -- 3.21 reaches EOL 2026-11-01.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

LABEL org.opencontainers.image.source="https://github.com/Concord-Voice/Concord-Voice"
LABEL org.opencontainers.image.description="Concord Voice control-plane service"
LABEL org.opencontainers.image.licenses="LicenseRef-CVSL-1.0"
LABEL org.concord.redis-credential-modes="legacy,managed"

RUN apk --no-cache add ca-certificates \
    && addgroup -S appuser && adduser -S appuser -G appuser

# Run as non-root user
WORKDIR /home/appuser

# Copy binaries from builder
COPY --from=builder /out/server ./main
COPY --from=builder /out/migrate-media /out/mfa-rekey ./

# Copy migrations
COPY --from=builder /app/migrations ./migrations

# The runtime serves these files through the injected admin fs.FS. Node and npm
# remain confined to admin-ui-builder and are absent from this Alpine stage.
COPY --chown=appuser:appuser --from=admin-ui-builder /src/dist ./admin-ui

# GID 10001 is reserved for the host security-event bind mounts, and GID 10002
# for host secret files under /opt/concord/etc (apple_signin.p8). Keep any
# image-local inode from inheriting either runtime supplementary identity.
RUN matches="$(find / -xdev \( -group 10001 -o -group 10002 \) -print -quit)" && test -z "$matches"

USER appuser

# Expose port
EXPOSE 8080

# Run the binary
CMD ["./main"]
