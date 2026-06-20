#!/bin/bash
# Post-install hook with two responsibilities:
#
#   1. Cross-platform: trigger Electron's lazy-download install so that
#      node_modules/electron/path.txt exists. Required for `electron/index.js`
#      to load successfully at vitest module-graph resolution time, even when
#      tests mock electron (the mock applies AFTER node resolves the path).
#      Electron 42+ no longer auto-runs its install.js as part of npm
#      postinstall; we trigger it explicitly here.
#
#   2. macOS-only: patch Electron's helper-app Info.plist files with
#      NSMicrophoneUsageDescription and NSCameraUsageDescription. Required
#      for macOS TCC to allow getUserMedia in dev mode (the renderer helper
#      process calls getUserMedia, not the main process). Without these keys,
#      macOS kills the process with SIGABRT before any JS error handling can
#      kick in.
#
# Runs automatically via npm postinstall.

set -euo pipefail

ELECTRON_APP="node_modules/electron/dist/Electron.app"
ELECTRON_PATH_TXT="node_modules/electron/path.txt"

# ─────────────────────────────────────────────────────────────────────────────
# Step 1: Cross-platform — ensure Electron binary is installed so path.txt
# exists. Without this, vitest on Linux flakes: `electron/index.js` reads
# path.txt at module-load time (before vi.mock can take effect), and missing
# path.txt triggers an in-process download that races with parallel test
# files.
#
# Note on concurrency: @electron/get does NOT lock its cache directory. The
# cache is GLOBAL (~/Library/Caches/electron/ on macOS, ~/.cache/electron/ on
# Linux, via envPaths()), so the race surface is any concurrent `npm install`
# on the same machine that pulls Electron — NOT just within the same
# node_modules/. This is safe today because Concord CI runners have clean
# filesystems and no concurrent Electron-pulling jobs. If that changes (e.g.,
# shared self-hosted runners, parallel dep-bumps), behavior becomes undefined
# and this assumption needs to be revisited. See #983 for the underlying
# analysis.
if [ ! -f "$ELECTRON_PATH_TXT" ] && [ -x "node_modules/.bin/install-electron" ]; then
  echo "[patch-electron-plist] electron/path.txt not present (v42+ lazy download); triggering install-electron..."
  # Disable set -e locally so we can capture the real exit code; in the prior
  # `if ! cmd; then rc=$?; fi` shape, $? was the status of the negated `! cmd`
  # (always 0 on failure), making the fail-closed branch never fire (Copilot
  # review on #1204 caught this).
  set +e
  node_modules/.bin/install-electron
  install_rc=$?
  set -e
  if [ "$install_rc" -ne 0 ]; then
    echo "[patch-electron-plist] install-electron failed (rc=$install_rc); test/dev launches will fail until resolved" >&2
    exit "$install_rc"
  fi
fi

# Safety net: install-electron sometimes returns rc=0 without writing path.txt.
# Empirically observed on PR #1213 (CI runs job 78207140452 + 78208242015):
# install-electron exited in <1 second with no output, but path.txt was still
# missing 7 minutes later when vitest tried to load `electron/index.js`. The
# likely cause is install.js's `isInstalled()` short-circuit racing with the
# `~/.cache/electron/` cache state — but the practical fix is independent of
# diagnosis: if path.txt is missing after install-electron returned 0, write
# it ourselves so `electron/index.js:47 getElectronPath` succeeds at module-
# load time. Tests mock electron via vi.mock anyway, so the binary path
# pointed at by path.txt doesn't need to actually exist for tests to pass.
if [ ! -f "$ELECTRON_PATH_TXT" ]; then
  case "$(uname -s)" in
    Darwin)
      printf 'Electron.app/Contents/MacOS/Electron' > "$ELECTRON_PATH_TXT"
      ;;
    Linux)
      printf 'electron' > "$ELECTRON_PATH_TXT"
      ;;
    MINGW*|MSYS*|CYGWIN*)
      printf 'electron.exe' > "$ELECTRON_PATH_TXT"
      ;;
    *)
      echo "[patch-electron-plist] WARN: unknown platform $(uname -s); cannot write path.txt safety net" >&2
      ;;
  esac
  if [ -f "$ELECTRON_PATH_TXT" ]; then
    echo "[patch-electron-plist] Safety net: wrote path.txt manually after install-electron exited 0 without it"
  fi
fi

# ─────────────────────────────────────────────────────────────────────────────
# Step 2: macOS-only — Info.plist patching for TCC getUserMedia consent.
if [ "$(uname)" != "Darwin" ]; then
  echo "[patch-electron-plist] Not macOS, skipping Info.plist patch"
  exit 0
fi

if [ ! -d "$ELECTRON_APP" ]; then
  echo "[patch-electron-plist] Electron.app not found, skipping Info.plist patch"
  exit 0
fi

