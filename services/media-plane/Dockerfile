# Build stage
FROM ghcr.io/concord-voice/node-buildtools:24-alpine-2026-09-04@sha256:f94d703b688d942e89e6225977c0110560a9f2b5095377d209aca10e4cf5df2a AS builder

WORKDIR /app

# stubs/ must precede npm ci — package.json overrides node-domexception → ./stubs/node-domexception
COPY package*.json ./
COPY stubs ./stubs
RUN npm ci

# Copy source code
COPY . .

# Build TypeScript
RUN npm run build

# Drop dev deps after build, then verify the `file:` override symlink survived prune.
# Defense against future npm versions changing prune semantics in a way that
# would silently break the node-domexception override (per #1083 → #1097).
RUN npm prune --omit=dev \
 && test -L node_modules/node-domexception \
 && test -d node_modules/node-domexception/ \
 && npm ls node-domexception

# Production stage — NO compilation toolchain.
# mediasoup-worker (native ELF) was compiled in the builder stage above.
# node_modules carries the worker binary + pruned prod deps + the node-domexception override symlink.
# stubs/ must persist at runtime — fetch-blob resolves the symlink at require time.
# See [internal]rules/media-plane.md "Docker Build Context Invariant" for the discipline.
# Pin the runtime independently so deploy rebuilds cannot reuse a stale floating Node base.
FROM node:24-alpine@sha256:e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf

LABEL org.opencontainers.image.source="https://github.com/Concord-Voice/Concord-Voice-Alpha"
LABEL org.opencontainers.image.description="Concord Voice media-plane (mediasoup SFU)"
LABEL org.opencontainers.image.licenses="LicenseRef-CVSL-1.0"

WORKDIR /app

COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/stubs ./stubs
COPY --from=builder /app/package*.json ./

# Run as non-root user
RUN addgroup -S appuser && adduser -S appuser -G appuser
RUN chown -R appuser:appuser /app
USER appuser

# Expose ports
EXPOSE 3000
EXPOSE 40000-49999/udp

# Run the application
CMD ["node", "dist/index.js"]
