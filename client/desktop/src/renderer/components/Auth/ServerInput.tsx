import React, { useEffect, useRef, useState } from 'react';
import './ServerInput.css';

interface ServerInputProps {
  onConnect: (serverUrl: string) => void;
  onBack: () => void;
}

// Main classifies the address; the renderer only maps a closed enum token to copy.
// An unrecognised token falls through to the generic string, so the guard can gain a
// class with no renderer redeploy. The raw token is never rendered.
type RefusalCode =
  | 'metadata_link_local'
  | 'unspecified'
  | 'multicast'
  | 'broadcast'
  | 'deprecated_site_local'
  | 'reserved';

const GENERIC_REFUSAL =
  "Concord can never connect to that address — it's a reserved address that can't host a server. Double-check the address with your server administrator.";

// Null-prototype, deliberately. `code` and `message` are arbitrary strings off the IPC
// wire (SelfHostedProbeResult types both as `string`), so a plain object literal would
// resolve `constructor` / `toString` / `valueOf` / `hasOwnProperty` to an INHERITED
// function — not nullish, so `?? GENERIC_REFUSAL` would never fire, React would drop the
// function, and the user would see a bare glyph with no text. The `| undefined` value
// type is what makes the fallback visible to the type checker; the old `as RefusalCode`
// cast is what hid it.
export const REFUSAL_COPY: Record<string, string | undefined> = Object.assign(
  Object.create(null) as Record<string, string | undefined>,
  {
    metadata_link_local:
      "Concord can never connect to that address — it's an internal address your computer or cloud provider reserves for itself, not a server address. Double-check the address with your server administrator.",
    unspecified:
      "Concord can never connect to that address — that isn't a specific server. Enter the address your server administrator gave you.",
    multicast:
      'Concord can never connect to that address — it reaches many devices at once, not one server. Double-check the address with your server administrator.',
    broadcast:
      'Concord can never connect to that address — it reaches many devices at once, not one server. Double-check the address with your server administrator.',
    deprecated_site_local: GENERIC_REFUSAL,
    reserved: GENERIC_REFUSAL,
  } satisfies Record<RefusalCode, string>
);

type HostCopy = (host: string) => string;

/** Host-parameterised copy. Named so the §8.4 vocabulary lock can assert over it. */
export const HOST_COPY = {
  unreachable: (h: string) => `Couldn't reach ${h}. Check the address and try again.`,
  schemaFailure: (h: string) => `${h} didn't respond like a Concord server.`,
  // The renderer NEVER renders main's `message` string. FAIL_COPY covers six codes;
  // main can return at least five more (rejected, invalid_url, credentials_not_allowed,
  // unsupported_scheme, https_required), each authored in src/main/selfHostedProbe.ts —
  // a file no renderer copy rule governs. An https URL carrying embedded userinfo
  // credentials still parses as https here, so the probe fires and main's own refusal
  // string reaches the user: that path is live, not hypothetical.
  genericFail: (h: string) => `Couldn't connect to ${h}. Check the address and try again.`,
  declined: (h: string) =>
    `Connection cancelled. ${h} was not added. You can try again if this is your server.`,
  notApproved: (h: string) =>
    `${h} hasn't been approved on this device yet. Try connecting again to approve it.`,
} satisfies Record<string, HostCopy>;

const APPROVAL_PENDING_TEXT =
  'This may include a one-time confirmation — check for a dialog on your screen.';

/** Host-independent copy. Same lock surface as HOST_COPY / REFUSAL_COPY / FAIL_COPY. */
export const STATIC_COPY = {
  emptyUrl: 'Please enter a server URL',
  invalidUrl: 'Invalid server URL',
  httpsRequired: 'HTTPS is required for security (except localhost)',
  bridgeMissing: 'Self-hosted server discovery is unavailable in this app version.',
  checking: 'Checking server…',
  approvalPending: APPROVAL_PENDING_TEXT,
  probingSlow: `${APPROVAL_PENDING_TEXT} Still checking… this can take a few seconds.`,
} as const;