# plist_has_key <plist-path> <key-name>
# Returns 0 if key exists in plist, 1 if key absent (caller should add it).
# Exits the script with PlistBuddy's exit code on a corrupted-plist failure
# (which produces a distinct "Error Reading File" stderr). Note: missing-file
# and permission-denied conditions ALSO produce the "Does Not Exist" fingerprint
# and would be misclassified as "key absent" — the script relies on `find` having
# already filtered to existing readable files (the only call paths in this script).
#
# Caveats:
#   - <key-name> SHOULD be a flat top-level key. Both current callers use flat
#     keys. Nested colon-paths (e.g., NSArray:0) produce the same "Does Not
#     Exist" fingerprint for a missing leaf, but edge cases involving missing
#     intermediate containers (e.g., :NonExistentDict:Key) haven't been audited
#     — if you add a caller with nested paths, verify the stderr fingerprint
#     for those failure modes before trusting the exit branch.
#   - On exit-on-error mid-loop, earlier Info.plist edits remain on disk but the
#     bundle is not re-signed. This is the intended "loud failure" behavior —
#     developer re-runs npm install after resolving the underlying corruption.
plist_has_key() {
  local plist="$1"
  local key="$2"
  local stderr_output rc

  # Capture stderr; discard stdout (we don't need the printed value).
  # set +e is required to read $? on the failure path; immediately re-enabled.
  set +e
  stderr_output=$(/usr/libexec/PlistBuddy -c "Print :$key" "$plist" 2>&1 >/dev/null)
  rc=$?
  set -e

  if [ "$rc" -eq 0 ]; then
    return 0
  fi

  if echo "$stderr_output" | grep -q "Does Not Exist"; then
    return 1
  fi

  echo "[patch-electron-plist] ERROR: PlistBuddy failed for $plist (rc=$rc): $stderr_output" >&2
  exit "$rc"
}

# sign_component <bundle-path>
# Re-signs a single .app or .framework bundle ad-hoc, preserving identifier,
# entitlements, and flags from the existing signature. Returns 0 on success,
# non-zero on failure (caller is responsible for aggregating/reporting).
#
# Note: --preserve-metadata is effectively a no-op on bundles that lack an
# existing signature — codesign produces a fresh ad-hoc signature, but
# there's no upstream metadata to carry forward. Official Electron builds
# arrive ad-hoc-signed, so this caveat applies only to unsigned dev builds.
sign_component() {
  local bundle="$1"
  local out rc
  # set +e is required to read $? on the failure path; set -e auto-aborts before rc=$?.
  set +e
  out=$(codesign --force --preserve-metadata=identifier,entitlements,flags \
    --sign - "$bundle" 2>&1)
  rc=$?
  set -e
  if [ "$rc" -eq 0 ]; then
    return 0
  fi
  echo "[patch-electron-plist] codesign failed for $bundle (rc=$rc): $out" >&2
  return "$rc"
}

MIC_DESC="This app needs microphone access for voice calls."
CAM_DESC="This app needs camera access for video calls."

# Process-substitution caveat: `find` runs in a subprocess whose exit code is
# discarded by `< <(...)`, so a silent find failure would yield an empty loop
# without aborting the script. Defend with a PROCESSED counter: a valid
# Electron.app always contains at least one Info.plist (the outer bundle's),
# so a count of 0 means find itself failed or the bundle is malformed.
PATCHED=0
PROCESSED=0
while IFS= read -r -d '' plist; do
  PROCESSED=$((PROCESSED + 1))
  CHANGED=false
  if ! plist_has_key "$plist" "NSMicrophoneUsageDescription"; then
    /usr/libexec/PlistBuddy -c "Add :NSMicrophoneUsageDescription string '$MIC_DESC'" "$plist"
    CHANGED=true
  fi
  if ! plist_has_key "$plist" "NSCameraUsageDescription"; then
    /usr/libexec/PlistBuddy -c "Add :NSCameraUsageDescription string '$CAM_DESC'" "$plist"
    CHANGED=true
  fi
  if [ "$CHANGED" = true ]; then
    PATCHED=$((PATCHED + 1))
  fi
done < <(find "$ELECTRON_APP" -name "Info.plist" -print0)

if [ "$PROCESSED" -eq 0 ]; then
  echo "[patch-electron-plist] ERROR: find returned no Info.plist files in $ELECTRON_APP — bundle may be malformed or find failed silently" >&2
  exit 1
fi

if [ "$PATCHED" -gt 0 ]; then
  echo "[patch-electron-plist] Patched $PATCHED Info.plist file(s), re-signing..."
  # Bottom-up: sign nested helpers/frameworks first, then the outer bundle.
  # Per Apple, codesign --deep is deprecated for signing as of macOS 13 Ventura
  # (2022, per codesign(1) manpage) — per-component
  # signing with --preserve-metadata is the recommended replacement.
  #
  # The walk matches *.app and *.framework only. Bare MachO dylibs nested
  # INSIDE a *.framework (e.g., libEGL.dylib, libGLESv2.dylib inside
  # Electron Framework.framework/Versions/A/Libraries/) are sealed via the
  # framework's CodeResources manifest (Sealed Resources) when the framework
  # itself is signed by the walk above — NOT via the outer Electron.app's
  # signature. Any genuinely top-level dylibs directly under Contents/Frameworks/
  # (none in current Electron 42 layout) would not be covered by this walk,
  # but codesign refuses to sign an outer bundle containing unsigned nested
  # code, so such regressions would surface loudly when sign_component
  # "$ELECTRON_APP" runs at the end.
  SIGN_FAILED=0
  # No `2>/dev/null` on the find below: Contents/Frameworks always exists in a
  # valid Electron install, so any find error (permission denied, broken
  # bundle, symlink loop) is informative and worth surfacing rather than
  # suppressing.
  while IFS= read -r -d '' nested; do
    sign_component "$nested" || SIGN_FAILED=$((SIGN_FAILED + 1))
  done < <(find "$ELECTRON_APP/Contents/Frameworks" -mindepth 1 -maxdepth 1 \
    \( -name "*.app" -o -name "*.framework" \) -print0)

  sign_component "$ELECTRON_APP" || SIGN_FAILED=$((SIGN_FAILED + 1))

  if [ "$SIGN_FAILED" -gt 0 ]; then
    echo "[patch-electron-plist] $SIGN_FAILED component(s) failed to re-sign; dev launches may exhibit signing-related issues but this is non-fatal for ad-hoc local builds" >&2
  fi
  echo "[patch-electron-plist] Done"
else
  echo "[patch-electron-plist] All Info.plist files already have usage descriptions"
fi
