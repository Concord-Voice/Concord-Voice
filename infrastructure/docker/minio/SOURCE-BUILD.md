# Concord MinIO source build

This recipe records the exact upstream build and the bounded dependency
derivative for issues #3441 and #3469. The general source policy remains
unchanged; the derivative changes only the dependency manifests for the exact
upstream release recorded below. The existing upstream-only image is
identified by
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

These inputs describe the already-published release: no Concord patch,
generated-source rewrite, or upstream binary download was part of that build.
Publication must stop if the tag does not resolve to the listed commit or if
the checked-out tree is dirty.

## Previously published upstream-only runtime

The runtime described in the next block is the previously published upstream-only
digest, not the dependency derivative. To reproduce it, use the historical
`Dockerfile` and
recipe from that release's corresponding-source artifact at the source digest
in the refresh runbook. The `Dockerfile` in this recipe builds the dependency
derivative and cannot reproduce the older runtime.

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
  --file /absolute/path/to/published-source-artifact/Dockerfile \
  --tag concord-minio:RELEASE.2025-10-15T17-29-55Z \
  minio-source
```

For a registry manifest, use `--platform linux/amd64,linux/arm64`,
`--build-arg SOURCE_DATE_EPOCH=1760549395`, and an image exporter with
`push=true,rewrite-timestamp=true`. The rewrite option normalizes file and
directory timestamps inside generated layers. `SOURCE_DATE_EPOCH` alone only
normalizes image metadata. The repository publisher uses the pinned Buildx
listed above and the derivative BuildKit pin below, writes run-scoped staging
artifacts, tests both native architectures by digest, and only then promotes
the stable runtime/source tag pair. Recovery accepts a matching pair or a
matching source-only partial. A
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

## Dependency derivative for #3441 and #3469

This section specifies the dependency-only derivative of the exact upstream
release above. It changes only `go.mod` and `go.sum`; no application source is
changed. Publication, consumer updates, and live cutover each remain subject to
their separate gates in the refresh runbook.

| Input or output | Exact value |
| --- | --- |
| Upstream tag | `RELEASE.2025-10-15T17-29-55Z` |
| Upstream commit | `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a` |
| Dependencies | `github.com/rabbitmq/amqp091-go v1.13.0`; `google.golang.org/grpc v1.83.2` |
| Go builder | `golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d` |
| BuildKit | `moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3` |
| Derivative tree | `7445ef60bbf408e35d98bbeb1833c93f71a44708` |
| Derivative commit | `823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a` |
| Derivative tag | `RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62` |
| Patch SHA-256 | `c8dafb0b2ef7bfcf1dbb5a567307c7797069a52dde502884b6ebd63c2a794ba8` |
| `go.mod` SHA-256 | `2a6338217a1dfeb3460e12b6bdc5b8d0b8f09eaf621abe6bce956114a57ba7ed` |
| `go.sum` SHA-256 | `4ee8f78b8f62146ead3471678bfd12cc8d0232f839ab26bd3d3b0827e0deb2ea` |
| Source archive SHA-256 | `7a8f034904c79ca656710e3c63804df388b0c6be6e74dd91524ee69d1dac8b29` |

The pinned patch uses the repository whitespace hook's normalized blank context lines. It applies to the same manifests and reproduces the same derivative tree, commit, and Linux archive.

The archive hash is for the Linux GNU gzip 1.12 output used by the publisher;
the uncompressed `git archive` tar SHA-256 is
`03ccda45d9ea3ce88f327bb1b0a9338a671dec2edbf6d33b60a25e33591c2b7c`.
The commit uses parent `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a`, subject
`Concord dependency remediation for #3441 and #3469`, and fixed author and
committer `Concord Voice <build@concordvoice.com>` at
`2026-09-26T00:00:00Z`. The upstream release time and
`SOURCE_DATE_EPOCH=1760549395` remain compatibility timestamps; they do not
identify the derivative source. Runtime base, Buildx, ports, credentials,
volume, licenses, and compile flags remain at their existing pins; the
derivative uses the BuildKit pin listed above.

From the repository root, prepare the source in a disposable directory. Keep
the checked-in `infrastructure/docker/minio/dependencies.patch` and the
workflow's expected hashes alongside the repository checkout.

