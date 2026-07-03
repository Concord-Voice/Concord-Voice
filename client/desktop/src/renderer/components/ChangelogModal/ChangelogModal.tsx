import { useEffect, useMemo, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import rehypeSanitize from 'rehype-sanitize';
import changelogRaw from 'virtual:concord-changelog';
import { sanitizeSchema } from '../Markdown/sanitizeSchema';
import SafeLink from '../Markdown/SafeLink';
import {
  parseChangelog,
  sectionsBetween,
  compareSemver,
  decideChangelogAction,
  type ChangelogSection,
} from '../../services/changelog';
import { useChangelogStore } from '../../stores/changelogStore';
import { useAuthStore } from '../../stores/authStore';
import { useAttestationFailureStore } from '../../stores/attestationFailureStore';
import { useClientConfigStore } from '../../stores/clientConfigStore';
import './ChangelogModal.css';

/** Public releases page (same public-repo target as UpdateSecurityBanner). */
const FULL_NOTES_URL = 'https://github.com/Concord-Voice/Concord-Voice/releases';

/** Newest sections rendered in full; the rest collapse to an "earlier versions" line. */
const MAX_RENDERED_SECTIONS = 3;

// Module-scope anchor override (same shape as MarkdownContent's MarkdownA —
// defined at module scope so it isn't redeclared every render).
interface ChangelogAProps {
  href?: string;
  title?: string;
  children?: React.ReactNode;
}
const ChangelogA: React.FC<ChangelogAProps> = ({ href, title, children }) => (
  <SafeLink href={href} title={title}>
    {children}
  </SafeLink>
);

// Changelog-scoped Markdown pipeline: GFM + strict sanitize + SafeLink anchors.
// Deliberately OMITS the chat pipeline's mention/emoji/spoiler plugins so
// changelog prose like "@mention routing" is never tokenized into MentionChips.
// rehype-sanitize with the shared strict schema remains the XSS defense; no raw
// HTML passes through.
function ChangelogMarkdown({ content }: Readonly<{ content: string }>) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      rehypePlugins={[[rehypeSanitize, sanitizeSchema]]}
      components={{ a: ChangelogA }}
    >
      {content}
    </ReactMarkdown>
  );
}

// ── Presentational component ───────────────────────────────────────────────

export interface ChangelogModalProps {
  currentVersion: string;
  sections: ChangelogSection[];
  onDismiss: () => void;
}

export function ChangelogModal({
  currentVersion,
  sections,
  onDismiss,
}: Readonly<ChangelogModalProps>) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const closeButtonRef = useRef<HTMLButtonElement>(null);

  // Native <dialog> modal behavior: focus trap, ::backdrop, Escape-to-close
  // (mirrors AttestationFailedModal). Initial focus is placed on the close
  // button imperatively — deterministic and avoids the autoFocus a11y lint.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg || dlg.open) return;
    dlg.showModal();
    closeButtonRef.current?.focus();
  }, []);

  // Bridge the native 'close' event (Escape / dlg.close()) to onDismiss.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg) return;
    const handleClose = () => onDismiss();
    dlg.addEventListener('close', handleClose);
    return () => {
      dlg.removeEventListener('close', handleClose);
    };
  }, [onDismiss]);

  const visible = sections.slice(0, MAX_RENDERED_SECTIONS);
  const earlierCount = sections.length - visible.length;
  const releaseWord = sections.length === 1 ? 'release' : 'releases';
  const summary =
    sections.length > 0
      ? `Highlights from ${sections.length} ${releaseWord} since your previous version.`
      : `You've been updated to v${currentVersion}. Detailed notes for this range aren't available here — see the full release notes.`;

  return (
    <div className="changelog-modal-overlay">
      <dialog
        ref={dialogRef}
        aria-modal="true"
        aria-labelledby="changelog-modal-title"
        aria-describedby="changelog-modal-summary"
        className="changelog-modal"
      >
        <h2 id="changelog-modal-title">What&apos;s new in v{currentVersion}</h2>
        <p id="changelog-modal-summary">{summary}</p>
        {visible.length > 0 && (
          // Long release notes scroll. Native <section aria-label> carries the
          // implicit region role (Sonar S6819/S6845 — no role/tabIndex props);
          // keyboard scrolling rides the focusable PR links every section body
          // carries, so focus moves the scroll position through the overflow.
          <section className="changelog-modal__notes" aria-label="Release notes">
            {visible.map((s) => (
              <section key={s.version} className="changelog-modal__section">
                <h3>
                  v{s.label}
                  {s.date && <span className="changelog-modal__date"> — {s.date}</span>}
                </h3>
                <div className="changelog-modal__section-body">
                  <ChangelogMarkdown content={s.body} />
                </div>
              </section>
            ))}
            {earlierCount > 0 && (
              <p className="changelog-modal__earlier">
                …and {earlierCount} earlier version{earlierCount === 1 ? '' : 's'}.
              </p>
            )}
          </section>
        )}
        <div className="changelog-modal__actions">
          <SafeLink href={FULL_NOTES_URL} title="View full release notes">
            View full release notes
          </SafeLink>
          <button
            ref={closeButtonRef}
            type="button"
            className="btn btn-primary"
            onClick={onDismiss}
          >
            Got it
          </button>
        </div>
      </dialog>
    </div>
  );
}

