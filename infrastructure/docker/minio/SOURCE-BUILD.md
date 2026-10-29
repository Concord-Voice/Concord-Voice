# Concord MinIO source build

This recipe records the exact upstream build and the bounded dependency
derivative for issues #3441, #3469, #3527, #3528 and #3530–#3534,
with the Go compiler correction for #3716 and publisher BuildKit correction
for #3482. The general
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

## Current dependency, compiler and builder recipe (#3716 / #3482)

This section specifies the dependency-only derivative of the exact upstream
release above. It changes only `go.mod` and `go.sum`; no application source is
changed. Publication, consumer updates, and live cutover each remain subject to
their separate gates in the refresh runbook.

The most recently recorded publication is
`RELEASE.2025-10-15T17-29-55Z.CONCORD.88c88a5bea7d`, with its corresponding
source, as recorded in the internal development reference omitted.
The shared Compose consumer still pins `823f9aa2bd62`; publication alone did
not change that consumer. The Thrift correction `ee1b8c5b1ed7` is superseded unpublished;
its privileged publisher execution was not performed. The current #3716 recipe
below preserves its thirteen reviewed module versions and changes the Go
compiler to 1.26.10. Its new identity has not been published or consumed. Merge,
release-specific legal approval, publisher dispatch and consumer/cutover approval
remain separate gates. The integrated #3482 change selects BuildKit 0.34.1 for
this recipe without changing the derivative source, compiler, thirteen module
versions, runtime base, Buildx or source archive identity.

| Input or output | Exact value |
| --- | --- |
| Upstream tag | `RELEASE.2025-10-15T17-29-55Z` |
| Upstream commit | `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a` |
| Module targets | `github.com/rabbitmq/amqp091-go v1.13.0`; `google.golang.org/grpc v1.83.2`; `github.com/apache/thrift v0.25.0`; `github.com/buger/jsonparser v1.1.2`; `github.com/eclipse/paho.mqtt.golang v1.5.1`; `github.com/Azure/go-ntlmssp v0.1.1`; `go.mongodb.org/mongo-driver v1.17.7`; `golang.org/x/crypto v0.56.0`; `filippo.io/edwards25519 v1.1.1`; `github.com/klauspost/compress v1.18.7`; `go.etcd.io/etcd/client/pkg/v3 v3.5.33` |
| Manifest directives | `go 1.26.0`, `toolchain go1.26.10` |
| Deliberately unchanged | `github.com/prometheus/prometheus v0.303.0`; `go.opentelemetry.io/otel/sdk v1.44.0` |
| Go builder | `golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af` |
| BuildKit | `moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c` |
| Derivative tree | `6c22e62a4893205c4e7d86a4a702e5cf5bd80fd9` |
| Derivative commit | `512ec76c0c95c9b4947c43f3c820811cae6fa10e` |
| Derivative tag | `RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95` |
| Patch SHA-256 | `9372ea296d67a1c4640ed1bbf94070ab7aa16b9d625361090199fd721281b264` |
| `go.mod` SHA-256 | `9e9094cdd73f3cdaeb888ab7efcb7a18727d9cf238ed2b03bc98fa19671a6704` |
| `go.sum` SHA-256 | `b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1` |
| Source archive SHA-256 | `be5589c09a506ec467619973cfdd03fcfc4a33cdac3b40b01eb77121ce583fed` |

The [BuildKit artifact evidence](../../../[internal]specs/2026-10-25-3482-buildkit-artifact-evidence.md)
records both regular and rootless images on linux/amd64 and linux/arm64.
All eight extracted `buildctl`/`buildkitd` binaries report Go 1.26.10 and
containerd/v2 2.4.1. Positive binary scans omit CVE-2026-53493 and
CVE-2026-95838, but retain six findings per client and seven per daemon,
including HIGH x/net findings and HIGH go-archive findings in the daemons.
Residual security disposition remains required; these results are neither a
clean full-image scan nor runtime evidence. Execution of the final publisher
recipe with Buildx 0.35.0 and this BuildKit remains **UNVERIFIED**. The managed
rootless builder needs its separate native admission/network proof before host
activation. Earlier build outputs and publication receipts do not validate
either upgraded builder. The proposed final #3716/#3482 boundary in `REVIEW.md`
requires explicit security/deployment acceptance for the final integrated head.

The pinned patch uses the repository whitespace hook's normalized blank context lines. It applies to the same manifests and reproduces the same derivative tree, commit, and Linux archive.