```bash
set -euo pipefail
test "$(gzip --version | head -n 1)" = 'gzip 1.12'
git clone https://github.com/minio/minio.git minio-source
git -C minio-source checkout --detach RELEASE.2025-10-15T17-29-55Z
test "$(git -C minio-source rev-parse HEAD)" = \
  "9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a" # pragma: allowlist secret -- public upstream Git commit
SOURCE_STATUS="$(git -C minio-source status --porcelain --untracked-files=all --ignored)"
test -z "$SOURCE_STATUS"
printf '%s  %s\n' \
  c8dafb0b2ef7bfcf1dbb5a567307c7797069a52dde502884b6ebd63c2a794ba8 \
  infrastructure/docker/minio/dependencies.patch | sha256sum -c -
git -C minio-source apply --check --whitespace=error \
  "$PWD/infrastructure/docker/minio/dependencies.patch"
git -C minio-source apply --whitespace=error \
  "$PWD/infrastructure/docker/minio/dependencies.patch"
PATCHED_STATUS="$(git -C minio-source status --porcelain --untracked-files=all --ignored)"
test "$PATCHED_STATUS" = $' M go.mod\n M go.sum'
test "$(git -C minio-source diff --name-only --no-renames)" = $'go.mod\ngo.sum'
printf '%s  %s\n' \
  2a6338217a1dfeb3460e12b6bdc5b8d0b8f09eaf621abe6bce956114a57ba7ed minio-source/go.mod \
  4ee8f78b8f62146ead3471678bfd12cc8d0232f839ab26bd3d3b0827e0deb2ea minio-source/go.sum \
  | sha256sum -c -
git -C minio-source add -- go.mod go.sum
DERIVATIVE_TREE="$(git -C minio-source write-tree)"
test "$DERIVATIVE_TREE" = 7445ef60bbf408e35d98bbeb1833c93f71a44708
DERIVATIVE_COMMIT="$(GIT_AUTHOR_NAME='Concord Voice' GIT_AUTHOR_EMAIL='build@concordvoice.com' \
  GIT_AUTHOR_DATE='2026-09-26T00:00:00Z' GIT_COMMITTER_NAME='Concord Voice' \
  GIT_COMMITTER_EMAIL='build@concordvoice.com' GIT_COMMITTER_DATE='2026-09-26T00:00:00Z' \
  git -C minio-source -c commit.gpgsign=false commit-tree "$DERIVATIVE_TREE" \
    -p 9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a \
    -m 'Concord dependency remediation for #3441 and #3469')"
test "$DERIVATIVE_COMMIT" = 823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a
test "$(git -C minio-source show -s --format=%P "$DERIVATIVE_COMMIT")" = \
  9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
git -C minio-source archive --format=tar "$DERIVATIVE_COMMIT" | gzip -n > minio-source.tar.gz
printf '%s  %s\n' \
  7a8f034904c79ca656710e3c63804df388b0c6be6e74dd91524ee69d1dac8b29 \
  minio-source.tar.gz | sha256sum -c -
mkdir minio-build-context
tar -xzf minio-source.tar.gz -C minio-build-context
sha256sum minio-source.tar.gz > minio-source.tar.gz.sha256
cp infrastructure/docker/minio/dependencies.patch dependencies.patch
sha256sum dependencies.patch > dependencies.patch.sha256
printf '%s  %s\n' \
  c8dafb0b2ef7bfcf1dbb5a567307c7797069a52dde502884b6ebd63c2a794ba8 \
  dependencies.patch | sha256sum -c -
printf '%s  %s\n' \
  2a6338217a1dfeb3460e12b6bdc5b8d0b8f09eaf621abe6bce956114a57ba7ed minio-build-context/go.mod \
  > go.mod.sha256
printf '%s  %s\n' \
  4ee8f78b8f62146ead3471678bfd12cc8d0232f839ab26bd3d3b0827e0deb2ea minio-build-context/go.sum \
  > go.sum.sha256
cat > source-provenance.txt <<'EOF'
upstream_tag=RELEASE.2025-10-15T17-29-55Z
upstream_commit=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
derivative_tag=RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62
derivative_commit=823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a
derivative_tree=7445ef60bbf408e35d98bbeb1833c93f71a44708
patch_sha256=c8dafb0b2ef7bfcf1dbb5a567307c7797069a52dde502884b6ebd63c2a794ba8
go_mod_sha256=2a6338217a1dfeb3460e12b6bdc5b8d0b8f09eaf621abe6bce956114a57ba7ed
go_sum_sha256=4ee8f78b8f62146ead3471678bfd12cc8d0232f839ab26bd3d3b0827e0deb2ea
source_archive_sha256=7a8f034904c79ca656710e3c63804df388b0c6be6e74dd91524ee69d1dac8b29
go_builder=golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
EOF
cp infrastructure/docker/minio/Dockerfile Dockerfile
cp infrastructure/docker/minio/SOURCE-BUILD.md SOURCE-BUILD.md
```

