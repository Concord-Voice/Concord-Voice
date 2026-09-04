/**
 * Validation and read helpers for the control plane's server-minted ICE list (#3104).
 *
 * Pure by design and deliberately free of any `console.*` call: entries carry live
 * HMAC TURN credentials, and every console method is captured into the bug-report
 * ring buffer that reaches a PUBLIC repo (`observability.md` principles 1 and 6).
 * See `[internal]rules/frontend.md` § ICE-server threading.
 */

/** Upper bound on the server-supplied list. An unbounded array feeds the ICE agent directly. */
export const MAX_ICE_SERVERS = 16;

const ALLOWED_SCHEMES = ['stun:', 'stuns:', 'turn:', 'turns:'] as const;
const CREDENTIALED_SCHEMES = ['turn:', 'turns:'] as const;

/**
 * The scheme names `describeIceServers` may emit verbatim. DERIVED from the allow-list so a
 * fifth accepted scheme becomes loggable in the same edit — a hand-maintained second copy
 * would drift into reporting a legitimate scheme as `other`.
 */
const LOGGABLE_SCHEMES: ReadonlySet<string> = new Set(ALLOWED_SCHEMES.map((s) => s.slice(0, -1)));
/**
 * The single label every non-allow-listed scheme collapses to. A fixed literal, never
 * derived from input: it is what keeps `describeIceServers`' output alphabet closed.
 */
const OTHER_SCHEME_LABEL = 'other';

export type CandidatePairType = 'host' | 'srflx' | 'prflx' | 'relay';
const CANDIDATE_PAIR_TYPES: ReadonlySet<string> = new Set(['host', 'srflx', 'prflx', 'relay']);

function normalizeEntry(raw: unknown): RTCIceServer | null {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return null;
  const entry = raw as { urls?: unknown; username?: unknown; credential?: unknown };

  if (typeof entry.urls !== 'string') return null;
  const urls = entry.urls.trim();
  if (urls.length === 0) return null;

  const scheme = ALLOWED_SCHEMES.find((s) => urls.startsWith(s));
  if (!scheme) return null;

  // A turn:/turns: entry without a usable credential pair can never allocate, so
  // keeping it only burns the ICE agent's check budget — the exact failure this
  // change exists to close. Drop it. stun:/stuns: legitimately carry neither.
  if ((CREDENTIALED_SCHEMES as readonly string[]).includes(scheme)) {
    if (typeof entry.username !== 'string' || typeof entry.credential !== 'string') return null;
    return { urls, username: entry.username, credential: entry.credential };
  }

  return { urls };
}

/**
 * Turn the unchecked `ice_servers` field of a join response into a list safe to hand
 * to mediasoup-client. A malformed ENTRY drops only itself: a control plane that later
 * adds a fifth entry shape must not zero out four working relays.
 *
 * "Malformed" includes an entry that THROWS on read. A hostile or buggy response whose
 * accessor traps must cost the caller that one entry, not every relay — an
 * all-or-nothing drop reproduces exactly the outage this file exists to close. The
 * element read is inside the guard too, so a trapped array index is absorbed the same
 * way. Nothing is caught-and-logged: the throw's message is attacker-chosen and NC-1
 * binds on this file.
 */
export function normalizeIceServers(raw: unknown): RTCIceServer[] {
  if (!Array.isArray(raw)) return [];
  const out: RTCIceServer[] = [];
  const limit = Math.min(raw.length, MAX_ICE_SERVERS);
  for (let i = 0; i < limit; i++) {
    try {
      const entry = normalizeEntry(raw[i]);
      if (entry) out.push(entry);
    } catch {
      /* hostile or buggy entry — it drops, the list survives */
    }
  }
  return out;
}

/** Fold one `urls` string into the closed label set, or nothing when it carries no scheme. */
function schemeLabel(url: string): string {
  const idx = url.indexOf(':');
  if (idx <= 0) return OTHER_SCHEME_LABEL;
  const scheme = url.slice(0, idx);
  return LOGGABLE_SCHEMES.has(scheme) ? scheme : OTHER_SCHEME_LABEL;
}