// ── Store-connected host (default export) ──────────────────────────────────

/**
 * ChangelogModalHost — mounts once in App.tsx alongside the other global
 * overlays. Runs the design-spec §5.1 decision table on the authenticated
 * shell only, and yields to security-critical overlays (attestation failure,
 * force-update).
 */
export default function ChangelogModalHost() {
  const accessToken = useAuthStore((s) => s.accessToken);
  const emailVerified = useAuthStore((s) => s.emailVerified);
  const attestationVisible = useAttestationFailureStore((s) => s.visible);
  const minVersion = useClientConfigStore((s) => s.minVersion);
  const lastSeenVersion = useChangelogStore((s) => s.lastSeenVersion);
  const markSeen = useChangelogStore((s) => s.markSeen);
  const [currentVersion, setCurrentVersion] = useState<string | null>(null);
  const [dismissed, setDismissed] = useState(false);

  useEffect(() => {
    // Optional-chain through the bridge call: when the preload bridge is
    // absent the whole chain short-circuits (no Promise in a conditional).
    globalThis.electron?.getVersion?.().then(setCurrentVersion, () => {
      // Version unavailable (bridge absent/failed) — modal stays hidden this session.
    });
  }, []);

  const authenticated = !!accessToken && emailVerified;
  const decision = useMemo(
    () => (currentVersion ? decideChangelogAction(lastSeenVersion, currentVersion) : null),
    [currentVersion, lastSeenVersion]
  );

  // 'record' rows write the seen version silently (fresh install, downgrade,
  // corrupt stored value) — an effect, not a render-time write.
  useEffect(() => {
    if (authenticated && currentVersion && decision?.kind === 'record') {
      markSeen(currentVersion);
    }
  }, [authenticated, currentVersion, decision, markSeen]);

  const sections = useMemo(() => {
    if (!currentVersion || decision?.kind !== 'show' || lastSeenVersion === null) return [];
    return sectionsBetween(parseChangelog(changelogRaw), lastSeenVersion, currentVersion);
  }, [currentVersion, decision, lastSeenVersion]);

  if (!authenticated) return null;
  if (attestationVisible) return null;
  // Force-update pending (current < minVersion): the mandatory-update overlay
  // owns the screen; never stack the changelog above it.
  if (currentVersion && minVersion && (compareSemver(currentVersion, minVersion) ?? 0) < 0) {
    return null;
  }
  if (dismissed || !currentVersion || decision?.kind !== 'show') {
    // Inert marker so tests can await "host settled, no modal".
    return <span data-testid="changelog-host-idle" hidden />;
  }

  return (
    <ChangelogModal
      currentVersion={currentVersion}
      sections={sections}
      onDismiss={() => {
        markSeen(currentVersion);
        setDismissed(true);
      }}
    />
  );
}
