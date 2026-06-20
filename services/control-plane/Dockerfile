# Build stage
# Patch-pinned to 1.26.4 (not the floating 1.26-alpine tag) so the SHIPPED
# binary is compiled with the Go stdlib CVE fixes (GO-2026-5037/5038/5039),
# deterministically — not reliant on Docker Hub tag-float + a GOTOOLCHAIN
# build-time download. Keep in lockstep with services/control-plane/go.mod's
# `go` directive.
FROM golang:1.26.4-alpine AS builder

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

# Final stage
FROM alpine:3.21

LABEL org.opencontainers.image.source="https://github.com/markdrogersjr/Concord"
LABEL org.opencontainers.image.description="Concord Voice control-plane service"
LABEL org.opencontainers.image.licenses="LicenseRef-CVSL-1.0"

RUN apk --no-cache add ca-certificates \
    && addgroup -S appuser && adduser -S appuser -G appuser

# Run as non-root user
WORKDIR /home/appuser

# Copy binaries from builder
COPY --from=builder /app/main .
COPY --from=builder /app/migrate-media .

# Copy migrations
COPY --from=builder /app/migrations ./migrations

USER appuser

# Expose port
EXPOSE 8080

# Run the binary
CMD ["./main"]