/**
 * The ONLY shape of an ICE-server list that may be logged: how many arrived and which
 * schemes. No host, no username, no credential. See NC-1 in the #3104 design.
 *
 * Total on ANY input, by deliberate choice of an `unknown` parameter. Production calls
 * this on the already-normalized list, but "safe to log" is this function's whole
 * purpose, so it cannot depend on a precondition a future caller may not honour: an
 * unchecked `ice_servers` handed here directly must still be safe. Two properties carry
 * that guarantee — every emitted scheme is drawn from the closed set
 * `stun | stuns | turn | turns | other` (an unrecognized scheme is COUNTED, never
 * echoed), and a non-array, a non-object entry, or a throwing accessor degrades to the
 * empty/partial answer instead of propagating.
 */
export function describeIceServers(servers: unknown): {
  count: number;
  schemes: string[];
} {
  if (!Array.isArray(servers)) return { count: 0, schemes: [] };
  const schemes = new Set<string>();
  for (const s of servers) {
    try {
      const urls = (s as { urls?: unknown } | null | undefined)?.urls;
      if (typeof urls === 'string') {
        schemes.add(schemeLabel(urls));
      } else if (Array.isArray(urls)) {
        // `RTCIceServer.urls` is `string | string[]`; the array form never reaches the
        // normalizer's output but a raw response may well carry it.
        for (const u of urls) if (typeof u === 'string') schemes.add(schemeLabel(u));
      }
    } catch {
      /* entry contributes its count and no scheme */
    }
  }
  return { count: servers.length, schemes: [...schemes].sort((a, b) => a.localeCompare(b)) };
}

/**
 * The selected candidate pair's LOCAL candidate type, or null if ICE has not settled.
 * One closed-enum string; nothing else from the stats report is read or returned.
 *
 * `RTCStatsReport.forEach`'s callback signature is `(value, key, map)`. Real browser
 * stats reports carry `id` on every entry, but Map-based test fixtures need not — so
 * this reads the iteration KEY rather than a `value.id` field, keeping both identical.
 */
export function extractSelectedCandidatePairType(stats: RTCStatsReport): CandidatePairType | null {
  let selectedPairId: string | undefined;
  let chosenLocalId: string | undefined;
  let sawSelected = false;
  const localTypes = new Map<string, unknown>();
  const pairLocalIds = new Map<string, string | undefined>();

  stats.forEach((report: unknown, key: string) => {
    const r = report as {
      type?: string;
      state?: string;
      nominated?: boolean;
      selected?: boolean;
      localCandidateId?: string;
      candidateType?: unknown;
      selectedCandidatePairId?: unknown;
    };
    if (r.type === 'local-candidate') {
      localTypes.set(key, r.candidateType);
      return;
    }
    if (r.type === 'transport') {
      // Authoritative: Chrome names the selected pair here as soon as ICE
      // selects one, which is strictly BEFORE nomination completes. The
      // heuristics below cannot see that window -- `selected` is a legacy
      // field Chrome never emits, and `nominated` is still false -- while
      // mediasoup collapses ICE `connected` and `completed` into one event
      // (Transport.js:794), so the post-nomination retry never arrives.
      if (typeof r.selectedCandidatePairId === 'string') {
        selectedPairId = r.selectedCandidatePairId;
      }
      return;
    }
    if (r.type !== 'candidate-pair') return;
    pairLocalIds.set(key, r.localCandidateId);
    if (r.state !== 'succeeded') return;
    if (r.selected === true) {
      chosenLocalId = r.localCandidateId;
      sawSelected = true;
      return;
    }
    if (!sawSelected && r.nominated === true && chosenLocalId === undefined) {
      chosenLocalId = r.localCandidateId;
    }
  });

  // A transport entry that names a pair wins outright. If it names one that is
  // absent the report is internally inconsistent, so fail closed to no
  // diagnostic rather than falling back to a guess about a different pair.
  const localId = selectedPairId === undefined ? chosenLocalId : pairLocalIds.get(selectedPairId);
  if (localId === undefined) return null;
  const type = localTypes.get(localId);
  if (typeof type !== 'string' || !CANDIDATE_PAIR_TYPES.has(type)) return null;
  return type as CandidatePairType;
}
