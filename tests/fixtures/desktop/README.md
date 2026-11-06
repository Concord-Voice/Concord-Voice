# Vendored desktop modules

The desktop client lives in [Concord-Voice/Desktop](https://github.com/Concord-Voice/Desktop).
Five of its files pin contracts that Hosted still owns, so Hosted keeps a copy of each here.
Each copy is byte-identical to `desktop/<same path>` at the commit in
`desktop-spa.sha` at the repository root (`b204d7c9a99e0e32bb6894f1772764f2be3d967a`).

| File | Checked by |
| --- | --- |
| `src/renderer/stores/voice/audioQualityTiers.ts` | `services/media-plane/tests/audioPolicyParity.test.ts`, `services/control-plane/internal/entitlements/audio_tiers_crossplane_test.go` |
| `src/renderer/services/voice/fecHeadroom.ts` | `services/media-plane/tests/audioPolicyParity.test.ts` |
| `src/renderer/services/voice/remoteVideoLayerPolicy.ts` | `services/media-plane/tests/cameraLayerPolicyParity.test.ts` |
| `src/renderer/components/Markdown/sanitizeSchema.ts` | `.github/scripts/tests/promote-unreleased-changelog.test.sh` (strip-list pin) |
| `src/main/ipcContract.ts` | `[internal]tests/test-deploy-spa.sh` (tests 30 and 35, real-contract floor) |

These are snapshots. When you bump `desktop-spa.sha`, copy the five files again from the new
commit so the parity tests check the client that production actually serves. PR CI compares
them with Desktop at the pinned commit and fails if any differs.
