# Concord MinIO source build

This recipe records the exact upstream build and the bounded dependency
derivative for issues #3441, #3469, #3527, #3528 and #3530–#3534. The general
source policy remains unchanged; the derivative changes only the dependency
manifests for the exact upstream release recorded below. The existing upstream-only image is
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

## Dependency derivative for #3441, #3469, #3527, #3528 and #3530–#3534

This section specifies the dependency-only derivative of the exact upstream
release above. It changes only `go.mod` and `go.sum`; no application source is
changed. Publication, consumer updates, and live cutover each remain subject to
their separate gates in the refresh runbook.

The most recently recorded publication is
`RELEASE.2025-10-15T17-29-55Z.CONCORD.88c88a5bea7d`, with its corresponding
source, as recorded in the internal development reference omitted.
The shared Compose consumer still pins `823f9aa2bd62`; publication alone did
not change that consumer. The `ee1b8c5b1ed7` recipe below is an unpublished
intermediate Thrift correction. The planned #3716 compiler correction must
produce its own reviewed final identity before the next separately authorized
publication; this intermediate recipe does not request publication.

| Input or output | Exact value |
| --- | --- |
| Upstream tag | `RELEASE.2025-10-15T17-29-55Z` |
| Upstream commit | `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a` |
| Module targets | `github.com/rabbitmq/amqp091-go v1.13.0`; `google.golang.org/grpc v1.83.2`; `github.com/apache/thrift v0.25.0`; `github.com/buger/jsonparser v1.1.2`; `github.com/eclipse/paho.mqtt.golang v1.5.1`; `github.com/Azure/go-ntlmssp v0.1.1`; `go.mongodb.org/mongo-driver v1.17.7`; `golang.org/x/crypto v0.56.0`; `filippo.io/edwards25519 v1.1.1`; `github.com/klauspost/compress v1.18.7`; `go.etcd.io/etcd/client/pkg/v3 v3.5.33` |
| Directive after tidy | `go 1.26.0`, `toolchain go1.26.8` |
| Deliberately unchanged | `github.com/prometheus/prometheus v0.303.0`; `go.opentelemetry.io/otel/sdk v1.44.0` |
| Go builder | `golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d` |
| BuildKit | `moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3` |
| Derivative tree | `e675e3cd96bb8d5ab5487a57cf0eecd214bcbc31` |
| Derivative commit | `ee1b8c5b1ed7e918684dfd3512e75cf5112ac48a` |
| Derivative tag | `RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7` |
| Patch SHA-256 | `ec5b3f0446205d3b28b5f7c00dfe5daae01950dc66da98e9388f12cd54903a30` |
| `go.mod` SHA-256 | `2af356341df8fc8465291437580cb39e6464b2cb90b43eb0b7ae6c229e457db0` |
| `go.sum` SHA-256 | `b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1` |
| Source archive SHA-256 | `9821311828507968de503027572758c5a7af1f63eb65e5db4e40ab6bca64a0dd` |

The pinned patch uses the repository whitespace hook's normalized blank context lines. It applies to the same manifests and reproduces the same derivative tree, commit, and Linux archive.

The archive hash is for the Linux GNU gzip 1.12 output used by the publisher;
the uncompressed `git archive` tar SHA-256 is
`0bdf7d8c616818f77d3349a23d043905ab17962f342d30d56b64638c1ed855ca`.
The commit uses parent `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a`, subject
`Concord dependency remediation for #3441 #3469 #3527 #3528 #3530-#3534`, and
fixed author and committer `Concord Voice <build@concordvoice.com>` at
`2026-10-16T00:00:00Z`. The upstream release time and
`SOURCE_DATE_EPOCH=1760549395` remain compatibility timestamps; they do not
identify the derivative source. Runtime base, Buildx, ports, credentials,
volume, licenses, and compile flags remain at their existing pins; the
derivative uses the BuildKit pin listed above.

The patch was generated, not hand-edited. Inside the pinned Go builder, with
`GOTOOLCHAIN=local`, on a disposable clean upstream clone:

```bash
go mod edit -go=1.25.0 -toolchain=go1.26.8
go get github.com/rabbitmq/amqp091-go@v1.13.0 google.golang.org/grpc@v1.83.2 \
  github.com/apache/thrift@v0.25.0 github.com/buger/jsonparser@v1.1.2 \
  github.com/eclipse/paho.mqtt.golang@v1.5.1 github.com/Azure/go-ntlmssp@v0.1.1 \
  go.mongodb.org/mongo-driver@v1.17.7 golang.org/x/crypto@v0.56.0 filippo.io/edwards25519@v1.1.1 \
  github.com/klauspost/compress@v1.18.7 go.etcd.io/etcd/client/pkg/v3@v3.5.33
go mod tidy && go mod verify
```

