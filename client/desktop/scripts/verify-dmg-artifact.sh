#!/usr/bin/env bash
# Verify the produced DMG's install layout and Finder metadata.
#
# The checker intentionally consumes the dmg-builder bundle already downloaded by
# Electron Forge. It never downloads a parser or decodes .DS_Store itself.

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <dmg-path>" >&2
  exit 2
fi

DMG_INPUT=$1
if [[ ! -f "$DMG_INPUT" ]]; then
  echo "::error::DMG does not exist or is not a regular file: $DMG_INPUT" >&2
  exit 1
fi
DMG_PATH="$(cd "$(dirname "$DMG_INPUT")" && pwd)/$(basename "$DMG_INPUT")"

case "$(uname -m)" in
  arm64) DMG_ARCH=arm64 ;;
  x86_64) DMG_ARCH=x86_64 ;;
  *)
    echo "::error::Unsupported macOS architecture for the pinned dmg-builder bundle: $(uname -m)" >&2
    exit 1
    ;;
esac

CACHE_ROOT=${ELECTRON_BUILDER_CACHE:-"$HOME/Library/Caches/electron-builder"}
VENDOR_ROOT="$CACHE_ROOT/dmg-builder@1.2.5"
DMG_BUILDER_PYTHON=""

# app-builder-lib's pinned dmg-builder@1.2.5 bundle carries the only approved
# DSStore/mac_alias implementation. Try every cached bundle in deterministic
# order, but never download or substitute an ambient Python installation.
if [[ -d "$VENDOR_ROOT" ]]; then
  while IFS= read -r bundle; do
    while IFS= read -r candidate; do
      if "$candidate" -c 'from ds_store import DSStore; import mac_alias' >/dev/null 2>&1; then
        DMG_BUILDER_PYTHON=$candidate
        break 2
      fi
    done < <(find "$bundle/python/bin" -type f -name 'python3*' -perm -111 -print 2>/dev/null | sort)
  done < <(find "$VENDOR_ROOT" -maxdepth 1 -type d -name "dmgbuild-bundle-${DMG_ARCH}-75c8a6c-*" -print 2>/dev/null | sort)
fi

if [[ -z "$DMG_BUILDER_PYTHON" ]]; then
  echo "::error::Pinned dmg-builder@1.2.5 bundle with ds_store/mac_alias is unavailable under $VENDOR_ROOT" >&2
  exit 1
fi

MOUNTPOINT="$(mktemp -d "${TMPDIR:-/tmp}/concord-dmg-mount.XXXXXX")"
ATTACHED_DEVICE=""
ATTACH_SUCCEEDED=0

cleanup() {
  local rc=$?
  trap - EXIT
  if [[ "$ATTACH_SUCCEEDED" -eq 1 ]]; then
    local detach_target="$ATTACHED_DEVICE"
    if [[ -z "$detach_target" ]]; then
      detach_target="$MOUNTPOINT"
    fi
    if ! hdiutil detach -force "$detach_target" >/dev/null 2>&1; then
      echo "::error::Failed to detach DMG attachment $detach_target" >&2
      [[ "$rc" -eq 0 ]] && rc=1
    fi
  fi
  if ! rmdir "$MOUNTPOINT" 2>/dev/null; then
    echo "::error::Failed to remove temporary DMG mountpoint $MOUNTPOINT" >&2
    [[ "$rc" -eq 0 ]] && rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT

ATTACH_PLIST="$(hdiutil attach -readonly -nobrowse -plist -mountpoint "$MOUNTPOINT" "$DMG_PATH")"
ATTACH_SUCCEEDED=1
ATTACHED_DEVICE="$(plutil -extract system-entities.0.dev-entry raw -o - - <<<"$ATTACH_PLIST" 2>/dev/null || true)"
if [[ -z "$ATTACHED_DEVICE" ]]; then
  echo "::error::hdiutil did not return an attached device for $DMG_PATH" >&2
  exit 1
fi

APP_PATH="$MOUNTPOINT/Concord Voice.app"
if [[ ! -d "$APP_PATH" ]]; then
  echo "::error::DMG is missing Concord Voice.app at the volume root" >&2
  exit 1
fi

APPLICATIONS_LINK="$MOUNTPOINT/Applications"
if [[ ! -L "$APPLICATIONS_LINK" ]]; then
  echo "::error::DMG is missing the /Applications symlink" >&2
  exit 1
fi
if [[ "$(readlink "$APPLICATIONS_LINK")" != "/Applications" ]]; then
  echo "::error::DMG Applications symlink does not target /Applications" >&2
  exit 1
fi

for asset in "$MOUNTPOINT/.background.tiff" "$MOUNTPOINT/.VolumeIcon.icns"; do
  if [[ ! -f "$asset" ]]; then
    echo "::error::DMG is missing required asset: $(basename "$asset")" >&2
    exit 1
  fi
done

if [[ ! -f "$MOUNTPOINT/.DS_Store" ]]; then
  echo "::error::DMG is missing .DS_Store" >&2
  exit 1
fi

"$DMG_BUILDER_PYTHON" - "$MOUNTPOINT/.DS_Store" <<'PY'
import re
import struct
import sys

from ds_store import DSStore
from mac_alias import Alias

store_path = sys.argv[1]
expected_locations = {
    "Concord Voice.app": (130, 200),
    "Applications": (410, 200),
}

try:
    with DSStore.open(store_path, "r") as store:
        for name, expected in expected_locations.items():
            actual = store[name]["Iloc"]
            if tuple(actual) != expected:
                raise ValueError(f"{name} icon location is {actual}, expected {expected}")

        root_view = store["."]
        window_bounds = root_view["bwsp"]["WindowBounds"]
        icvp = root_view["icvp"]
        if icvp["backgroundType"] != 2:
            raise ValueError(
                f"backgroundType is {icvp['backgroundType']!r}, expected image type 2"
            )
        background_alias = Alias.from_bytes(icvp["backgroundImageAlias"])
        target = background_alias.target
        if target is None:
            raise ValueError("backgroundImageAlias has no target")
        if target.filename != ".background.tiff":
            raise ValueError(
                f"background alias filename is {target.filename!r}, "
                "expected '.background.tiff'"
            )
        if target.posix_path != "/.background.tiff":
            raise ValueError(
                f"background alias path is {target.posix_path!r}, "
                "expected '/.background.tiff'"
            )
except (AttributeError, KeyError, TypeError, ValueError, struct.error) as exc:
    print(f"::error::DMG .DS_Store layout records are invalid: {exc}", file=sys.stderr)
    raise SystemExit(1)

match = re.fullmatch(r"\{\{(-?\d+), (-?\d+)\}, \{(\d+), (\d+)\}\}", window_bounds)
if match is None:
    print(f"::error::DMG .DS_Store WindowBounds has unexpected form: {window_bounds!r}", file=sys.stderr)
    raise SystemExit(1)

width, height = (int(match.group(3)), int(match.group(4)))
if (width, height) != (540, 380):
    print(f"::error::DMG Finder window is {width}x{height}, expected 540x380", file=sys.stderr)
    raise SystemExit(1)

print("DMG .DS_Store layout verified: window 540x380, app 130,200, Applications 410,200")
PY

echo "DMG artifact verified: Concord Voice.app, /Applications link, background, icon, and Finder layout are present"