// Same null-prototype rationale as REFUSAL_COPY: a `code` of `toString` would otherwise
// resolve to Object.prototype.toString and render the literal `[object Undefined]`.
export const FAIL_COPY: Record<string, HostCopy | undefined> = Object.assign(
  Object.create(null) as Record<string, HostCopy | undefined>,
  {
    // Main sends an empty message for this outcome, so an unmapped code would render
    // nothing at all — a silent no-op. The copy stays mechanism-free (§8.4).
    busy: () => `Already checking a server. Wait for that to finish and try again.`,
    too_many_prompts: () => `Too many connection attempts. Wait a few minutes and try again.`,
    unreachable: HOST_COPY.unreachable,
    client_config_failed: HOST_COPY.schemaFailure,
    capabilities_failed: HOST_COPY.schemaFailure,
    approval_not_saved: () => `Concord couldn't save your choice. Try again.`,
  }
);

type ConnectState =
  | { k: 'idle' }
  | { k: 'invalid'; text: string }
  | { k: 'probing' }
  | { k: 'probing-visible' }
  | { k: 'probing-slow' }
  | { k: 'approval-pending' }
  | { k: 'declined'; host: string }
  | { k: 'not-approved'; host: string }
  | { k: 'refused'; text: string }
  | { k: 'failed'; text: string };

const PROBING_VISIBLE_MS = 250;
const APPROVAL_PENDING_MS = 3000;
const PROBING_SLOW_MS = 4000;

const STATUS_REGION_ID = 'server-url-status';

