// Stub for the deprecated node-domexception package.
// Node 18+ has a native global DOMException; we just re-export it.
// See services/media-plane/package.json overrides_rationale for why.
//
// This file is CommonJS by design (no "type" field in this package's own
// package.json), so fetch-blob's transitive `require('node-domexception')` can
// consume it from an otherwise ESM media-plane workspace.
//
// Defensive guard: if a future environment loads this stub before
// globalThis.DOMException is defined (e.g., a downgraded Node, an unusual
// worker context), fail loudly at require-time rather than silently re-export
// undefined and let consumers later fail with confusing TypeError messages.
if (typeof globalThis.DOMException !== 'function') {
  throw new Error(
    'node-domexception stub: globalThis.DOMException is not available. ' +
      'This stub re-exports the native DOMException (introduced in Node 18+); ' +
      'the runtime appears to be older than expected.'
  );
}
module.exports = globalThis.DOMException;
