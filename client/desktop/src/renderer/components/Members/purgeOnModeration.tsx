import { useState } from 'react';
import { useMemberStore, type ServerMember } from '../../stores/chat/memberStore';
import { safeJson } from '../../services/system/apiClient';
import { captureApiRequestContext } from '../../services/system/requestContext';
import {
  includePinnedFor,
  PIN_CLAIM_UNCONFIRMED_MESSAGE,
  pinModeFor,
  sendPinClaim,
  type PinMode,
} from '../../services/messaging/purgeApi';
import { usePurgeKeepsPinnedAtOpen } from '../../hooks/messaging/usePurgeKeepsPinnedAtOpen';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import './purgeOnModeration.css';

export { PIN_CLAIM_UNCONFIRMED_MESSAGE };

/**
 * The kick/ban purge opt-in (#1354), shared by the two surfaces that can ban or
 * kick a member: the member sidebar (`Members/MemberList`) and the server
 * settings member list (`Servers/MemberListPanel`). Both hit the same endpoints,
 * so both offer the same checkbox, send the same body, and speak the same copy.
 */

/**
 * The additive purge sub-outcome the kick/ban endpoints report. It is present in
 * the response ONLY when the purge was requested.
 */
export interface ModerationPurgeOutcome {
  requested: boolean;
  status: string;
  purged_count: number;
}

/**
 * What a caller should announce. An empty `notice` is deliberately ambiguous on
 * its own — it is both "no purge was requested" and "a purge happened that this
 * client cannot describe" — so `unknownStatus` separates the two. Callers render
 * nothing either way today; the flag exists so the distinction is available to
 * whoever needs it (diagnostics, a future fallback line) rather than lost.
 */
export interface PurgeNoticeResult {
  notice: string;
  unknownStatus: boolean;
}

/**
 * Copy for that sub-outcome. The ban/kick commits first and the purge is
 * best-effort, so every line leads with the moderation action having succeeded
 * and all four statuses read as notices, never errors.
 *
 * `skipped_rate_limited` is deliberately vaguer than the standalone purge
 * copy: the moderation path cannot distinguish a spent quota from a Redis
 * error, and the budget is operator-tunable — so the line asserts neither.
 */
export function purgeNotice(
  name: string,
  verb: 'banned' | 'kicked',
  status: string,
  pinMode: PinMode
): PurgeNoticeResult {
  switch (status) {
    case 'completed':
      return {
        notice:
          pinMode === 'keep'
            ? `${name} was ${verb} and their messages were purged. Pinned messages were kept.`
            : `${name} was ${verb} and their messages were purged.`,
        unknownStatus: false,
      };
    case 'skipped_unauthorized':
      return {
        notice: `${name} was ${verb}. Their messages were not purged — you do not have permission to purge messages in this server.`,
        unknownStatus: false,
      };
    case 'skipped_rate_limited':
      return {
        notice: `${name} was ${verb}. Their messages were not purged — the purge limit was not available just now. You can purge them from a channel later.`,
        unknownStatus: false,
      };
    case 'failed':
      return {
        notice: `${name} was ${verb}. Their messages could not be purged. You can try again from a channel.`,
        unknownStatus: false,
      };
    default:
      // A status this client does not know cannot be described honestly, and
      // the ban/kick itself succeeded — say nothing rather than guess.
      return { notice: '', unknownStatus: true };
  }
}

const PURGE_SUMMARY: Record<PinMode, string> = {
  keep: 'Their messages will be permanently removed from every channel you can moderate, except pinned messages.',
  include:
    'Their messages, including pinned messages, will be permanently removed from every channel you can moderate.',
  unsupported: 'Their messages will be permanently removed from every channel you can moderate.',
};

/**
 * The kick and ban request structs carry only `purge_messages` and
 * `include_pinned`, so these are checkboxes and nothing more — a range picker
 * would promise a choice the API does not accept. Absent on Leave Server:
 * self-removal never purges. The pinned choice appears only under a checked
 * opt-in on a server that keeps pins (#3458); unchecking the opt-in clears it.
 */
export function PurgeMessagesOptIn({
  checked,
  onChange,
  pinMode,
  onIncludePinnedChange,
}: Readonly<{
  checked: boolean;
  onChange: (next: boolean) => void;
  pinMode: PinMode;
  onIncludePinnedChange: (next: boolean) => void;
}>) {
  return (
    <div className="member-purge-optin">
      <label className="member-purge-optin__row">
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => {
            onChange(e.target.checked);
            if (!e.target.checked) onIncludePinnedChange(false);
          }}
        />
        <span>Also purge their messages in this server</span>
      </label>
      {checked && pinMode !== 'unsupported' && (
        <label className="member-purge-optin__row member-purge-optin__pinned">
          <input
            type="checkbox"
            checked={pinMode === 'include'}
            onChange={(e) => onIncludePinnedChange(e.target.checked)}
          />
          <span>Include pinned messages</span>
        </label>
      )}
      {checked && <p className="member-purge-optin__summary">{PURGE_SUMMARY[pinMode]}</p>}
    </div>
  );
}

type ModerationAction = 'ban' | 'kick';

