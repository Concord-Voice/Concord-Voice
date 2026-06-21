import { readFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Google OAuth "Desktop application" client_secret — non-confidential per
 * Google's native-app guidance (PKCE is the control). Shipped as a resource
 * JSON (mirrors buildtag.json, #920 §5.13), read MAIN-process-only. Never
 * exposed to the renderer, never VITE_-prefixed. Dev fallback: env var.
 */
export function loadGoogleClientSecret(): string {
  try {
    const resourcesPath = process.resourcesPath;
    if (!resourcesPath) throw new Error('no-resources-path');
    const p = join(resourcesPath, 'googleClientSecret.json');
    const parsed = JSON.parse(readFileSync(p, 'utf-8')) as { clientSecret?: unknown };
    if (typeof parsed.clientSecret === 'string' && parsed.clientSecret.length > 0) {
      return parsed.clientSecret;
    }
  } catch {
    /* not packaged / dev — fall through to env */
  }
  return process.env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP ?? '';
}
