# Concord MinIO source build

Concord publishes its MinIO server image from an unchanged, exact upstream
source checkout. This recipe describes the inputs embedded in
`ghcr.io/concord-voice/minio:RELEASE.2025-10-15T17-29-55Z` and the companion
corresponding-source artifact at
`ghcr.io/concord-voice/minio-source:RELEASE.2025-10-15T17-29-55Z`.

## Fixed inputs

| Input               | Immutable value                                                                                                         |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Upstream repository | `https://github.com/minio/minio`                                                                                        |
| Upstream tag        | `RELEASE.2025-10-15T17-29-55Z`                                                                                          |
| Upstream commit     | `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a`                                                                              |
| Commit timestamp    | `2025-10-15T17:29:55Z` (`SOURCE_DATE_EPOCH=1760549395`)                                                                 |
| Go builder          | `golang:1.24.8-bookworm@sha256:4ed690d6649d63c312b99a6120025ec79ce3b542968a37da53d6236c7c61a848`                        |
| Runtime base        | `registry.access.redhat.com/ubi9/ubi-micro:9.6@sha256:990002083442f6a93cd3249da32ecb7c3f6be778a1bec3a73a9c17fbc40edc15` |
| Buildx              | `v0.35.0` Linux amd64 asset, SHA-256 `d41ece72044243b4f58b343441ae37446d9c29a7d6b5e11c61847bbcf8f7dfda`                 |
| BuildKit            | `moby/buildkit:v0.31.1@sha256:6b59b7df63a8cb9902736f9ddf7fcff8261613d3e7449b8ea8b7537fc399c03a`                         |
| Runtime platforms   | `linux/amd64`, `linux/arm64`                                                                                            |

No Concord patch, generated-source rewrite, or upstream binary download is
part of this build. Publication must stop if the tag does not resolve to the
listed commit or if the checked-out tree is dirty.

## Reproduce the runtime image

Clone and verify the exact upstream tree:

```bash
git clone https://github.com/minio/minio.git minio-source
git -C minio-source checkout --detach RELEASE.2025-10-15T17-29-55Z
test "$(git -C minio-source rev-parse HEAD)" = \
  "9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a" # pragma: allowlist secret -- public upstream Git commit
SOURCE_STATUS="$(git -C minio-source status --porcelain)"
test -z "$SOURCE_STATUS"
```

Build from that directory, using this Dockerfile by absolute path:

```bash
docker buildx build \
  --platform linux/amd64 \
  --build-arg SOURCE_DATE_EPOCH=1760549395 \
  --load \
  --file /absolute/path/to/repository/infrastructure/docker/minio/Dockerfile \
  --tag concord-minio:RELEASE.2025-10-15T17-29-55Z \
  minio-source
```

For a registry manifest, use `--platform linux/amd64,linux/arm64`,
`--build-arg SOURCE_DATE_EPOCH=1760549395`, and an image exporter with
`push=true,rewrite-timestamp=true`. The rewrite option normalizes file and
directory timestamps inside generated layers. `SOURCE_DATE_EPOCH` alone only
normalizes image metadata. The repository publisher uses the pinned Buildx and
BuildKit versions above, writes run-scoped staging artifacts, tests both native
architectures by digest, and only then promotes the stable runtime/source tag
pair. Recovery accepts a matching pair or a matching source-only partial. A
runtime-only stable tag stops publication even when its digest matches. GHCR
does not document registry-enforced immutable tags, so the captured digests—not
the discovery tags—are
authoritative. Package write access is restricted to the controlled publisher
repository, and corresponding source is promoted and verified before the
runtime tag.

The Dockerfile runs the pinned Go toolchain on Buildx's native build platform
and cross-compiles the root MinIO package with `GOTOOLCHAIN=local`,
`CGO_ENABLED=0`, the Buildx target OS and architecture, `-mod=readonly`,
`-buildvcs=false`, `-tags=kqueue`, `-trimpath`, and fixed upstream release
linker metadata. It copies only the resulting server binary, the builder CA
trust bundle, and unchanged upstream `LICENSE`, `NOTICE`, and `CREDITS` files
into the runtime image.

## Verify the output

```bash
docker run --rm \
  concord-minio:RELEASE.2025-10-15T17-29-55Z \
  --version

docker image inspect \
  concord-minio:RELEASE.2025-10-15T17-29-55Z \
  --format '{{json .Config.Labels}}'
```

The first output line must be:

```text
minio version RELEASE.2025-10-15T17-29-55Z (commit-id=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a)
```

The image runs as UID 0 for compatibility with existing Concord volumes,
directly executes `/usr/bin/minio`, declares `/data`, exposes 9000/9001, and
uses SIGTERM for shutdown. Runtime `mc`, `curl`, and a shell entrypoint are not
included.

## Corresponding source

The companion OCI artifact contains:

- `minio-source.tar.gz`, produced from the exact commit with `git archive` and
  normalized `gzip -n` output
- `minio-source.tar.gz.sha256`
- `runtime-manifest-digest.txt`, binding the source artifact to its runtime
- this Dockerfile
- this `SOURCE-BUILD.md` recipe

The source archive plus these build files are the inputs used to produce the
runtime binary. MinIO's upstream license materials are also copied unchanged
into `/licenses` in the runtime image. On initial package creation, GHCR defaults
the packages to private and Concord changes them to public only after required
legal review. Concord makes and anonymously verifies the corresponding-source
package public before it exposes the runtime package. Because package visibility
cannot return from public to private, every later release requires recorded
legal approval before publisher dispatch.
