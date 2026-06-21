// CallEventMessage — renders dm_messages rows where type='call_event'
// (#1209 plan task F6 component half). The payload comes from the
// dm_messages.call_event_payload JSONB column populated by the
// insertCallEvent helper in services/control-plane/internal/dm/call_events.go.
//
// Wired into MessageList by #1219 (R7): the backend message-fetch serializer
// now returns the `type` + `call_event_payload` columns and the Message
// interface (types/chat.ts) carries them. MessageList dispatches call_event
// rows here; useMessageFetch skips the E2EE decrypt pass for these rows.
// Group calls (#1219) render "Group voice call — M:SS" with a joiner tooltip.
//
// Per spec §6.4 (post-pivot 2026-05-28): payload is stored as plaintext
// JSONB. No client-side decryption needed.

import './CallEventMessage.css';
import type { CallEventPayload } from '../../types/chat';

// Re-export the centralized types (#1219) via `export...from` so existing
// importers (e.g. CallEventMessage.test.tsx) keep working off this module path
// without an import-then-re-export round-trip (sonar typescript:S7763).
export type { CallEventPayload, CallEventStatus } from '../../types/chat';

interface CallEventMessageProps {
  payload: CallEventPayload;
  /** Render group-call wording ("Group voice call — M:SS") + joiner tooltip
   *  for completed calls. Defaults to the 1:1 wording. (#1219) */
  isGroup?: boolean;
}

function formatDuration(seconds: number): string {
  if (seconds <= 0) return '0:00';
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return `${m}:${s.toString().padStart(2, '0')}`;
}

function formatTime(iso: string): string {
  // `new Date(invalidString)` doesn't throw — it returns an Invalid Date
  // whose toLocaleTimeString yields "Invalid Date". Validate explicitly so
  // the renderer shows empty rather than a confusing literal string
  // (Copilot #1231 finding C12).
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
}

export function CallEventMessage({ payload, isGroup }: Readonly<CallEventMessageProps>) {
  const { status, duration_seconds, started_at, participant_user_ids } = payload;

  let text: string;
  switch (status) {
    case 'completed':
      text = isGroup
        ? `Group voice call — ${formatDuration(duration_seconds)}`
        : `Voice call — ${formatDuration(duration_seconds)}`;
      break;
    case 'missed':
      text = 'Missed voice call';
      break;
    case 'declined':
      text = 'Voice call declined';
      break;
    case 'canceled':
      text = 'Voice call canceled';
      break;
  }

  // Group joiner tooltip is sourced from participant_user_ids (a first-joiner
  // approximation per spec C2), NOT caller_user_id. Undefined for 1:1 / empty.
  const joinerTooltip =
    isGroup && participant_user_ids && participant_user_ids.length > 0
      ? participant_user_ids.join(', ')
      : undefined;

  const iconColor = status === 'completed' ? 'var(--success, #22c55e)' : 'var(--text-muted, #aab)';

  return (
    <li className="call-event-message">
      <svg
        className="call-event-message__icon"
        width="16"
        height="16"
        viewBox="0 0 16 16"
        fill="none"
        style={{ color: iconColor }}
        aria-hidden="true"
      >
        <path
          d="M3 4a1 1 0 011-1h2.28a1 1 0 01.95.68l1 3a1 1 0 01-.27 1.03L5.6 9.31a8 8 0 003.09 3.09l1.6-1.36a1 1 0 011.03-.27l3 1a1 1 0 01.68.95V15a1 1 0 01-1 1h-1C7.4 16 0 8.6 0 0v-1z"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinejoin="round"
          transform="translate(0.5, 0)"
        />
      </svg>
      <span className="call-event-message__text" title={joinerTooltip}>
        {text}
      </span>
      <span className="call-event-message__time">{formatTime(started_at)}</span>
    </li>
  );
}
