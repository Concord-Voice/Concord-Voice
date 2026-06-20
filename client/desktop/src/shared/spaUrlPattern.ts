/**
 * Shared SPA URL pattern (#753, ADR-0001; #976/ADR-0015 flat-host update).
 *
 * `SPA_CHUNK_URL_PATTERN` identifies SPA chunk-asset URLs across BOTH deploy
 * topologies:
 *   - legacy per-SHA (pre-#976):        https?://<host>/spa/<7-hex-sha>/assets/...
 *   - flat Cloudflare Pages (post-#976): https?://<host>/assets/...
 * The `/spa/<sha>/` prefix is OPTIONAL. This is a host-agnostic SHAPE filter,
 * NOT a trust boundary — the renderer pairs it with a same-origin guard
 * (spaSelfHealClient.ts), so the actual host is pinned dynamically there.
 *
 * Consumed by:
 *   - src/renderer/spaSelfHealClient.ts (renderer chunk-error detection)
 *
 * The former `SPA_URL_PATTERN` (a hardcoded `/spa/<sha>/` FRAME-shape regex)
 * was removed by the #976 self-heal fix. The main-process did-fail-load filter
 * and the `spa:requestSelfHeal` sender-frame validator now derive the allowed
 * path-prefix from the RUNTIME SPA URL (`spaState.getRemoteSpaBaseDir`) rather
 * than a hardcoded shape, so a future host/path migration cannot silently
 * re-break self-heal the way the hardcoded pattern did when #976 moved the SPA
 * off `/spa/<sha>/`. See src/main/spaSelfHealMainFrame.ts.
 */
export const SPA_CHUNK_URL_PATTERN = /^https?:\/\/[^/]+\/(?:spa\/[0-9a-f]{7}\/)?assets\/.+/;