Build both architectures from the extracted archive context with the repository
Dockerfile. The derivative BuildKit pin listed above and the existing
runtime-base pin remain in force.

```bash
for PLATFORM in linux/amd64 linux/arm64; do
  ARCH="${PLATFORM##*/}"
  docker buildx build \
    --platform "$PLATFORM" \
    --build-arg SOURCE_DATE_EPOCH=1760549395 \
    --build-arg DERIVATIVE_COMMIT=823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a \
    --build-arg DERIVATIVE_TAG=RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62 \
    --build-arg UPSTREAM_COMMIT=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a \
    --load \
    --file "$PWD/infrastructure/docker/minio/Dockerfile" \
    --tag "concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62-$ARCH" \
    minio-build-context
done
```

The Dockerfile requires `go1.26.8` and builds with `GOTOOLCHAIN=local`,
`-mod=readonly`, `CGO_ENABLED=0`, `-buildvcs=false`, `-tags=kqueue`, and
`-trimpath`. The built binaries reported Go 1.26.8, AMQP 1.13.0, and gRPC
1.83.2; their SHA-256 values are `330e711d2e4dfeb5e1ed12c233a77b4af1c3a2038c72ed51bafcff9b8b9cfaab`
(amd64) and `e812643388f7d2233f9de49939b5c87ae3ed3fab77d3d0918cd1faa1c6edd3b8`
(arm64). The shared Trivy inventory comparison found zero new package/advisory
pairs and 47 cleared pairs, with 31 residual pairs (13 high, 15 medium, 2 low,
1 unknown). This is inventory evidence, not a reachability result or a clean
scan claim. The refresh runbook defines the separate publication and deployment
gates.

For each locally built image, check `--version` for derivative tag
`RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62` and full commit
`823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a`. The source archive is the build
context; do not build from the checkout directory.

## Verify the derivative output

```bash
EXPECTED_VERSION_LINE='minio version RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62 (commit-id=823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a)'
for ARCH in amd64 arm64; do
  IMAGE="concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62-$ARCH"
  VERSION_OUTPUT="$(docker run --rm "$IMAGE" --version)"
  test "$(printf '%s\n' "$VERSION_OUTPUT" | sed -n '1p')" = "$EXPECTED_VERSION_LINE"
done

docker image inspect \
  concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62-amd64 \
  --format '{{json .Config.Labels}}'
```

Both images' first output line must be:

```text
minio version RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62 (commit-id=823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a)
```

Local isolated validation passed the full S3 lifecycle, HEAD, and object-hash
checks on native arm64, and its server-info response reported version
`2025-10-15T17:29:55Z`, commit ID
`823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a`, and state `online`. Local amd64
execution was emulated. The publisher workflow must perform its native amd64
verification on the configured runner.

The image runs as UID 0 for compatibility with existing Concord volumes,
directly executes `/usr/bin/minio`, declares `/data`, exposes 9000/9001, and
uses SIGTERM for shutdown. Runtime `mc`, `curl`, and a shell entrypoint are not
included.

## Corresponding source

The upstream-only release's corresponding-source OCI artifact contains:

- `minio-source.tar.gz`, produced from the exact commit with `git archive` and
  normalized `gzip -n` output
- `minio-source.tar.gz.sha256`
- `runtime-manifest-digest.txt`, binding the source artifact to its runtime
- this Dockerfile
- this `SOURCE-BUILD.md` recipe

The corresponding-source artifact for the derivative must include
`dependencies.patch`, its checksum, `go.mod.sha256`, `go.sum.sha256`,
`source-provenance.txt`, and the generated `runtime-manifest-digest.txt`.
The provenance record binds the upstream and derivative tag/commit/tree, patch
and manifest hashes, archive hash, and Go builder. The archive is extracted to
a fresh `minio-build-context`, which is the context passed to the runtime
build; the OCI artifact's runtime digest binds those source bytes to that
runtime. The refresh runbook defines the separate publication and deployment
gates.

The source archive plus these build files are the inputs used to produce the
runtime binary. MinIO's upstream license materials are also copied unchanged
into `/licenses` in the runtime image. On initial package creation, GHCR defaults
the packages to private and Concord changes them to public only after required
legal review. Concord makes and anonymously verifies the corresponding-source
package public before it exposes the runtime package. Because package visibility
cannot return from public to private, every later release requires recorded
legal approval before publisher dispatch.
