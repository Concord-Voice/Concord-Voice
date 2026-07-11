// Cloudflare Pages Function — status-aware caching for hashed assets (issue #2173).
//
// public/_headers marks /assets/* `immutable`, but that rule is URL-matched and
// STATUS-BLIND. So a request for a hashed chunk that is briefly missing (a
// deploy-propagation window) is served the not-found fallback (the 404.html body,
// or the SPA shell) WITH the immutable Cache-Control — poisoning the browser cache
// for hours (the #2173 incident). `_headers` cannot express "immutable for 200,
// no-store for the fallback", so this Function does it.
//
// It is file-based (not a top-level `_worker.js`), so `context.next()` still runs the
// static-asset pipeline (and `_redirects`) instead of bypassing it. But Cloudflare
// does NOT reliably apply `_headers` to a response returned from a Pages Function, so
// this Function sets the /assets/* cache policy ITSELF rather than trusting `_headers`
// to re-decorate the pass-through. Setting it here is correct under both readings:
// a redundant no-op if `_headers` is applied, and the only source of the policy if it
// is not — which also keeps the post-deploy real-asset check (main-cd.yml #2173) green.
//
//   - A real asset (a genuine 200 whose body is not HTML) is re-served with the
//     canonical immutable policy (`public, max-age=31536000, immutable` +
//     `X-Content-Type-Options: nosniff`), mirroring public/_headers /assets/* so
//     ADR-0015 holds regardless of whether `_headers` reaches Function responses.
//   - Anything else at an /assets/* path is re-served with `Cache-Control: no-store`
//     so the browser does not cache it and recovers on the next request once the asset
//     propagates. This covers not just the 404 / text/html SPA-shell fallback but also
//     a transient 3xx/5xx served during a deploy-propagation window (empty body, JSON,
//     text/plain, or no content-type). The immutable policy is gated on status === 200
//     precisely so such non-success responses are never pinned for a year — stamping
//     immutable on them would re-open the exact cache-poisoning class this fixes.
//
// Discovery: wrangler resolves `functions/` relative to its CWD, so deploy-spa.sh
// passes `--cwd client/desktop`. Verify post-deploy:
//   curl -sI <origin>/assets/<real-hash>.js    -> 200, immutable
//   curl -sI <origin>/assets/DOES-NOT-EXIST.js  -> 404, no-store
export async function onRequest(context) {
  const response = await context.next();
  const contentType = response.headers.get('content-type') || '';
  // Only a genuine 200 non-HTML response is a real hashed asset. A 404, an HTML
  // SPA-shell fallback, or a transient 3xx/5xx at an /assets/* path must NOT be
  // immutable-cached (see the header note).
  const isRealAsset = response.status === 200 && !contentType.includes('text/html');

  const headers = new Headers(response.headers);
  if (isRealAsset) {
    // Apply the canonical immutable policy ourselves so it holds even if `_headers`
    // is not applied to Function responses.
    headers.set('Cache-Control', 'public, max-age=31536000, immutable');
    headers.set('X-Content-Type-Options', 'nosniff');
  } else {
    // Never cache a non-success response served at an /assets/* URL, so a
    // deploy-window client recovers once the real chunk propagates.
    headers.set('Cache-Control', 'no-store');
  }

  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}
