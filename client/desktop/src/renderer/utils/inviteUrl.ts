/** Canonical public host for invite links (Slice 2 will serve a landing page here). */
export const INVITE_HOST = 'invite.concordvoice.chat';

/**
 * 8 chars from the ambiguity-stripped charset in
 * services/control-plane/internal/invites/code.go:
 *   ABCDEFGHJKLMNPQRSTUVWXYZ abcdefghjkmnpqrstuvwxyz 23456789
 * Upper excludes I,O (keeps L); lower excludes i,l,o; digits exclude 0,1.
 */
const CODE_CLASS = 'A-HJ-NP-Za-hj-km-np-z2-9';
const INVITE_CODE_RE = new RegExp(`^[${CODE_CLASS}]{8}$`);

export function isValidInviteCode(code: string): boolean {
  return INVITE_CODE_RE.test(code);
}

export function buildInviteUrl(code: string): string {
  return `https://${INVITE_HOST}/${code}`;
}

/**
 * Find canonical invite-link codes in free text. ONLY exact-host
 * `https://invite.concordvoice.chat/{code}` URLs match (host parsed via the URL
 * API for exactness — no bare codes, no look-alike hosts, https only). Deduped,
 * capped (default 3) — the anti-probing / anti-amplification rule.
 */
export function extractInviteCodes(text: string, max = 3): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const candidates = text.match(/https?:\/\/[^\s<>"')]+/gi) ?? [];
  for (const cand of candidates) {
    let code: string | null = null;
    try {
      // Strip trailing punctuation that can never be part of a canonical invite
      // URL but may appear when a link ends a sentence or sits inside brackets.
      const trimmed = cand.replace(/[.,!?;:'")\]}]+$/, '');
      const u = new URL(trimmed);
      if (u.protocol === 'https:' && u.host === INVITE_HOST) {
        const path = u.pathname.replace(/^\/+/, '').replace(/\/+$/, '');
        if (isValidInviteCode(path)) code = path;
      }
    } catch {
      // not a URL — skip
    }
    if (code && !seen.has(code)) {
      seen.add(code);
      out.push(code);
      if (out.length >= max) break;
    }
  }
  return out;
}

/** Render-gate: never resolve invites in a message whose content isn't decrypted. */
export function messageInviteCodes(
  content: string,
  flags: { pendingKeys?: boolean; decryptFailed?: boolean }
): string[] {
  if (flags.pendingKeys || flags.decryptFailed) return [];
  return extractInviteCodes(content);
}
