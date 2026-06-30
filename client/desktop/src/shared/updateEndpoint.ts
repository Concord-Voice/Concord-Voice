// Static public electron-updater feed.
//
// GitHub /releases/latest/download/<asset> resolves to the latest public
// release asset, giving packaged clients a recovery path that does not depend
// on the pinned API hostname.
export const UPDATE_ENDPOINT_URL =
  'https://github.com/Concord-Voice/Concord-Voice/releases/latest/download' as const;
