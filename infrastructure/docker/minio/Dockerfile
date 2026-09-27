FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG DERIVATIVE_COMMIT
ARG DERIVATIVE_TAG
ARG UPSTREAM_COMMIT
WORKDIR /src
COPY . .
ENV SOURCE_DATE_EPOCH=1760549395
RUN : "${DERIVATIVE_COMMIT:?}" "${DERIVATIVE_TAG:?}" "${UPSTREAM_COMMIT:?}" && \
    test "$(go env GOVERSION)" = "go1.26.8" && \
    mkdir -p /out && \
    LDFLAGS="-s -w -X github.com/minio/minio/cmd.Version=2025-10-15T17:29:55Z -X github.com/minio/minio/cmd.CopyrightYear=2025 -X github.com/minio/minio/cmd.ReleaseTag=$DERIVATIVE_TAG -X github.com/minio/minio/cmd.CommitID=$DERIVATIVE_COMMIT -X github.com/minio/minio/cmd.ShortCommitID=$(printf '%.12s' "$DERIVATIVE_COMMIT") -X github.com/minio/minio/cmd.GOPATH=/go -X github.com/minio/minio/cmd.GOROOT=/usr/local/go" && \
    GOTOOLCHAIN=local CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -mod=readonly -buildvcs=false -tags=kqueue -trimpath -ldflags "$LDFLAGS" -o /out/minio .

FROM registry.access.redhat.com/ubi9/ubi-micro:9.6@sha256:990002083442f6a93cd3249da32ecb7c3f6be778a1bec3a73a9c17fbc40edc15 AS runtime
ARG DERIVATIVE_COMMIT
ARG DERIVATIVE_TAG
ARG UPSTREAM_COMMIT
COPY --from=builder /out/minio /usr/bin/minio
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /src/LICENSE /licenses/LICENSE
COPY --from=builder /src/NOTICE /licenses/NOTICE
COPY --from=builder /src/CREDITS /licenses/CREDITS
LABEL org.opencontainers.image.title="MinIO"
LABEL org.opencontainers.image.source="https://github.com/minio/minio"
LABEL org.opencontainers.image.version="$DERIVATIVE_TAG"
LABEL org.opencontainers.image.revision="$DERIVATIVE_COMMIT"
LABEL org.opencontainers.image.licenses="AGPL-3.0-or-later"
LABEL org.opencontainers.image.created="2025-10-15T17:29:55Z"
LABEL org.opencontainers.image.documentation="https://github.com/Concord-Voice/Concord-Voice/blob/main/infrastructure/docker/minio/SOURCE-BUILD.md"
LABEL io.concordvoice.minio.upstream-tag="RELEASE.2025-10-15T17-29-55Z"
LABEL io.concordvoice.minio.upstream-commit="$UPSTREAM_COMMIT"
USER 0
EXPOSE 9000 9001
VOLUME ["/data"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/minio"]
CMD ["server", "/data", "--console-address", ":9001"]
