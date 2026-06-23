const INVITE_CODE_RE = /^[A-HJ-NP-Za-hj-km-np-z2-9]{8}$/;

export type InviteDeepLinkResult =
  | { ok: true; code: string }
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
  if (parsed.host !== 'invite') return { ok: false, reason: 'wrong-host' };

  const parts = parsed.pathname.split('/').filter(Boolean);
  if (parts.length !== 1) return { ok: false, reason: 'bad-path' };

  const code = parts[0];
  if (code === undefined || !INVITE_CODE_RE.test(code)) {
    return { ok: false, reason: 'bad-code' };
  }
  return { ok: true, code };
}

export function extractInviteDeepLinkFromArgv(argv?: readonly string[]): InviteDeepLinkResult {
  if (!Array.isArray(argv)) return { ok: false, reason: 'empty' };

  for (const arg of argv) {
    const result = normalizeInviteDeepLink(arg);
    if (result.ok) return result;
  }
  return { ok: false, reason: 'empty' };
}
