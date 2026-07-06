// Fixed-width UUID (8-4-4-4-12). Fixed quantifiers → no catastrophic
// backtracking (Sonar S5852-safe by construction).
const UUID_RE = /\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b/g;

/**
 * Replace every UUID in `text` with a consistent per-call ordinal token
 * (`<id:1>`, `<id:2>`, …). The same UUID gets the same token WITHIN one call
 * (intra-report correlation for triage); a fresh call starts numbering over,
 * so the same UUID gets a different token in a different report (no
 * cross-report linkability). Non-reversible — the ordinal reveals only
 * order-of-appearance, never identity. Runs at SUBMIT time over the assembled
 * log bundle; capture-time secret scrubbing is unchanged.
 */
export function pseudonymizeLogUuids(text: string): string {
  const map = new Map<string, string>();
  return text.replace(UUID_RE, (uuid) => {
    const key = uuid.toLowerCase();
    let token = map.get(key);
    if (!token) {
      token = `<id:${map.size + 1}>`;
      map.set(key, token);
    }
    return token;
  });
}
