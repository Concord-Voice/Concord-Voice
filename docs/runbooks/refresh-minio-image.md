# Concord MinIO Source Publication and Cutover Gate

> **Status:** Phase 1 complete; consumer pinned; Phase 2 live cutover still deferred
> **Owner:** Concord Voice operations
> **Last updated:** 2026-09-27 (published dependency-only derivative recorded)
> **Build record:**
> [`infrastructure/docker/minio/SOURCE-BUILD.md`](../../infrastructure/docker/minio/SOURCE-BUILD.md)

> ### Consumer history
>
> [PR #2226](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2226)
> first pinned the shared `docker-compose.yml` consumer on **2026-07-14** to the
> upstream-only release. The current consumer pin uses the published derivative
> recorded in the "Current Fixed Release" table below.
>
> The live production cutover remains deferred; the repository cannot confirm
> which image is running on the production server.

## Purpose

This runbook governs Concord's MinIO source build, publication, and the safety
gate for a later production cutover. General refreshes use the exact upstream
source without a Concord patch, subject only to the release-specific exception
recorded below.

Phase 1 publishes a reproducible runtime and its corresponding source. It does
not replace a live container or touch a data volume. Phase 2 starts only after
the published digests exist.

A separately reviewed, disposable rehearsal must also pass against the then-current
deployment.

**The `docker-compose.yml` clause is spent** — see the correction banner above. Read the
remaining Phase 2 language as covering the live container only.

The container is disposable. The data is not.

This procedure never authorizes:

- an unreviewed patch to the upstream MinIO tree; the sole approved dependency
  exception is limited to #3441 and #3469 as described below
- publication or use of `latest`
- a fallback to Docker Hub or an unrelated third-party image
- a change to the current static SigV4, internal-only, no-STS authentication
  model as part of an image refresh
- deletion, recreation, formatting, or overwrite of a production data volume

The general policy remains: any source patch or authentication-model change
needs its own design, security, and legal review. The approved design exception
for #3441 and #3469 is limited to a dependency-only derivative of this upstream
commit. It changes only `go.mod` and `go.sum`, uses AMQP 1.13.0, gRPC 1.83.2,
and the pinned Go 1.26.8 builder. Required security review and recorded legal
approval remain gates before publication dispatch. No application edits,
authentication changes, or live cutover are authorized by the exception.

## Release Contract

Each release is one digest-authoritative set:

| Item | Required identity |
| --- | --- |
| Upstream | Exact MinIO tag and full Git commit |
| Runtime | `ghcr.io/concord-voice/minio:<tag>@sha256:<digest>` |
| Source | `ghcr.io/concord-voice/minio-source:<tag>@sha256:<digest>` |

GHCR does not enforce immutable ordinary tags. Tags are discovery labels.
Captured digests are the release identity. Never move or overwrite a stable tag.

The publisher permits only these stable-tag states:

- both absent
- both present at the exact staged digests
- source present at the exact staged digest, with runtime absent

A runtime-only tag, a mismatched digest, or an ambiguous registry response is a
hard stop.

## 1. Select and Validate Upstream Inputs

1. Review the upstream release and security reason.
2. Resolve the release tag to its full commit.
3. Confirm the source tree is clean and unchanged.
4. Pin the Go builder, runtime base, BuildKit, Buildx asset checksum, available
   runner tooling, and third-party Actions.
5. Update together:
   - `infrastructure/docker/minio/Dockerfile`
   - `infrastructure/docker/minio/SOURCE-BUILD.md`
   - `.github/workflows/publish-minio-image.yml`
   - `scripts/tests/test-publish-minio-image.sh`
   - the fixed-release table below

Example source verification:

```bash
git clone https://github.com/minio/minio.git minio-source
git -C minio-source checkout --detach "$MINIO_TAG"
test "$(git -C minio-source rev-parse HEAD)" = "$MINIO_COMMIT"
SOURCE_STATUS="$(git -C minio-source status --porcelain)"
test -z "$SOURCE_STATUS"
git -C minio-source show -s --format='%H %cI' HEAD
```

For the published upstream-only recipe, the Dockerfile consumes that clean
checkout as its build context. It must not download a MinIO binary. For the
approved derivative, the publisher verifies the clean upstream checkout,
applies and validates only the approved dependency patch, stages those two
manifest files, creates the deterministic derivative commit, archives it, and
extracts the archive into a fresh build context. The derivative runtime is
built from that extracted context, not from the checkout.

Run the repository contract:

The release gate requires Docker Compose v2 and `jq`. It renders configuration
locally and does not contact the Docker daemon or network.

```bash
bash scripts/tests/test-check-oci-tag.sh
bash scripts/tests/test-publish-minio-image.sh --require-compose
actionlint -config-file=.actionlint.yaml \
  .github/workflows/publish-minio-image.yml
git diff --check
```

The publisher must verify, on native amd64 and arm64:

- fixed version and commit metadata
- exactly `linux/amd64` and `linux/arm64`
- liveness and readiness
- authenticated S3 create, PUT, stat, GET, checksum, delete, and bucket removal
- the production capability and `no-new-privileges` tuple
- a clean SIGTERM exit

For the derivative, the native amd64 smoke uses the runner's `curl`
with AWS SigV4 signing instead of pulling a separate MinIO client image. With
temporary protected credentials in a mode-0600 temporary curl config and the
isolated smoke server, require exact HTTP statuses for bucket creation (200),
object PUT (200), HEAD (200), GET
(200), object deletion (204), and bucket deletion (204), then compare the
SHA-256 of the uploaded and downloaded bytes. This exercises standard S3
operations only; it does not validate admin APIs. See the official
[`CURLOPT_AWS_SIGV4` documentation](https://curl.se/libcurl/c/CURLOPT_AWS_SIGV4.html).
The successful [publisher run](https://github.com/Concord-Voice/Concord-Voice-Alpha/actions/runs/36305893881)
verified native amd64 S3 operations and runtime metadata, plus native arm64
startup, readiness, shutdown, and metadata. The publisher arm64 job did not run
full S3 checks; separate local native arm64 validation passed the full S3
lifecycle and object-hash checks. After publication, both public artifacts were
independently pulled from empty authentication stores by digest and verified;
see [the tracker evidence](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1975#issuecomment-5854238424).
The artifact checks do not establish production CVE coverage.

The derivative source inputs are recorded in
[`SOURCE-BUILD.md`](../../infrastructure/docker/minio/SOURCE-BUILD.md), including
the patch, manifest, derivative commit/tree, and archive hashes. The published
tag is `RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62`, with derivative
commit `823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a`. The table below records the
publisher's runtime and source digests.

## 2. Merge and Publish Private Artifacts

The workflow is manual and default-branch only. Merge the reviewed publisher
before dispatch.

Classify package state first:

- **Initial release:** both stable packages are absent. Dispatch the workflow.
  GHCR creates the runtime, source, and staging packages private. After the run
  succeeds, and before Section 3, restrict all four packages to
  `Concord-Voice/Concord-Voice-Alpha` Actions at `Write`. Then remove every
  other repository, team, or user writer.
- **Later release:** both packages are already public. Record legal approval.
  Verify that same four-package access restriction before dispatch, because the
  new tags become public immediately.
- **Anything else:** stop for operator review.

Dispatch from `main`:

```bash
gh workflow run publish-minio-image.yml \
  --ref main \
  -f reason="<approved release reason>"
gh run list --workflow publish-minio-image.yml --limit 1
```

The workflow publishes run-scoped staging artifacts. It runs both native runtime
checks. It verifies the source-to-runtime digest binding. It promotes the stable
source tag before the runtime tag.

Capture from the successful workflow summary:

- workflow URL and commit
- upstream tag and commit
- runtime manifest and platform digests
- full runtime `tag@digest`
- corresponding-source artifact digest

Record those values on the tracking issue. Never place credentials, live
hostnames, object counts, backup paths, or production checksums in the issue.

If only the matching source tag survives, a same-source dispatch may resume
publication. Never delete or rewrite the tag. A runtime-only stable tag is a
contract violation. Stop and escalate.

## 3. Initial Public Release

For initial package creation, record legal approval before you change
visibility. That approval must cover the AGPL attribution, the license
materials, the corresponding-source contents, and the package presentation.

Visibility changes are source-first:

1. Make `minio-source` public.
2. Prove an anonymous source pull by its digest, and verify its checksum.
3. Only then make `minio` public.
4. Prove an anonymous runtime pull by its digest.

The order permits a temporary source-only state and prevents a public runtime
without public corresponding source. Each public transition is irreversible. If
the source proof fails, leave runtime private and stop.

Use empty authentication stores:

```bash
set -euo pipefail
umask 077

EMPTY_ORAS_CONFIG="$(mktemp)"
EMPTY_DOCKER_CONFIG="$(mktemp -d)"
SOURCE_DIR="$(mktemp -d)"
cleanup() {
  rm -rf -- "$SOURCE_DIR" "$EMPTY_DOCKER_CONFIG"
  rm -f -- "$EMPTY_ORAS_CONFIG"
}
trap cleanup EXIT
printf '{"auths":{}}\n' > "$EMPTY_ORAS_CONFIG"

# Run only after minio-source is public.
oras pull \
  --registry-config "$EMPTY_ORAS_CONFIG" \
  --output "$SOURCE_DIR" \
  "ghcr.io/concord-voice/minio-source:$MINIO_TAG@$SOURCE_DIGEST"
(cd "$SOURCE_DIR" && sha256sum -c minio-source.tar.gz.sha256)

# Make minio public only after the source proof above succeeds.
docker --config "$EMPTY_DOCKER_CONFIG" pull \
  "ghcr.io/concord-voice/minio:$MINIO_TAG@$RUNTIME_DIGEST"
```

A logged-in pull is not anonymous-access evidence.

## 4. Shared Consumer and Future Servers

**Done.** The shared `docker-compose.yml` consumer was first added after public digest
proofs passed, by [PR #2226](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2226)
on 2026-07-14. It now carries the current derivative's exact Concord
`tag@sha256:digest`, retains `pull_policy: missing`, and has no Docker Hub fallback.
Verify with `grep -n 'image:.*minio' docker-compose.yml`.

The requirements still stand for any future re-pin. The MinIO service must use the exact
Concord `tag@sha256:digest`, retain `pull_policy: missing`, and have no Docker Hub
fallback.

That one shared reference is the contract for production, managed provisioning,
self-hosting, development, CI, and every future server. New servers pull the
reviewed digest when it is absent. Existing servers pre-pull it before a
targeted replacement.

No Phase 1 **publish** PR may change the consumer or claim production CVE coverage. The
consumer pin remains a separate change from publishing. The pin alone gives no production
CVE coverage because the running container on the live server has not been replaced.

## 5. Phase 2 Live-Cutover Gate

This section is a gate, not executable authorization. Do not run a live cutover
from Phase 1 documentation.

When Concord authorizes live migration, create a short, host-specific procedure
from current evidence. Have operations, security, and database reviewers approve
that procedure. A disposable end-to-end rehearsal must pass before production.

The Phase 2 procedure must include all of these controls:

- Render Compose into root-private temporary files with one cleanup trap. Never
  persist resolved secrets to predictable paths.
- Derive temporary MinIO and PostgreSQL client environment files from resolved
  Compose values, not from raw quoted dotenv lines.
- Record exact running image IDs, container IDs, commands, capabilities, health
  checks, and named-volume identities.
- Prove the rendered top-level MinIO volume name equals the live `/data` volume,
  before the candidate can open it.
- Drain ingress. Keep the maintenance gate closed until application canaries
  pass.
- Require explicit stop timeouts, `exited`, exit code 0, and `OOMKilled=false`
  for every quiesced writer or storage service.
- Capture one quiesced PostgreSQL dump and one metadata-preserving MinIO
  archive.
- Verify the backup destination's reviewed remote source and filesystem type
  with `findmnt`. Then verify checksums on that remote destination.
- Fully restore PostgreSQL before cutover, and compare active storage keys.
- Restore MinIO into fresh named volumes. Test both the candidate digest and the
  exact previous digest against inventory and representative-object hashes.
- Prepare unopened MinIO, PostgreSQL-data, and PostgreSQL-WAL rollback volumes
  before cutover.
- Write a root-private, non-secret, phase-aware recovery checkpoint, so an SSH
  disconnect does not force you to reconstruct state.
- Replace only MinIO, with `--pull never`. Prove the `/data` mount identity does
  not change.
- Verify health, inventory, representative hashes, and new S3 and
  application-level canaries before you reopen ingress.
- Retain pre-upgrade, post-upgrade, and rollback volumes through the incident
  retention window.

Rollback has two explicit boundaries:

- **Before writers reopen:** stop the candidate and start the previous digest on
  the unopened restored MinIO volume. Keep PostgreSQL unchanged.
- **After writers reopen:** stop writers cleanly. Then start the previous MinIO
  with the pre-restored PostgreSQL data and WAL volumes, as one paired
  checkpoint.

The previous MinIO binary must never open a production volume after the new
binary has opened it.

The Phase 2 procedure must never contain or use:

```text
docker compose down -v
docker compose up -V
docker system prune --volumes
rm -rf <data-path>
restore into an existing data volume
overwrite a volume's _data directory
blind-mirror production objects
```

If any identity, backup, restore, rehearsal, quiescence, or canary check fails,
stop. Do not improvise destructive recovery.

## Current Fixed Release

| Item | Value |
| --- | --- |
| Upstream tag | `RELEASE.2025-10-15T17-29-55Z` |
| Upstream commit | `9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a` |
| Derivative commit | `823f9aa2bd624e7fa0bcc2217c4174d9ed6e1c8a` |
| Runtime tag | `ghcr.io/concord-voice/minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62` |
| Source tag | `ghcr.io/concord-voice/minio-source:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62` |
| Runtime digest | `sha256:a1d35733ca68335cc4782e7a81eafa577d841c70ebf9e51fc07f54978fcfc494` |
| Source digest | `sha256:66977d4270328fb09813b1509d492adc25bb6d7b8f2d369b80a6a8c22629bfe4` |

These identities come from [successful publisher run 36305893881](https://github.com/Concord-Voice/Concord-Voice-Alpha/actions/runs/36305893881).
The source archive checksum, derivative metadata, source-runtime binding, and
exact-main recipe were verified. The independent anonymous digest pulls are
recorded in [the tracker evidence](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1975#issuecomment-5854238424).
The upstream tag and commit are unchanged.

The shared Compose consumer is pinned to the runtime digest above. The **live server**
still requires separate authorization, review, rehearsal, and verification under Phase 2.
That deployment state is **not verified from the repository** — the repository cannot
show which image a running container was started from. Check the host directly:

```bash
docker inspect concordvoice-minio --format '{{.Image}} {{.Config.Image}}'
```