const MODERATION_COPY: Record<
  ModerationAction,
  { verb: string; message: string; purgeLabel: string; loadingLabel: string }
> = {
  ban: {
    verb: 'Ban',
    message: 'This will permanently remove them from the server and prevent them from rejoining.',
    purgeLabel: 'Ban and purge',
    loadingLabel: 'Banning...',
  },
  kick: {
    verb: 'Kick',
    message: 'This will remove them from the server. They can rejoin with a new invite.',
    purgeLabel: 'Kick and purge',
    loadingLabel: 'Kicking...',
  },
};

/**
 * The ban or kick confirmation, open while `target` is set. It owns the purge
 * opt-in, resets it on close, and samples the pinned-message capability when it
 * opens (#3458), so `onConfirm` receives exactly what the dialog showed.
 */
export function ModerationConfirmModal({
  action,
  target,
  onClose,
  onConfirm,
}: Readonly<{
  action: ModerationAction;
  target: ServerMember | null;
  onClose: () => void;
  onConfirm: (
    target: ServerMember,
    action: ModerationAction,
    alsoPurge: boolean,
    pinMode: PinMode
  ) => Promise<void>;
}>) {
  const [alsoPurge, setAlsoPurge] = useState(false);
  const [includePinned, setIncludePinned] = useState(false);
  const pinMode = pinModeFor(usePurgeKeepsPinnedAtOpen(target !== null), includePinned);
  const copy = MODERATION_COPY[action];
  return (
    <ConfirmActionModal
      isOpen={target !== null}
      onClose={() => {
        setAlsoPurge(false);
        setIncludePinned(false);
        onClose();
      }}
      title={`${copy.verb} ${target?.display_name || target?.username || 'User'}`}
      message={copy.message}
      extraContent={
        <PurgeMessagesOptIn
          checked={alsoPurge}
          onChange={setAlsoPurge}
          pinMode={pinMode}
          onIncludePinnedChange={setIncludePinned}
        />
      }
      // Degrades gracefully: an unchecked box never blocks the ban or kick.
      confirmLabel={alsoPurge ? copy.purgeLabel : copy.verb}
      loadingLabel={copy.loadingLabel}
      onConfirm={async () => {
        if (target) await onConfirm(target, action, alsoPurge, pinMode);
      }}
    />
  );
}

/**
 * One request shape for both moderation actions on both surfaces: the endpoints
 * differ only in method and path, and both accept the same optional
 * `purge_messages` and `include_pinned` body.
 *
 * Returns the purge notice the caller should announce — empty when no purge was
 * requested, or when the server reported a status this client does not know
 * (`unknownStatus` tells those two apart). `ConfirmActionModal` closes itself on
 * success, so the notice belongs to the calling component's own `role="status"`
 * region rather than to the modal.
 *
 * `alsoPurge` rather than `purgeMessages`: the latter is the name of the purge
 * service function exported from `services/messaging/purgeApi.ts`, and this module — or
 * either of its two callers — importing it would shadow the parameter.
 */
export async function moderateMember(
  serverId: string,
  target: ServerMember,
  action: ModerationAction,
  alsoPurge: boolean,
  pinMode: PinMode
): Promise<PurgeNoticeResult> {
  const isBan = action === 'ban';
  // The account and server that confirmed, captured before the recheck's
  // request: a change while it is in flight refuses the action rather than
  // sending it as the new account (#3552 review).
  const operation = captureApiRequestContext();
  // A purge that promises something about pins is rechecked right before the
  // request: a rollback since the dialog opened would delete them anyway
  // (#3552 review). Without a purge there is no claim to check.
  const res = await sendPinClaim(
    isBan
      ? `/api/v1/servers/${serverId}/bans/${target.user_id}`
      : `/api/v1/servers/${serverId}/members/${target.user_id}`,
    {
      method: isBan ? 'POST' : 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        purge_messages: alsoPurge,
        include_pinned: includePinnedFor(pinMode),
      }),
    },
    operation,
    alsoPurge ? pinMode : 'unsupported'
  );
  if (res === null) throw new Error(PIN_CLAIM_UNCONFIRMED_MESSAGE);
  if (!res.ok) {
    // `safeJson` throws on a non-JSON content-type as well as on a parse
    // failure, so an HTML 502 from a proxy would otherwise surface its own
    // message ("Expected JSON but got text/html…") to the user in place of ours.
    const data = await safeJson<{ error?: string }>(res).catch(() => null);
    throw new Error(data?.error || (isBan ? 'Ban failed' : 'Kick failed'));
  }
  useMemberStore.getState().removeMember(target.user_id);

  // Best-effort: the moderation action has already committed, so a response we
  // cannot parse must never surface as a failed ban or kick.
  let body: { purge?: ModerationPurgeOutcome } | null = null;
  try {
    body = await safeJson<{ purge?: ModerationPurgeOutcome }>(res);
  } catch {
    body = null;
  }
  // No purge fragment means none was requested — nothing to say, and nothing
  // undescribable happened.
  if (!body?.purge) return { notice: '', unknownStatus: false };
  const verb = isBan ? 'banned' : 'kicked';
  return purgeNotice(target.display_name || target.username, verb, body.purge.status, pinMode);
}
