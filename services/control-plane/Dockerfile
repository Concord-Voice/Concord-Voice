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
RUN npm ci --ignore-scripts
COPY --from=admin_ui . .
RUN npm run build && test -f dist/index.html

# Go build stage
# Pin the patch tag and reviewed digest to go.mod's `toolchain` directive
# (or its `go` directive when no toolchain is declared). The builder supplies
# the shipped stdlib directly, without a GOTOOLCHAIN build-time download.
# The `go` directive separately controls the module's compatibility defaults.
# scripts/check-go-toolchain.py enforces alignment with Dockerfile.ops-agent.
FROM golang:1.26.9-alpine@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0 AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application and tools
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o migrate-media ./cmd/migrate-media
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o mfa-rekey ./cmd/mfa-rekey

# Final stage. Tracks the supported Alpine line -- 3.21 reaches EOL 2026-11-01.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

LABEL org.opencontainers.image.source="https://github.com/Concord-Voice/Concord-Voice"
LABEL org.opencontainers.image.description="Concord Voice control-plane service"
LABEL org.opencontainers.image.licenses="LicenseRef-CVSL-1.0"

RUN apk --no-cache add ca-certificates \
    && addgroup -S appuser && adduser -S appuser -G appuser

# Run as non-root user
WORKDIR /home/appuser

# Copy binaries from builder
COPY --from=builder /app/main .
COPY --from=builder /app/migrate-media .
COPY --from=builder /app/mfa-rekey .

# Copy migrations
COPY --from=builder /app/migrations ./migrations

# The runtime serves these files through the injected admin fs.FS. Node and npm
# remain confined to admin-ui-builder and are absent from this Alpine stage.
COPY --chown=appuser:appuser --from=admin-ui-builder /src/dist ./admin-ui

# GID 10001 is reserved for the host security-event bind mounts. Keep any
# image-local inode from inheriting that runtime supplementary identity.
RUN matches="$(find / -xdev -group 10001 -print -quit)" && test -z "$matches"

USER appuser

# Expose port
EXPOSE 8080

# Run the binary
CMD ["./main"]
