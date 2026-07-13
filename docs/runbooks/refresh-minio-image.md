# Concord MinIO Source Publication and Cutover Gate

> **Status:** Phase 1 active; live cutover deferred
> **Owner:** Concord Voice operations
> **Last updated:** 2026-07-13
> **Build record:**
> [`infrastructure/docker/minio/SOURCE-BUILD.md`](../../infrastructure/docker/minio/SOURCE-BUILD.md)

## Purpose

This runbook governs Concord's unmodified upstream-source MinIO build,
publication, and the safety gate for a later production cutover.

Phase 1 publishes a reproducible runtime and its corresponding source. It does
not change `docker-compose.yml`, replace a live container, or touch a data
volume. Phase 2 begins only after the published digests exist and a separately
reviewed, disposable rehearsal has passed against the then-current deployment.

The container is disposable. The data is not.

This procedure never authorizes:

- patching the upstream MinIO tree;
- publishing or consuming `latest`;
- falling back to Docker Hub or an unrelated third-party image;
- changing the current static SigV4, internal-only, no-STS authentication
  model as part of an image refresh; or
- deleting, recreating, formatting, or overwriting a production data volume.

Any source patch or authentication-model change needs its own design, security,
and legal review.

## Release Contract

Each release is one digest-authoritative set:

| Item | Required identity |
| --- | --- |
| Upstream | Exact MinIO tag and full Git commit |
| Runtime | `ghcr.io/concord-voice/minio:<tag>@sha256:<digest>` |
| Source | `ghcr.io/concord-voice/minio-source:<tag>@sha256:<digest>` |

GHCR does not enforce immutable ordinary tags. Tags are discovery labels;
captured digests are the release identity. Never move or overwrite a stable
tag.

The publisher permits only these stable-tag states:

- both absent;
- both present at the exact staged digests; or
- source present at the exact staged digest while runtime is absent.

A runtime-only tag, a mismatched digest, or an ambiguous registry response is a
hard stop.

## 1. Select and Validate Upstream Inputs

1. Review the upstream release and security reason.
2. Resolve the release tag to its full commit.
3. Confirm the source tree is clean and unchanged.
4. Pin the Go builder, runtime base, BuildKit, Buildx asset checksum, smoke
   client, and third-party Actions.
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

The Dockerfile consumes that clean checkout as its build context. It must not
download a MinIO binary or modify the source.

Run the repository contract:

```bash
bash scripts/tests/test-check-oci-tag.sh
bash scripts/tests/test-publish-minio-image.sh
actionlint -config-file=.actionlint.yaml \
  .github/workflows/publish-minio-image.yml
git diff --check
```

The publisher must verify, on native amd64 and arm64:

- fixed version and commit metadata;
- exactly `linux/amd64` and `linux/arm64`;
- liveness and readiness;
- authenticated S3 create, PUT, stat, GET, checksum, delete, and bucket
  removal;
- the production capability and `no-new-privileges` tuple; and
- clean SIGTERM exit.

## 2. Merge and Publish Private Artifacts

The workflow is manual and default-branch only. Merge the reviewed publisher
before dispatch.

Classify package state first:

- **Initial release:** both stable packages are absent. Dispatch; GHCR creates
  the runtime, source, and staging packages private. After the run succeeds and
  before Section 3, restrict all four packages to
  `Concord-Voice/Concord-Voice-Alpha` Actions at `Write` and remove every
  other repository, team, or user writer.
- **Later release:** both packages are already public. Record legal approval
  and verify that same four-package access restriction before dispatch because
  the new tags will be public immediately.
- **Anything else:** stop for operator review.

Dispatch from `main`:

```bash
gh workflow run publish-minio-image.yml \
  --ref main \
  -f reason="<approved release reason>"
gh run list --workflow publish-minio-image.yml --limit 1
```

The workflow publishes run-scoped staging artifacts, performs both native
runtime checks, verifies the source-to-runtime digest binding, and promotes the
stable source tag before the runtime tag.

Capture from the successful workflow summary:

- workflow URL and commit;
- upstream tag and commit;
- runtime manifest and platform digests;
- full runtime `tag@digest`; and
- corresponding-source artifact digest.

Record those values on the tracking issue. Never place credentials, live
hostnames, object counts, backup paths, or production checksums in the issue.

If only the matching source tag survives, a same-source dispatch may resume
publication. Never delete or rewrite the tag. A runtime-only stable tag is a
contract violation; stop and escalate.

## 3. Initial Public Release

For initial package creation, record legal approval of the AGPL attribution,
license materials, corresponding-source contents, and package presentation
before changing visibility.

Visibility changes are source-first:

1. make `minio-source` public;
2. prove an anonymous source pull by its digest and verify its checksum;
3. only then make `minio` public; and
4. prove an anonymous runtime pull by its digest.

The order permits a temporary source-only state and prevents a public runtime
without public corresponding source. Each public transition is irreversible.
If the source proof fails, leave runtime private and stop.

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

Phase 2 updates the shared `docker-compose.yml` only after both public digest
proofs pass. The MinIO service must use the exact Concord
`tag@sha256:digest`, retain `pull_policy: missing`, and have no Docker Hub
fallback.

That one shared reference is the contract for production, managed
provisioning, self-hosting, development, CI, and every future server. New
servers pull the reviewed digest when absent; existing servers pre-pull it
before a targeted replacement.

No Phase 1 PR may change the consumer or claim production CVE coverage.

## 5. Phase 2 Live-Cutover Gate

This section is a gate, not executable authorization. Do not run a live
cutover from Phase 1 documentation.

When live migration is authorized, create a short, host-specific procedure
from current evidence and have operations, security, and database reviewers
approve it. A disposable end-to-end rehearsal must pass before production.

The Phase 2 procedure must include all of these controls:

- render Compose into root-private temporary files with one cleanup trap;
  never persist resolved secrets to predictable paths;
- derive temporary MinIO and PostgreSQL client environment files from resolved
  Compose values, not raw quoted dotenv lines;
- record exact running image IDs, container IDs, commands, capabilities,
  health checks, and named-volume identities;
- prove the rendered top-level MinIO volume name equals the live `/data`
  volume before the candidate can open it;
- drain ingress and keep the maintenance gate closed until application
  canaries pass;
- require explicit stop timeouts, `exited`, exit code 0, and
  `OOMKilled=false` for every quiesced writer or storage service;
- capture one quiesced PostgreSQL dump and metadata-preserving MinIO archive;
- verify the backup destination's reviewed remote source and filesystem type
  with `findmnt`, then verify checksums on that remote destination;
- fully restore PostgreSQL before cutover and compare active storage keys;
- restore MinIO into fresh named volumes and test both the candidate digest and
  exact previous digest against inventory and representative-object hashes;
- prepare unopened MinIO, PostgreSQL-data, and PostgreSQL-WAL rollback volumes
  before cutover;
- write a root-private, non-secret, phase-aware recovery checkpoint so an SSH
  disconnect does not require reconstructing state;
- replace only MinIO, with `--pull never`, and prove the `/data` mount
  identity is unchanged;
- verify health, inventory, representative hashes, and new S3 and
  application-level canaries before reopening ingress; and
- retain pre-upgrade, post-upgrade, and rollback volumes through the incident
  retention window.

Rollback has two explicit boundaries:

- **Before writers reopen:** stop the candidate and start the previous digest
  on the unopened restored MinIO volume. Keep PostgreSQL unchanged.
- **After writers reopen:** stop writers cleanly and start the previous MinIO
  plus the pre-restored PostgreSQL data and WAL volumes as one paired
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
| Runtime tag | `ghcr.io/concord-voice/minio:RELEASE.2025-10-15T17-29-55Z` |
| Source tag | `ghcr.io/concord-voice/minio-source:RELEASE.2025-10-15T17-29-55Z` |
| Runtime digest | Pending first default-branch publication |
| Source digest | Pending first default-branch publication |

Update this table only from a successful publisher workflow summary. The live
server and shared Compose consumer remain unchanged until Phase 2 is separately
authorized, reviewed, rehearsed, and verified.
