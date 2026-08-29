# Concord MinIO Source Publication and Cutover Gate

> **Status:** Phase 1 complete; consumer pinned; Phase 2 live cutover still deferred
> **Owner:** Concord Voice operations
> **Last updated:** 2026-08-28 (status corrected; body last revised 2026-07-13)
> **Build record:**
> [`infrastructure/docker/minio/SOURCE-BUILD.md`](../../infrastructure/docker/minio/SOURCE-BUILD.md)

> ### Corrected 2026-08-28 — the consumer step already happened
>
> This runbook described updating the shared `docker-compose.yml` as forbidden during
> Phase 1 and pending in Phase 2. It landed on **2026-07-14** in
> [PR #2226](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2226) —
> one day after the body was last revised. `docker-compose.yml:126` pins
> `ghcr.io/concord-voice/minio:RELEASE.2025-10-15T17-29-55Z@sha256:08e1ba1a…`, the exact
> digest the "Current Fixed Release" table below records.
>
> The **live cutover** is genuinely still deferred: nothing here replaced the running
> MinIO container on the production server. That half cannot be confirmed from the
> repository — check the host.

## Purpose

This runbook governs Concord's unmodified upstream-source MinIO build,
publication, and the safety gate for a later production cutover.

Phase 1 publishes a reproducible runtime and its corresponding source. It does
not replace a live container or touch a data volume. Phase 2 starts only after
the published digests exist.

A separately reviewed, disposable rehearsal must also pass against the then-current
deployment.

**The `docker-compose.yml` clause is spent** — see the correction banner above. Read the
remaining Phase 2 language as covering the live container only.

The container is disposable. The data is not.

This procedure never authorizes:

- a patch to the upstream MinIO tree
- publication or use of `latest`
- a fallback to Docker Hub or an unrelated third-party image
- a change to the current static SigV4, internal-only, no-STS authentication
  model as part of an image refresh
- deletion, recreation, formatting, or overwrite of a production data volume

Any source patch or authentication-model change needs its own design, security,
and legal review.

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

**Done.** The shared `docker-compose.yml` was updated after both public digest proofs
passed, by [PR #2226](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2226) on
2026-07-14. Verify with `grep -n 'image:.*minio' docker-compose.yml`: line 126 carries the
exact Concord `tag@sha256:digest` with `pull_policy: missing` and no Docker Hub fallback.

The requirements still stand for any future re-pin. The MinIO service must use the exact
Concord `tag@sha256:digest`, retain `pull_policy: missing`, and have no Docker Hub
fallback.

That one shared reference is the contract for production, managed provisioning,
self-hosting, development, CI, and every future server. New servers pull the
reviewed digest when it is absent. Existing servers pre-pull it before a
targeted replacement.

No Phase 1 **publish** PR may change the consumer or claim production CVE coverage. The
consumer pin was a deliberate separate change (PR #2226), not a Phase 1 publish. The CVE
half is unchanged and still binding: the pin alone gives no production CVE coverage,
because the running container on the live server has not been replaced.

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
| Runtime tag | `ghcr.io/concord-voice/minio:RELEASE.2025-10-15T17-29-55Z` |
| Source tag | `ghcr.io/concord-voice/minio-source:RELEASE.2025-10-15T17-29-55Z` |
| Runtime digest | `sha256:08e1ba1a7396036f40c57fed2dfabe687ca722d233576484a817a8908bee66c5` |
| Source digest | `sha256:749db4d34817703406a32e6c416e262c632a7a43b0b106947387fbb3c9ed84bd` |

Update this table only from a successful publisher workflow summary.

**The shared Compose consumer no longer "remains unchanged" — it is pinned to the runtime
digest above** (`docker-compose.yml:126`, PR #2226, 2026-07-14). What is still outstanding
is the **live server**: replacing the running MinIO container on production requires
Concord to separately authorize, review, rehearse, and verify Phase 2. That half is **not
verified from the repository** — the repository cannot show which image a running container
was started from. Check the host directly:

```bash
docker inspect concordvoice-minio --format '{{.Image}} {{.Config.Image}}'
```