The final archive hash was measured with Linux GNU gzip 1.13 and reproduced
with GNU gzip 1.12. An independent pristine replay reproduced the final source
identity. The publisher must reproduce this exact checksum.
The uncompressed `git archive` tar SHA-256 is
`dd2a99e2aafaaa1f5c30857f7f5f15704e0a7cd835f9337b4eab5108b82a71a9`.
The commit uses parent `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a`, subject
`Concord dependency and compiler remediation for #3441 #3469 #3527 #3528 #3530-#3534 #3716`, and
fixed author and committer `Concord Voice <build@concordvoice.com>` at
`2026-10-25T00:00:00Z`. The upstream release time and
`SOURCE_DATE_EPOCH=1760549395` remain compatibility timestamps; they do not
identify the derivative source. Runtime base, Buildx, ports, credentials,
volume, licenses, and compile flags remain at their existing pins; the
derivative uses the BuildKit pin listed above.

The patch was regenerated from the exact upstream checkout after applying the
reviewed `ee1b8c5b1ed7` manifest patch. Under the pinned Go 1.26.10 compiler:

```bash
GOTOOLCHAIN=local go mod edit -toolchain=go1.26.10
```

The resulting `go.mod` differs from that intermediate only in the toolchain
line; `go 1.26.0`, all module requirements and the entire `go.sum` are unchanged.
No `go get` or `go mod tidy` was used for this compiler continuation. The
cumulative upstream-relative patch still modifies only `go.mod` and `go.sum`.
Blank context lines are normalized by removing their single context space,
as in the existing repository whitespace convention.

From the repository root, prepare the source in a disposable directory. Keep
the checked-in `infrastructure/docker/minio/dependencies.patch` and the
workflow's expected hashes alongside the repository checkout.

```bash
set -euo pipefail
gzip --version | sed -n '1p' # record compressor provenance; the checksum below is authoritative
git clone https://github.com/minio/minio.git minio-source
git -C minio-source checkout --detach RELEASE.2025-10-15T17-29-55Z
test "$(git -C minio-source rev-parse HEAD)" = \
  "9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a" # pragma: allowlist secret -- public upstream Git commit
SOURCE_STATUS="$(git -C minio-source status --porcelain --untracked-files=all --ignored)"
test -z "$SOURCE_STATUS"
printf '%s  %s\n' \
  9372ea296d67a1c4640ed1bbf94070ab7aa16b9d625361090199fd721281b264 \
  infrastructure/docker/minio/dependencies.patch | sha256sum -c -
git -C minio-source apply --check --whitespace=error \
  "$PWD/infrastructure/docker/minio/dependencies.patch"
git -C minio-source apply --whitespace=error \
  "$PWD/infrastructure/docker/minio/dependencies.patch"
PATCHED_STATUS="$(git -C minio-source status --porcelain --untracked-files=all --ignored)"
test "$PATCHED_STATUS" = $' M go.mod\n M go.sum'
test "$(git -C minio-source diff --name-only --no-renames)" = $'go.mod\ngo.sum'
printf '%s  %s\n' \
  9e9094cdd73f3cdaeb888ab7efcb7a18727d9cf238ed2b03bc98fa19671a6704 minio-source/go.mod \
  b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1 minio-source/go.sum \
  | sha256sum -c -
git -C minio-source add -- go.mod go.sum
DERIVATIVE_TREE="$(git -C minio-source write-tree)"
test "$DERIVATIVE_TREE" = 6c22e62a4893205c4e7d86a4a702e5cf5bd80fd9
DERIVATIVE_COMMIT="$(GIT_AUTHOR_NAME='Concord Voice' GIT_AUTHOR_EMAIL='build@concordvoice.com' \
  GIT_AUTHOR_DATE='2026-10-25T00:00:00Z' GIT_COMMITTER_NAME='Concord Voice' \
  GIT_COMMITTER_EMAIL='build@concordvoice.com' GIT_COMMITTER_DATE='2026-10-25T00:00:00Z' \
  git -C minio-source -c commit.gpgsign=false commit-tree "$DERIVATIVE_TREE" \
    -p 9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a \
    -m 'Concord dependency and compiler remediation for #3441 #3469 #3527 #3528 #3530-#3534 #3716')"
test "$DERIVATIVE_COMMIT" = 512ec76c0c95c9b4947c43f3c820811cae6fa10e
test "$(git -C minio-source show -s --format=%P "$DERIVATIVE_COMMIT")" = \
  9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
git -C minio-source archive --format=tar "$DERIVATIVE_COMMIT" | gzip -n > minio-source.tar.gz
printf '%s  %s\n' \
  be5589c09a506ec467619973cfdd03fcfc4a33cdac3b40b01eb77121ce583fed \
  minio-source.tar.gz | sha256sum -c -
mkdir minio-build-context
tar -xzf minio-source.tar.gz -C minio-build-context
sha256sum minio-source.tar.gz > minio-source.tar.gz.sha256
cp infrastructure/docker/minio/dependencies.patch dependencies.patch
sha256sum dependencies.patch > dependencies.patch.sha256
printf '%s  %s\n' \
  9372ea296d67a1c4640ed1bbf94070ab7aa16b9d625361090199fd721281b264 \
  dependencies.patch | sha256sum -c -
printf '%s  %s\n' \
  9e9094cdd73f3cdaeb888ab7efcb7a18727d9cf238ed2b03bc98fa19671a6704 minio-build-context/go.mod \
  > go.mod.sha256
printf '%s  %s\n' \
  b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1 minio-build-context/go.sum \
  > go.sum.sha256
cat > source-provenance.txt <<'EOF'
upstream_tag=RELEASE.2025-10-15T17-29-55Z
upstream_commit=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
derivative_tag=RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95
derivative_commit=512ec76c0c95c9b4947c43f3c820811cae6fa10e
derivative_tree=6c22e62a4893205c4e7d86a4a702e5cf5bd80fd9
patch_sha256=9372ea296d67a1c4640ed1bbf94070ab7aa16b9d625361090199fd721281b264
go_mod_sha256=9e9094cdd73f3cdaeb888ab7efcb7a18727d9cf238ed2b03bc98fa19671a6704
go_sum_sha256=b2917a1758ed5ac48c97ea6b66e9dd1c45f0a4a594ffe1afa14475e26dfadec1
source_archive_sha256=be5589c09a506ec467619973cfdd03fcfc4a33cdac3b40b01eb77121ce583fed
go_builder=golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af
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
    --build-arg DERIVATIVE_COMMIT=512ec76c0c95c9b4947c43f3c820811cae6fa10e \
    --build-arg DERIVATIVE_TAG=RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95 \
    --build-arg UPSTREAM_COMMIT=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a \
    --load \
    --file "$PWD/infrastructure/docker/minio/Dockerfile" \
    --tag "concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95-$ARCH" \
    minio-build-context
done
```

