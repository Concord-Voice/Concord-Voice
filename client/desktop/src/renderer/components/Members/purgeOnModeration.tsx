import { useRef, useState } from 'react';
import { useMemberStore, type ServerMember } from '../../stores/chat/memberStore';
import type { VerificationReturn } from '../../stores/ui/settingsOverlayStore';
import { safeJson } from '../../services/system/apiClient';
import type { ApiRequestContext } from '../../services/system/requestContext';
import {
  describeFailureWith,
  resendWithCode,
  sendFirst,
  type Dispatch,
  type FrozenRequest,
} from '../../services/system/dangerousActionRequest';
import {
  includePinnedFor,
  PIN_CLAIM_UNCONFIRMED_MESSAGE,
  pinModeFor,
  sendPinClaim,
  type PinMode,
} from '../../services/messaging/purgeApi';
import { usePurgeKeepsPinnedAtOpen } from '../../hooks/messaging/usePurgeKeepsPinnedAtOpen';
import { useStepUpHandoff } from '../../hooks/auth/useStepUpHandoff';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import DangerousActionStepUpDialog from '../Auth/DangerousActionStepUpDialog';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
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

export type ModerationAction = 'ban' | 'kick';

/**
 * One request shape for both moderation actions on both surfaces: the endpoints
 * differ only in method and path, and both accept the same optional
 * `purge_messages` and `include_pinned` body (and, on a re-send after
 * verification, `mfa_code`).
 */
function moderationRequest(
  serverId: string,
  target: ServerMember,
  action: ModerationAction,
  alsoPurge: boolean,
  pinMode: PinMode
): FrozenRequest {
  const body = { purge_messages: alsoPurge, include_pinned: includePinnedFor(pinMode) };
  return action === 'ban'
    ? { path: `/api/v1/servers/${serverId}/bans/${target.user_id}`, method: 'POST', body }
    : { path: `/api/v1/servers/${serverId}/members/${target.user_id}`, method: 'DELETE', body };
}

/**
 * The purge sub-outcome of a committed ban or kick. Best-effort: the moderation
 * action has already committed, so a response we cannot parse must never surface
 * as a failed ban or kick.
 */
async function readPurgeOutcome(res: Response): Promise<ModerationPurgeOutcome | undefined> {
  try {
    const body = await safeJson<{ purge?: ModerationPurgeOutcome }>(res);
    return body?.purge;
  } catch {
    return undefined;
  }
}

/**
 * What a committed ban or kick does locally. No purge fragment means none was
 * requested — nothing to say, and nothing undescribable happened.
 */
function settleModeration(
  target: ServerMember,
  action: ModerationAction,
  purge: ModerationPurgeOutcome | undefined,
  pinMode: PinMode
): PurgeNoticeResult {
  useMemberStore.getState().removeMember(target.user_id);
  if (!purge) return { notice: '', unknownStatus: false };
  const verb = action === 'ban' ? 'banned' : 'kicked';
  return purgeNotice(memberName(target), verb, purge.status, pinMode);
}

/**
 * Every send of a moderation request, the code-less first one and the
 * verified re-send alike, goes through the pin claim (#3458): a purge that
 * promises to keep pins is rechecked right before the request, because a server
 * rolled back since the dialog opened would delete them anyway (#3552 review).
 * Without a purge there is no claim to check. Null: nothing was sent.
 */
function pinClaimDispatch(alsoPurge: boolean, pinMode: PinMode): Dispatch {
  const claim = alsoPurge ? pinMode : 'unsupported';
  return (path, init, context) => sendPinClaim(path, init, context, claim);
}

/** `User` stands in for a member the roster no longer names. */
function memberName(member: ServerMember | null | undefined): string {
  return member?.display_name || member?.username || 'User';
}

export type ModerationResult =
  | { kind: 'done'; notice: PurgeNoticeResult }
  /**
   * The server wants the actor verified first. `request` is what was sent,
   * frozen, and `context` the account and server it went out as.
   */
  | {
      kind: 'stepUp';
      request: FrozenRequest;
      refusal: StepUpFactorRefusal;
      context: ApiRequestContext;
    };

/**
 * Sends a ban or kick with no code. Returns the purge notice the caller should
 * announce — empty when no purge was requested, or when the server reported a
 * status this client does not know (`unknownStatus` tells those two apart) —
 * or, on a server that enforces MFA for dangerous actions, the refusal that
 * hands the frozen request to `ModerationDialog`'s step-up (#3456). A kick
 * without a purge is not gated, so only a lack of enrolment opens that.
 * `ConfirmActionModal` closes itself on success, so the notice belongs to the
 * calling component's own `role="status"` region rather than to the modal.
 *
 * Any other refusal throws an `Error` carrying the server's sentence, or ours
 * when it sent none. A non-JSON body (an HTML 502 from a proxy) must not put
 * its own parse message in front of the user in place of that.
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
): Promise<ModerationResult> {
  const request = moderationRequest(serverId, target, action, alsoPurge, pinMode);
  const failure = action === 'ban' ? 'Ban failed' : 'Kick failed';
  const first = await sendFirst(request, failure, {
    gated: action === 'ban' || alsoPurge,
    dispatch: pinClaimDispatch(alsoPurge, pinMode),
    unsentMessage: PIN_CLAIM_UNCONFIRMED_MESSAGE,
  });
  if (first.kind === 'stepUp') {
    return { kind: 'stepUp', request, refusal: first.refusal, context: first.context };
  }
  const purge = await readPurgeOutcome(first.response);
  return { kind: 'done', notice: settleModeration(target, action, purge, pinMode) };
}

interface ModerationCopy {
  verb: string;
  message: string;
  loadingLabel: string;
  purpose: StepUpPurpose;
  describeFailure: (status: number, body: unknown) => string;
}

const MODERATION_COPY: Record<ModerationAction, ModerationCopy> = {
  ban: {
    verb: 'Ban',
    message: 'This will permanently remove them from the server and prevent them from rejoining.',
    loadingLabel: 'Banning...',
    purpose: 'members.ban',
    describeFailure: describeFailureWith('Ban failed'),
  },
  kick: {
    verb: 'Kick',
    message: 'This will remove them from the server. They can rejoin with a new invite.',
    loadingLabel: 'Kicking...',
    purpose: 'members.kick_purge',
    describeFailure: describeFailureWith('Kick failed'),
  },
};

/** What the verification dialog is for, frozen with the request it will re-send. */
interface PendingModeration {
  request: FrozenRequest;
  refusal: StepUpFactorRefusal;
  context: ApiRequestContext;
  target: ServerMember;
  alsoPurge: boolean;
  /** What the confirmation showed about pins, and the claim every send rechecks. */
  pinMode: PinMode;
}

