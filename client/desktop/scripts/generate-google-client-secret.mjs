#!/usr/bin/env node
// Generator for client/desktop/googleClientSecret.json (#975).
//
// Reads GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP from process.env (or a sibling
// .env file) and writes it into googleClientSecret.json so the MAIN process
// can read it via process.resourcesPath (forge extraResource), mirroring
// buildtag.json (#920 §5.13).
//
// Per Google's "Desktop application" OAuth client guidance, this client_secret
// is NOT confidential — PKCE is the control. Deliberately NOT VITE_-prefixed:
// the renderer never performs the exchange and must never see this value.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export function resolveClientSecret(env, envFileContent) {
  if (env && typeof env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP === 'string') {
    const trimmed = env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP.trim();
    if (trimmed.length > 0) return trimmed;
  }
  if (envFileContent) {
    const m = envFileContent.match(/^GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP=(.+)$/m);
    if (m) {
      const trimmed = m[1].trim();
      if (trimmed.length > 0) return trimmed;
    }
  }
  return '';
}

export function formatClientSecretJson(clientSecret) {
  return JSON.stringify({ clientSecret }, null, 2) + '\n';
}

/* istanbul ignore next -- thin CLI shim; helpers above are covered by tests */
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const cwd = process.cwd();
  const envPath = path.resolve(cwd, '.env');
  const envFileContent = fs.existsSync(envPath) ? fs.readFileSync(envPath, 'utf-8') : null;
  const clientSecret = resolveClientSecret(process.env, envFileContent);
  // Fail-loud in CI when Google SSO is expected: an empty secret silently
  // breaks packaged Google sign-in. Gate on an explicit opt-in var so dev/CI
  // builds that don't ship Google SSO still pass.
  if (clientSecret === '' && process.env.CI === 'true' && process.env.GOOGLE_SSO_DESKTOP_REQUIRED === 'true') {
    console.error('generate-google-client-secret: GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP unset in CI with GOOGLE_SSO_DESKTOP_REQUIRED=true.');
    process.exit(1);
  }
  const outputPath = path.resolve(cwd, 'googleClientSecret.json');
  fs.writeFileSync(outputPath, formatClientSecretJson(clientSecret));
  console.log(`Wrote ${outputPath} (clientSecret ${clientSecret ? 'set' : 'empty'})`);
}