`go mod tidy` raises the directive to `go 1.26.0`, because `golang.org/x/crypto
v0.56.0` declares it. Only `go.mod` and `go.sum` differ afterwards.

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
  ec5b3f0446205d3b28b5f7c00dfe5daae01950dc66da98e9388f12cd54903a30 \
  infrastructure/docker/minio/dependencies.patch | sha256sum -c -
git -C minio-source apply --check --whitespace=error \
  "$PWD/infrastructure/docker/minio/dependencies.patch"
git -C minio-source apply --whitespace=error \
  "$PWD/infrastructure/docker/minio/dependencies.patch"
PATCHED_STATUS="$(git -C minio-source status --porcelain --untracked-files=all --ignored)"
test "$PATCHED_STATUS" = $' M go.mod\n M go.sum'
test "$(git -C minio-source diff --name-only --no-renames)" = $'go.mod\ngo.sum'
printf '%s  %s\n' \
  2af356341df8fc8465291437580cb39e6464b2cb90b43eb0b7ae6c229e457db0 minio-source/go.mod \
  b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1 minio-source/go.sum \
  | sha256sum -c -
git -C minio-source add -- go.mod go.sum
DERIVATIVE_TREE="$(git -C minio-source write-tree)"
test "$DERIVATIVE_TREE" = e675e3cd96bb8d5ab5487a57cf0eecd214bcbc31
DERIVATIVE_COMMIT="$(GIT_AUTHOR_NAME='Concord Voice' GIT_AUTHOR_EMAIL='build@concordvoice.com' \
  GIT_AUTHOR_DATE='2026-10-16T00:00:00Z' GIT_COMMITTER_NAME='Concord Voice' \
  GIT_COMMITTER_EMAIL='build@concordvoice.com' GIT_COMMITTER_DATE='2026-10-16T00:00:00Z' \
  git -C minio-source -c commit.gpgsign=false commit-tree "$DERIVATIVE_TREE" \
    -p 9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a \
    -m 'Concord dependency remediation for #3441 #3469 #3527 #3528 #3530-#3534')"
test "$DERIVATIVE_COMMIT" = ee1b8c5b1ed7e918684dfd3512e75cf5112ac48a
test "$(git -C minio-source show -s --format=%P "$DERIVATIVE_COMMIT")" = \
  9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
git -C minio-source archive --format=tar "$DERIVATIVE_COMMIT" | gzip -n > minio-source.tar.gz
printf '%s  %s\n' \
  9821311828507968de503027572758c5a7af1f63eb65e5db4e40ab6bca64a0dd \
  minio-source.tar.gz | sha256sum -c -
mkdir minio-build-context
tar -xzf minio-source.tar.gz -C minio-build-context
sha256sum minio-source.tar.gz > minio-source.tar.gz.sha256
cp infrastructure/docker/minio/dependencies.patch dependencies.patch
sha256sum dependencies.patch > dependencies.patch.sha256
printf '%s  %s\n' \
  ec5b3f0446205d3b28b5f7c00dfe5daae01950dc66da98e9388f12cd54903a30 \
  dependencies.patch | sha256sum -c -
printf '%s  %s\n' \
  2af356341df8fc8465291437580cb39e6464b2cb90b43eb0b7ae6c229e457db0 minio-build-context/go.mod \
  > go.mod.sha256
printf '%s  %s\n' \
  b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1 minio-build-context/go.sum \
  > go.sum.sha256
cat > source-provenance.txt <<'EOF'
upstream_tag=RELEASE.2025-10-15T17-29-55Z
upstream_commit=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
derivative_tag=RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7
derivative_commit=ee1b8c5b1ed7e918684dfd3512e75cf5112ac48a
derivative_tree=e675e3cd96bb8d5ab5487a57cf0eecd214bcbc31
patch_sha256=ec5b3f0446205d3b28b5f7c00dfe5daae01950dc66da98e9388f12cd54903a30
go_mod_sha256=2af356341df8fc8465291437580cb39e6464b2cb90b43eb0b7ae6c229e457db0
go_sum_sha256=b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1
source_archive_sha256=9821311828507968de503027572758c5a7af1f63eb65e5db4e40ab6bca64a0dd
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
    --build-arg DERIVATIVE_COMMIT=ee1b8c5b1ed7e918684dfd3512e75cf5112ac48a \
    --build-arg DERIVATIVE_TAG=RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7 \
    --build-arg UPSTREAM_COMMIT=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a \
    --load \
    --file "$PWD/infrastructure/docker/minio/Dockerfile" \
    --tag "concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7-$ARCH" \
    minio-build-context
