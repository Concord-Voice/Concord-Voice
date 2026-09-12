# Admin Portal build stage. This is the reviewed node:24-alpine multi-platform
# index; the named admin_ui context keeps the primary Go context narrow.
# The `node:24-alpine` tag component is load-bearing, not decorative: a bare
# `FROM node@sha256:` gives Dependabot no variant to anchor on, so it resolves
# the default tag (`node:latest`, currently Debian-based `node:26`). PR #2800
# did exactly that -- swapping this stage to a 443 MB Debian image with nothing
# in the diff to show it. Keep tag+digest together on every bump.
FROM node:26-alpine@sha256:ef24c5053d50fdc3e4e56eb4e7ddb7861874ab0fdc797046ba897581deb8e868 AS admin-ui-builder

WORKDIR /src
COPY --from=admin_ui package.json package-lock.json ./
RUN npm ci --ignore-scripts
COPY --from=admin_ui . .
RUN npm run build && test -f dist/index.html

# Go build stage
# Patch-pinned to 1.26.6 (not the floating 1.26-alpine tag) so the SHIPPED
# binary is compiled with the Go stdlib CVE fixes (GO-2026-5037/5038/5039
# plus GO-2026-5856/GO-2026-4970, and the 1.26.6 set: GO-2026-5026/5972/6088/
# 6089/6090/6091/6218 across net/http, crypto/tls, html/template, encoding/xml,
# encoding/asn1 and net/url), deterministically, not reliant on Docker Hub
# tag-float + a GOTOOLCHAIN build-time download. Keep in lockstep with
# services/control-plane/go.mod's `go` directive.
FROM golang:1.27.1-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS builder

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
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

LABEL org.opencontainers.image.source="https://github.com/Concord-Voice/Concord-Voice-Alpha"
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