const ServerInput: React.FC<ServerInputProps> = ({ onConnect, onBack }) => {
  const [serverUrl, setServerUrl] = useState('');
  const [state, setState] = useState<ConnectState>({ k: 'idle' });
  const inputRef = useRef<HTMLInputElement>(null);
  const timersRef = useRef<ReturnType<typeof setTimeout>[]>([]);

  const clearTimers = () => {
    timersRef.current.forEach(clearTimeout);
    timersRef.current = [];
  };
  useEffect(() => clearTimers, []);

  const handleConnect = async () => {
    clearTimers();

    if (!serverUrl) {
      setState({ k: 'invalid', text: STATIC_COPY.emptyUrl });
      return;
    }

    // Prepend only when there is no scheme AT ALL. Matching on `http://`/`https://`
    // alone turned `ftp://host` into `https://ftp://host` — host `ftp`, path
    // `//host` — silently probing a different server than the user typed, and
    // leaving the protocol check below unreachable. The `://` is required so a
    // bare `localhost:8080` is still treated as host:port and gets the prefix.
    const hasScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(serverUrl);
    const fullUrl = hasScheme ? serverUrl : `https://${serverUrl}`;

    let parsed: URL;
    try {
      parsed = new URL(fullUrl);
    } catch {
      setState({ k: 'invalid', text: STATIC_COPY.invalidUrl });
      return;
    }
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
      setState({ k: 'invalid', text: STATIC_COPY.invalidUrl });
      return;
    }

    const host = parsed.hostname;
    // B2: compare the parsed host, not the URL string — the old
    // `fullUrl.includes('localhost')` substring check let
    // `http://localhost.attacker.example` through this gate. This closes THIS gate;
    // it is not a claim that every comparison in the file is exact. A prefix test has
    // the same defect as the substring one (`startsWith('127.')` admits
    // `127.example.com`), so 127/8 is matched as a full dotted quad.
    // The accepted set mirrors main's authority (`isHttpAllowedHost`,
    // src/main/selfHostedProbe.ts): `localhost`, any 127/8 literal, and the IPv6
    // loopback — which `URL` canonicalises and keeps bracketed. Omitting `[::1]`
    // blocked IPv6 self-hosters the main process supports.
    const isLoopback =
      host === 'localhost' || /^127\.\d+\.\d+\.\d+$/.test(host) || host === '[::1]';
    if (parsed.protocol === 'http:' && !isLoopback) {
      setState({ k: 'invalid', text: STATIC_COPY.httpsRequired });
      return;
    }

    setState({ k: 'probing' });
    // Elapsed-time escalation only: PR-1 adds no event, so the renderer cannot know a
    // confirmation dialog is open. Once approval-pending is reached nothing escalates
    // past it except the appended slow note, and neither reverts to probing-visible.
    timersRef.current.push(
      setTimeout(
        () => setState((s) => (s.k === 'probing' ? { k: 'probing-visible' } : s)),
        PROBING_VISIBLE_MS
      ),
      setTimeout(
        () => setState((s) => (s.k === 'probing-visible' ? { k: 'approval-pending' } : s)),
        APPROVAL_PENDING_MS
      ),
      // Only `approval-pending` can be current at 4000ms: all three timers arm in the same
      // tick (one push, arguments evaluated left to right), the 3000ms one moves
      // probing-visible on unconditionally, nothing else can transition while busy (input
      // readOnly, both buttons guarded, Enter gated on !busy), and a settled probe clears
      // the timers. A `probing-visible` arm here would be a permanently-uncovered branch
      // implying a transition the machine does not have.
      setTimeout(
        () => setState((s) => (s.k === 'approval-pending' ? { k: 'probing-slow' } : s)),
        PROBING_SLOW_MS
      )
    );

    try {
      const result = await globalThis.electron?.selfHosted?.probeServer(fullUrl);
      clearTimers();
      if (!result) {
        setState({ k: 'failed', text: STATIC_COPY.bridgeMissing });
        return;
      }
      if (result.status === 'ok') {
        onConnect(result.apiBase);
        return;
      }
      if (result.code === 'address_not_allowed') {
        // Never render `message` raw here — it is an enum token, not user-facing copy.
        setState({ k: 'refused', text: REFUSAL_COPY[result.message] ?? GENERIC_REFUSAL });
        return;
      }
      if (result.code === 'approval_declined') {
        setState({ k: 'declined', host });
        return;
      }
      // Tier 2 is the approvable case, so this is recoverable, not terminal. Main sends
      // an empty message; the renderer owns the copy.
      if (result.code === 'origin_not_approved') {
        setState({ k: 'not-approved', host });
        return;
      }
      const fail = FAIL_COPY[result.code];
      setState({ k: 'failed', text: (fail ?? HOST_COPY.genericFail)(host) });
    } catch {
      clearTimers();
      setState({ k: 'failed', text: HOST_COPY.unreachable(host) });
    }
  };

  // Focus returns to the input on the neutral, recoverable outcomes only. It must NOT
  // move while a check is in flight (WCAG 3.2.1).
  useEffect(() => {
    if (state.k === 'declined' || state.k === 'not-approved') inputRef.current?.focus();
  }, [state.k]);

  const busy =
    state.k === 'probing' ||
    state.k === 'probing-visible' ||
    state.k === 'probing-slow' ||
    state.k === 'approval-pending';
  const hasErrorBorder = state.k === 'invalid' || state.k === 'refused' || state.k === 'failed';

  const message = (() => {
    switch (state.k) {
      case 'invalid':
        return { glyph: '⚠', className: 'input-error', text: state.text };
      case 'probing-visible':
        return { glyph: 'spinner', className: 'input-status', text: STATIC_COPY.checking };
      case 'approval-pending':
        return { glyph: '⏳', className: 'input-status', text: STATIC_COPY.approvalPending };
      case 'probing-slow':
        return { glyph: '⏳', className: 'input-status', text: STATIC_COPY.probingSlow };
      case 'declined':
        return { glyph: 'ℹ', className: 'input-note', text: HOST_COPY.declined(state.host) };
      case 'not-approved':
        return { glyph: 'ℹ', className: 'input-note', text: HOST_COPY.notApproved(state.host) };
      case 'refused':
        return { glyph: '⛔', className: 'input-error', text: state.text };
      case 'failed':
        return { glyph: '⚠', className: 'input-error', text: state.text };
      default:
        return null;
    }
  })();

  return (
    <div className="server-input">
      <div className="server-content">
        {/* Header */}
        <div className="server-header">
          <img
            src="./branding/Concord-Voice/logos/symbol-transparent-vector.svg"
            alt="Concord Voice"
            className="server-icon"
          />
          <h2 className="server-title">Connect to Self-Hosted Server</h2>
          <p className="server-subtitle">Enter the address provided by your server administrator</p>
        </div>

        {/* URL Input */}
        <div className="server-form">
          <div className="input-group">
            <label htmlFor="server-url" className="input-label">
              Server URL
            </label>
            {/* The red border is colour-only, and this input is programmatically focused
                on two outcomes — so the error state is also exposed via aria-invalid and
                the status region is bound with aria-describedby. A screen-reader user who
                tabs back after the polite announcement has passed then still gets it. */}
            <input
              id="server-url"
              ref={inputRef}
              type="text"
              className={`server-url-input ${hasErrorBorder ? 'error' : ''}`}
              placeholder="https://concord.myserver.com"
              value={serverUrl}
              readOnly={busy}
              aria-invalid={hasErrorBorder || undefined}
              aria-describedby={STATUS_REGION_ID}
              onChange={(e) => {
                setServerUrl(e.target.value);
                if (state.k !== 'idle' && !busy) setState({ k: 'idle' });
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && serverUrl && !busy) void handleConnect();
              }}
              autoFocus
            />
            {/* One live region with swapped content — three siblings would duplicate or
                drop announcements. */}
            <div
              id={STATUS_REGION_ID}
              role="status"
              aria-live="polite"
              className="input-status-region"
            >
              {message && (
                <div className={message.className}>
                  {message.glyph === 'spinner' ? (
                    <span className="server-spinner" aria-hidden="true" />
                  ) : (
                    <span aria-hidden="true">{message.glyph}</span>
                  )}{' '}
                  <span className="input-message-text">{message.text}</span>
                </div>
              )}
            </div>
          </div>

          {/* Connect Button */}
          {/* `busy` is aria-disabled + an activation guard, never the native `disabled`
              attribute: disabling the button the keyboard user just activated blurs it and
              drops focus to <body> for the whole probe — including the modal ceremony —
              and the refused/failed outcomes never restore it. Same shape as the
              FontSection dyslexic hard-lock ([internal]rules/frontend.md § Application
              font). The at-rest empty-URL case stays a real `disabled`: nothing has
              focus to lose there. */}
          <button
            className="server-connect-btn"
            onClick={() => {
              if (busy) return;
              void handleConnect();
            }}
            disabled={!serverUrl}
            aria-disabled={busy || undefined}
            aria-busy={busy}
          >
            {busy ? 'Connecting…' : 'Connect to Server'}
          </button>

          {/* Back Button */}
          <button
            className="server-back-btn"
            onClick={() => {
              if (busy) return;
              onBack();
            }}
            aria-disabled={busy || undefined}
          >
            ← Back to Connection Options
          </button>

          {/*
           * Retained deliberately. The design spec (§8.5) scopes this footer OUT of
           * #2354: the `help-link` span is inert (it looks like a link and does
           * nothing), but that is a separate defect from the B1–B5 boy-scout set and
           * fixing it properly means deciding what it should point at — a content
           * call, not part of an SSRF fix. Removing it here would be unauthorized
           * user-visible content loss on the very screen this PR redesigns.
           */}
          <div className="server-footer">
            <p className="footer-help">
              Don&apos;t have a server?{' '}
              <span className="help-link">Learn about self-hosting →</span>
            </p>
          </div>
        </div>
      </div>
    </div>
  );
};

export default ServerInput;