interface ModerationDialogProps {
  action: ModerationAction;
  serverId: string;
  /** The member being moderated; null while no confirmation is open. */
  target: ServerMember | null;
  /** Where "Set up verification" comes back to: the chat, or the Server Settings section this sits in. */
  returnTo: VerificationReturn;
  /** The notice to announce once the action has committed. */
  onNotice: (notice: string) => void;
  /** The action is over, however it ended: the host forgets its target. */
  onEnd: () => void;
  /** Where focus goes when the member row that opened the menu is gone. */
  focusFallback: () => HTMLElement | null;
}

/**
 * The confirmation for a ban or a kick, shared by the member sidebar and the
 * server settings member list, and the verification that stands in its place
 * when a server enforcing MFA refuses the first send (#3456 §3.4, D-5).
 *
 * The confirmation owns the purge opt-in. The first send carries it, and the
 * dialog re-sends that same frozen request with `mfa_code`, so a checkbox
 * flipped meanwhile could not change what is verified.
 */
export function ModerationDialog({
  action,
  serverId,
  target,
  returnTo,
  onNotice,
  onEnd,
  focusFallback,
}: Readonly<ModerationDialogProps>) {
  const copy = MODERATION_COPY[action];
  const [alsoPurge, setAlsoPurge] = useState(false);
  const [includePinned, setIncludePinned] = useState(false);
  // Sampled when the confirmation opens (#3458), so the send carries exactly
  // what the dialog showed.
  const pinMode = pinModeFor(usePurgeKeepsPinnedAtOpen(target !== null), includePinned);
  const purgeOutcomeRef = useRef<ModerationPurgeOutcome | undefined>(undefined);

  const { pending, ending, handOff, confirmClosed, endStepUp } =
    useStepUpHandoff<PendingModeration>(() => {
      setAlsoPurge(false);
      setIncludePinned(false);
      onEnd();
    });

  const confirm = async () => {
    if (!target) return;
    const result = await moderateMember(serverId, target, action, alsoPurge, pinMode);
    if (result.kind === 'done') {
      onNotice(result.notice.notice);
      return;
    }
    const { request, refusal, context } = result;
    handOff({ request, refusal, context, target, alsoPurge, pinMode });
  };

  const send = async (mfaCode: string | undefined, context: ApiRequestContext) => {
    if (pending === null) return { kind: 'aborted' as const };
    const result = await resendWithCode(
      pending.request,
      mfaCode,
      context,
      pinClaimDispatch(pending.alsoPurge, pending.pinMode),
      PIN_CLAIM_UNCONFIRMED_MESSAGE
    );
    if (result.kind === 'ok') purgeOutcomeRef.current = await readPurgeOutcome(result.response);
    return result;
  };

  const succeeded = () => {
    if (pending !== null) {
      const { target: settled, pinMode: settledPins } = pending;
      onNotice(settleModeration(settled, action, purgeOutcomeRef.current, settledPins).notice);
    }
    purgeOutcomeRef.current = undefined;
    endStepUp();
  };

  const purging = pending?.alsoPurge ?? false;
  let purgeClause = '';
  if (purging) {
    purgeClause =
      pending?.pinMode === 'include'
        ? ' and purge their messages, pinned messages included'
        : ' and purge their messages';
  }
  return (
    <>
      <ConfirmActionModal
        // Not during the hand-off's ending commit: reopened for that one commit,
        // the confirmation would take focus and drop it to <body> as onEnd closes it.
        isOpen={target !== null && pending === null && !ending}
        onClose={confirmClosed}
        title={`${copy.verb} ${memberName(target)}`}
        message={copy.message}
        extraContent={
          <PurgeMessagesOptIn
            checked={alsoPurge}
            onChange={setAlsoPurge}
            pinMode={pinMode}
            onIncludePinnedChange={setIncludePinned}
          />
        }
        // Degrades gracefully: an unchecked box never blocks the action.
        confirmLabel={alsoPurge ? `${copy.verb} and purge` : copy.verb}
        loadingLabel={copy.loadingLabel}
        onConfirm={confirm}
      />
      <DangerousActionStepUpDialog
        isOpen={pending !== null}
        purpose={copy.purpose}
        seed={pending?.refusal}
        intro={`This server asks you to verify before you ${copy.verb.toLowerCase()} ${memberName(pending?.target)}${purgeClause}.`}
        primaryLabel={purging ? `${copy.verb} and purge` : copy.verb}
        busyLabel={copy.loadingLabel}
        send={send}
        capture={pending?.context}
        describeFailure={copy.describeFailure}
        onSuccess={succeeded}
        onClose={endStepUp}
        onSetUpVerification={() => {
          void openVerificationSetup({ returnTo, closeHost: endStepUp });
        }}
        focusFallback={focusFallback}
      />
    </>
  );
}
