import { useId, type Ref } from 'react';
import type {
  ExpirationPolicy,
  ExpirationWindowSeconds,
} from '../../services/messaging/expirationPolicyApi';
import { useExpirationPinnedPolicyUnsupported } from '../../stores/ui/clientConfigStore';

export interface PinnedRetentionAvailability {
  policy: ExpirationPolicy | null;
  policyState: 'loading' | 'ready' | 'unavailable';
  /** The window the editor is showing right now: the staged one when something is staged,
   *  else the applied one. `null` is Off; `undefined` is "not known yet". */
  effectiveWindow: ExpirationWindowSeconds | null | undefined;
  /** `useExpirationPinnedPolicyUnsupported()`: the server is CONFIRMED not to take the field. */
  capabilityUnsupported: boolean;
}

/**
 * Whether the "Include pinned messages" switch exists at all (#3459). All four must hold:
 *
 *  - the policy is `ready` (an unreadable one has no value to show);
 *  - the policy carries a BOOLEAN `includePinned`, because the switch's start value is the
 *    server's, and a control seeded from a guess could turn pinned expiry on by accident;
 *  - the capability is not confirmed-unsupported. This fails OPEN: a capabilities fetch that is
 *    still loading or has failed says nothing, and hiding a safe control behind a transient
 *    blip is worse than showing it, since the field's own presence already proves the server;
 *  - the shown window is non-null. With no timer there is nothing for pins to be excepted from.
 *
 * Exported separately from the component because the editor must apply the SAME rule to what
 * it is willing to SEND: a value staged before the capability flipped is not sent just because
 * the control is no longer drawn.
 */
export function isPinnedRetentionAvailable({
  policy,
  policyState,
  effectiveWindow,
  capabilityUnsupported,
}: PinnedRetentionAvailability): boolean {
  return (
    policyState === 'ready' &&
    typeof policy?.includePinned === 'boolean' &&
    !capabilityUnsupported &&
    typeof effectiveWindow === 'number'
  );
}

export interface PinnedRetentionOptionProps {
  policy: ExpirationPolicy | null;
  policyState: 'loading' | 'ready' | 'unavailable';
  effectiveWindow: ExpirationWindowSeconds | null | undefined;
  checked: boolean;
  /** Not editable right now (no permission, a refusal, a refresh owed). Announced with
   *  `aria-disabled` and enforced by a guarded `onChange`, never native `disabled`: a native
   *  `disabled` checkbox leaves the tab order, and the person who cannot change the timer is
   *  exactly the one who needs to land here and hear why. */
  locked: boolean;
  /** The lock explanation's id, so a locked switch is described by the sentence that says why. */
  lockDescriptionId?: string;
  onChange: (next: boolean) => void;
  /** Lands on the wrapper so the editor can return focus here after an include-only apply: the
   *  Apply button it came from is gone by then, and focus must not drop to `<body>`. */
  ref?: Ref<HTMLDivElement>;
}

/**
 * The staged "Include pinned messages" switch (#3459). Presentation only: the editor owns the
 * staged value, the send-only-when-changed rule and the apply routing. Renders nothing unless
 * `isPinnedRetentionAvailable`, so a caller cannot draw it for a policy that cannot back it.
 */
export default function PinnedRetentionOption({
  policy,
  policyState,
  effectiveWindow,
  checked,
  locked,
  lockDescriptionId,
  onChange,
  ref,
}: Readonly<PinnedRetentionOptionProps>) {
  const hintId = useId();
  const capabilityUnsupported = useExpirationPinnedPolicyUnsupported();
  if (
    !isPinnedRetentionAvailable({ policy, policyState, effectiveWindow, capabilityUnsupported })
  ) {
    return null;
  }
  return (
    <div ref={ref} className={`message-expiration-pinned-option${locked ? ' is-locked' : ''}`}>
      <label className="message-expiration-pinned-label">
        <input
          type="checkbox"
          checked={checked}
          aria-disabled={locked}
          aria-describedby={locked && lockDescriptionId ? `${hintId} ${lockDescriptionId}` : hintId}
          onChange={(event) => {
            if (locked) return;
            onChange(event.target.checked);
          }}
        />
        <span>Include pinned messages</span>
      </label>
      <p id={hintId} className="message-expiration-pinned-hint">
        When off, pinned messages are kept for as long as they stay pinned.
      </p>
    </div>
  );
}