The current Dockerfile requires `go1.26.10`. Final native amd64 and cross-built
arm64 binaries have SHA-256
`fc6d516f633474d452c4b1a70f53ad8aa724fbbc95591f2896db423cbb39e672` and
`c3197fd2b0fb6495f4b6995c1915106f5a6c4d045b6fe49508e59668b66f8327`,
respectively. Both report Go 1.26.10 and all thirteen expected module versions,
without replacements or a range-limit override. The native amd64 CLI reports
the final derivative identity. Its bounded console Range check, disposable
S3 lifecycle, Parquet probe and s3select tests passed. Native arm64 execution
and protected-main publisher/image attestations remain outstanding.

Trivy 0.74.0 with database updated `2026-10-25T01:20:33.190884269Z` reported
twelve findings on each final binary; CVE-2026-78667 and CVE-2026-111943 were
absent. The residuals comprise six findings represented in historical
digest-bound VEX, AMQP CVE-2026-107386 and five x/net HTTP/2 findings. Historical
VEX and configuration-dependent dispositions do not attest these new binaries
or the current fleet. govulncheck remains incomplete because its database
request returned HTTP Forbidden. String-name controls passed, but do not
establish call-graph reachability. The bounded Range harness and its
module-directive requirement are documented in
[`scripts/tests/minio/README.md`](../../../scripts/tests/minio/README.md).

### Superseded compiler trial: `c250402a45d2` (2026-10-25)

The unpublished Go 1.26.9 trial produced native amd64 and cross-built arm64
binaries with SHA-256
`120001814493933f8309064b9017722acab37b4d167dae8f76dc808c4d527982` and
`5a525309d861a3d5f3e6e9f3d4dd02a38c49bcfa77cf558f4867a9620ab202d5`,
respectively. Both reported Go 1.26.9 and all thirteen expected module versions,
with no replacements or DefaultGODEBUG range override. Its native console,
S3 lifecycle and Parquet checks passed. Fresh scanning identified
CVE-2026-111943, prompting the final Go 1.26.10 refinement. These observations
remain bound to the trial; they do not validate the final recipe's binaries.

### Historical intermediate evidence: `ee1b8c5b1ed7` (2026-10-16)

The following paragraph describes the superseded, unpublished intermediate.
It is retained under its original compiler and binary identities.

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
`RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95` and full commit
`512ec76c0c95c9b4947c43f3c820811cae6fa10e`. The source archive is the build
context; do not build from the checkout directory.

## Verify the derivative output

```bash
EXPECTED_VERSION_LINE='minio version RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95 (commit-id=512ec76c0c95c9b4947c43f3c820811cae6fa10e)'
for ARCH in amd64 arm64; do
  IMAGE="concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95-$ARCH"
  VERSION_OUTPUT="$(docker run --rm "$IMAGE" --version)"
  test "$(printf '%s\n' "$VERSION_OUTPUT" | sed -n '1p')" = "$EXPECTED_VERSION_LINE"
done

docker image inspect \
  concord-minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95-amd64 \
  --format '{{json .Config.Labels}}'
```

Both images' first output line must be:

```text
minio version RELEASE.2025-10-15T17-29-55Z.CONCORD.512ec76c0c95 (commit-id=512ec76c0c95c9b4947c43f3c820811cae6fa10e)
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