done
```

The Dockerfile requires `go1.26.8` and builds with `GOTOOLCHAIN=local`,
`-mod=readonly`, `CGO_ENABLED=0`, `-buildvcs=false`, `-tags=kqueue`, and
`-trimpath`. Rebuilt locally on 2026-10-16, the binaries have SHA-256
`e91bdd478ce9bb36b15d0a27a2f72a636e0bfe30cd6ad85885e69f4c5dfffe41` (amd64) and
`e37deb932c97b9e62f7dfdb44691247bb4845761919f11ab3e87f34568448c21` (arm64).
`go version -m` on both reports Go 1.26.8 and all thirteen module versions: the
eleven module targets in the table above, plus `prometheus` v0.303.0 and
`otel/sdk` v1.44.0, which are unchanged. These binary hashes and metadata are
the local build report for this correction. An independent source replay
reproduced the patch, manifests, tree, commit, tar and gzip hashes; it did not
rebuild the binaries. The internal development reference omitted
preserves the supplied local metadata, package-name counts and Parquet results,
with their provenance and limits, including the clarified `strings` command
and the supplied Thrift 0.24 control. Fresh scanner and runtime evidence remains
outstanding;
they must not be inferred from the historical results below.

### Historical scan evidence: `88c88a5bea7d` (2026-10-01)

The following scan, symbol and default-behavior observations belong to the
published `88c88a5bea7d` binaries, SHA-256
`142923973826fb7d1964ab9835b8ec9f8409b7d61c1010b97f17e3f918152774` (amd64) and
`99a4041b7ef177be0781aa33aa2fc237ba0697cd4d50289f5668ed1b4e1e1445` (arm64).
The publication receipt confirms those binaries matched the local rebuilds.
Their module and symbol checks passed 49 assertions. These are historical
measurements, not a fresh scan or reachability decision for `ee1b8c5b1ed7`.

Residual result. `trivy rootfs` (Trivy 0.74.0) on each architecture reports
exactly six findings, and none of the advisories the eleven module targets fix:
`prometheus` v0.303.0 (CVE-2026-42151, CVE-2026-42154, CVE-2026-40179,
CVE-2026-44903), `otel/sdk` v1.44.0 (CVE-2026-81870), and `x/crypto` v0.56.0
(GO-2026-5932). A symbol check of the Go function-name table in both binaries
finds none of the vulnerable code for these six: `prometheus` `storage/remote`,
`storage/remote/azuread` and `web`, `otel/sdk/trace`, and `x/crypto/openpgp` each
count 0, and so does a canary symbol. Positive controls that must be linked
(`prometheus` `model/histogram` `ZeroBucket`, `otel/sdk/metric`, and `x/crypto`
`chacha20poly1305` `Open`) each count at least 1. The six findings are recorded
in the not-affected record below. This is scanner and symbol evidence, not a
clean-scan claim. The refresh runbook defines the separate publication and
deployment gates.

govulncheck v1.6.0 in `-mode=binary` (vulnerability database updated
2026-09-28) on both binaries reports exactly one vulnerability, GO-2026-5932.
That advisory lists no symbols, so govulncheck reports a package wildcard match
(`openpgp/*`, `openpgp/armor/*`, and so on) and not a called function. The symbol
check finds no `x/crypto/openpgp` name in either binary. GO-2026-6107
(`go.etcd.io/etcd/client/pkg/v3`, unbounded TLS handshake goroutines in
`transport.NewListener` and `transport.NewListenerWithOpts`) and GO-2026-5841
(`github.com/klauspost/compress/s2`, out-of-bounds read in `s2.NewDict`) are no
longer reported. Trivy 0.74.0 did not report either on the build before these
two bumps, so the bumps that fix them rest on the govulncheck result.

The published derivative `RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62`
carries `DefaultGODEBUG=cryptocustomrand=1,tlssecpmlkem=0,urlstrictcolons=0`,
measured with `go version -m`. This derivative's `go 1.26.0` directive, set by
`go mod tidy`, leaves `DefaultGODEBUG` empty, so three Go 1.26 defaults take
effect. `net/url.Parse` rejects hostnames with colons outside a bracketed IPv6
address (`urlstrictcolons`). `crypto/tls` enables the SecP256r1MLKEM768 and
SecP384r1MLKEM1024 post-quantum key exchanges (`tlssecpmlkem`). Most `crypto/...`
APIs ignore a random `io.Reader` supplied by the caller (`cryptocustomrand`).
The S3 lifecycle runs recorded below passed with these defaults.

For each locally built image, check `--version` for derivative tag
`RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7` and full commit
`ee1b8c5b1ed7e918684dfd3512e75cf5112ac48a`. The source archive is the build
context; do not build from the checkout directory.

## Verify the derivative output

```bash
EXPECTED_VERSION_LINE='minio version RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7 (commit-id=ee1b8c5b1ed7e918684dfd3512e75cf5112ac48a)'
for ARCH in amd64 arm64; do
  IMAGE="concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7-$ARCH"
  VERSION_OUTPUT="$(docker run --rm "$IMAGE" --version)"
  test "$(printf '%s\n' "$VERSION_OUTPUT" | sed -n '1p')" = "$EXPECTED_VERSION_LINE"
done

docker image inspect \
  concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7-amd64 \
  --format '{{json .Config.Labels}}'
```

Both images' first output line must be:

```text
minio version RELEASE.2025-10-15T17-29-55Z.CONCORD.ee1b8c5b1ed7 (commit-id=ee1b8c5b1ed7e918684dfd3512e75cf5112ac48a)
```

The publisher runs the S3 lifecycle on native amd64 only. On native arm64 it
checks version metadata, readiness and a clean SIGTERM shutdown. The commands
above describe the new recipe's required version checks; the historical runtime
receipt below does not establish those checks for the new binary hashes.

To cover arm64, the full S3 lifecycle was run locally on 2026-10-01 against the
locally built `88c88a5bea7d` arm64 binary, inside the pinned Go builder on
linux/arm64. It used
the same six requests and expected codes as the publisher: bucket PUT 200,
object PUT 200, HEAD 200, GET 200 with a matching SHA-256, object DELETE 204 and
bucket DELETE 204, then SIGTERM with exit 0. The same run on the then-published
arm64 binary (`823f9aa2bd62`) passed as a control. The builder's curl
7.88.1 predates curl 8.1, which sends the `x-amz-content-sha256` header itself
for SigV4 service `s3`. The local run sends that header explicitly. The
publisher runner's curl 8 sends it automatically.

The image runs as UID 0 for compatibility with existing Concord volumes,
directly executes `/usr/bin/minio`, declares `/data`, exposes 9000/9001, and
uses SIGTERM for shutdown. Runtime `mc`, `curl`, and a shell entrypoint are not
included.

## Not-affected record

`infrastructure/docker/minio/minio.openvex.json` is an OpenVEX v0.2.0 record
for the six residual findings listed above. Each statement is `not_affected`
with justification `vulnerable_code_not_present`, and its impact statement names
the package or symbol that is absent from the binary and the date it was
measured.

Each statement names exactly two products: the amd64 and arm64 platform images
of the derivative still pinned by the Compose consumer,
`RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62`, by digest
(`pkg:oci/minio@sha256:<digest>?repository_url=ghcr.io/concord-voice/minio`). The
products are bound to those digests because a statement measured on one binary
must not suppress its finding on a later image. A product naming the whole
repository did exactly that: a rebuild that links the vulnerable code at the
same module version stayed suppressed, which was reproduced with Trivy. The
GO-2026-5932 statement lists only `x/crypto` v0.55.0, the version measured on
the published image.

The record suppresses nothing on any other image. That includes the
multi-platform index digest and this derivative once it is published. Against
the registry with Trivy 0.74.0, all six findings were suppressed on each child
digest, none on the index digest, and none with a wrong-digest control. When a
new derivative is published, the consumer PR adds that derivative's platform
digests and the measured subcomponent versions, after measuring the published
binaries. For this derivative that includes the `x/crypto` v0.56.0
subcomponent. The steps are in
[the runbook](../../../[internal]refresh-minio-image.md#3a-vex-verification).

Every subcomponent is pinned to an exact module version
(`pkg:golang/<module>@<version>`). Trivy matches only that version, so a
statement lapses on the next dependency bump and the finding returns for a
fresh decision. Never widen a product to the repository, and never widen a
subcomponent to a version-less form.

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
