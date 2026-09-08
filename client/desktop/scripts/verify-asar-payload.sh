#!/usr/bin/env bash
#
# verify-asar-payload.sh — assert the app.asar payload boundary on a PRODUCED archive.
#
# Usage: verify-asar-payload.sh <asar-bin> <path/to/app.asar>
#
# Why this exists as a script rather than inline workflow shell (#3156): the check
# is needed identically by build-desktop.yml's `pr-smoke-build` and `release-build`
# jobs, and an inline copy in each drifted within a single PR — the smoke copy ended
# up carrying a comment about a Windows matrix it does not have. One file, two call
# sites, and it is runnable locally against `npm run package` output, which is the
# only way to inspect the boundary without waiting for CI.
#
# Why it asserts an exact top-level SET rather than a list of forbidden names:
# `packagingIdentity.test.ts` applies packagerConfig.ignore's regexes to synthetic
# paths, which proves the config holds a value — not that Forge and
# @electron/packager honour it through path normalization, directory traversal and
# archive construction. A forbidden-name list shares the unit test's blind spot: an
# entry nobody listed passes both layers silently. The archive is the one layer that
# knows the whole truth, so this is where enumerate-the-good belongs.
set -euo pipefail

ASAR_BIN=${1:?usage: verify-asar-payload.sh <asar-bin> <app.asar>}
ASAR=${2:?usage: verify-asar-payload.sh <asar-bin> <app.asar>}

# `asar list` builds entries with Node's platform-dependent path.join, so on a
# Windows runner it emits "\build\icon.png". Normalize unconditionally: this runs on
# macOS, Linux and Windows legs, and a POSIX filename containing a literal backslash
# is not a shape this tree produces.
ENTRIES=$("$ASAR_BIN" list "$ASAR" | tr '\134' '/')

# MUST be present. Their absence means the archive is unusable, and the bare
# directory entries are the load-bearing half: @electron/packager's copy filter is
# consulted on a DIRECTORY before it descends, so a pattern matching /node_modules
# or /dist suppresses the whole subtree while every per-file assertion elsewhere
# stays green (the app then fails to launch with nothing red).
REQUIRED_TOP='/build
/dist
/node_modules
/package.json'

# MAY additionally be present: generated into the packager's dir root by the release
# workflow immediately before packaging. They also reach <Resources>/ via
# extraResource, which does NOT go through the ignore array, so the archive copies
# are redundant rather than load-bearing. Listed here so their presence is a
# reviewed decision rather than an unnoticed residue.
OPTIONAL_TOP='/app-update.yml
/buildtag.json
/googleClientSecret.json'

ACTUAL_TOP=$(awk -F/ 'NF==2' <<<"$ENTRIES" | sort -u)
ALLOWED=$(printf '%s\n%s\n' "$REQUIRED_TOP" "$OPTIONAL_TOP" | sort -u)

FAILURES=0

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! grep -qxF -- "$entry" <<<"$ALLOWED"; then
    echo "::error::Unexpected top-level entry in app.asar: ${entry}"
    FAILURES=$((FAILURES + 1))
  fi
done <<<"$ACTUAL_TOP"

while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  if ! grep -qxF -- "$entry" <<<"$ACTUAL_TOP"; then
    echo "::error::Required top-level entry missing from app.asar: ${entry}"
    FAILURES=$((FAILURES + 1))
  fi
done <<<"$REQUIRED_TOP"

# build/ is a PARTIAL exclusion and needs assertions in both directions. The packaged
# main process reads build/icon.png (src/main/main.ts, splash) and build/icon.icns
# (src/main/applicationsFolderGate.ts, macOS move-to-Applications) from inside the
# archive via app.getAppPath(); both go through nativeImage.createFromPath, which
# never throws, so an over-broad rule loses the branding silently in release builds.
# Asserting the exact set catches an over-broad rule AND a widened lookahead.
EXPECTED_BUILD='/build/icon.icns
/build/icon.png'
ACTUAL_BUILD=$(grep '^/build/' <<<"$ENTRIES" | sort || true)
if [ "$ACTUAL_BUILD" != "$EXPECTED_BUILD" ]; then
  echo "::error::build/ in app.asar must contain exactly the two runtime icons. Found:"
  printf '%s\n' "${ACTUAL_BUILD:-(nothing)}"
  FAILURES=$((FAILURES + 1))
fi

# DEPTH check. Everything above asserts the top level plus /build/, which says
# nothing about what is INSIDE /dist and /node_modules — and that is exactly where
# the compile-time-only files live. Both suffix rules in packagerConfig.ignore are
# checked here, in the same shape, because a regression in either is invisible to
# every other layer: `.d.ts` shipped for the life of this PR (1035 entries,
# ~13.4 MiB) precisely because /\.map$/ takes only their .d.ts.map companions and
# the top-level set treats all of /dist as valid.
#
# Archive-wide, not scoped to /dist: both rules are suffix rules that deliberately
# reach into production dependencies, which is where 92% of the declarations were.
# grep -E, not the default BRE: `[cm]?` is a literal '?' in a basic regex, so a
# BRE version of this pattern would match nothing and the check would pass
# vacuously — the exact failure mode this depth check exists to prevent.
for suffix in '\.d\.[cm]?ts' '\.map'; do
  OFFENDERS=$(grep -E -- "${suffix}\$" <<<"$ENTRIES" || true)
  [ -n "$OFFENDERS" ] || continue
  COUNT=$(printf '%s\n' "$OFFENDERS" | wc -l | tr -d ' ')
  echo "::error::app.asar contains ${COUNT} compile-time-only file(s) matching /${suffix}\$/; none should ship."
  printf '%s\n' "$OFFENDERS" | head -20
  FAILURES=$((FAILURES + 1))
done

if [ "$FAILURES" -gt 0 ]; then
  echo "--- Diagnostic: all top-level entries in app.asar ---"
  printf '%s\n' "$ACTUAL_TOP"
  echo "::error::${FAILURES} payload-boundary violation(s) in app.asar"
  exit 1
fi

echo "app.asar payload boundary verified: top level is exactly what it should be, and no compile-time-only files ship."
