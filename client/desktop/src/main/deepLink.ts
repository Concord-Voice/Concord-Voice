/**
 * The 8-char ambiguity-stripped charset predicate for this surface. It is one of
 * THREE independent copies — the others are `INVITE_CODE_PATTERN` in
 * [internal]workers/invite-landing/src/stub.ts and `CODE_CLASS`
 * in client/desktop/src/renderer/utils/messaging/inviteUrl.ts. #1557's variable-length
 * vanity slugs must relax all three; missing the Worker copy is a validation
 * bypass on the anonymous edge path.
 */
const INVITE_CODE_RE = /^[A-HJ-NP-Za-hj-km-np-z2-9]{8}$/;

export type DeepLinkKind = 'invite' | 'friend';

export type InviteDeepLinkResult =
  | { ok: true; kind: DeepLinkKind; code: string }
  | {
      ok: false;
      reason: 'empty' | 'invalid-url' | 'wrong-protocol' | 'wrong-host' | 'bad-path' | 'bad-code';
    };

export function normalizeInviteDeepLink(raw: string | undefined): InviteDeepLinkResult {
  if (!raw) return { ok: false, reason: 'empty' };

  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return { ok: false, reason: 'invalid-url' };
  }

  if (parsed.protocol !== 'concord:') return { ok: false, reason: 'wrong-protocol' };
  if (parsed.host !== 'invite' && parsed.host !== 'friend') {
    return { ok: false, reason: 'wrong-host' };
  }

  const parts = parsed.pathname.split('/').filter(Boolean);
  if (parts.length !== 1) return { ok: false, reason: 'bad-path' };

  const code = parts[0];
  if (code === undefined || !INVITE_CODE_RE.test(code)) {
    return { ok: false, reason: 'bad-code' };
  }
  return { ok: true, kind: parsed.host === 'friend' ? 'friend' : 'invite', code };
}

export function extractInviteDeepLinkFromArgv(argv?: readonly string[]): InviteDeepLinkResult {
  if (!Array.isArray(argv)) return { ok: false, reason: 'empty' };

  for (const arg of argv) {
    const result = normalizeInviteDeepLink(arg);
    if (result.ok) return result;
  }
  return { ok: false, reason: 'empty' };
}
