import React, { useId, useState } from 'react';
import DangerousActionStepUpDialog from '../Auth/DangerousActionStepUpDialog';
import ErrorBanner from '../Settings/ErrorBanner';
import { stepUpBanner } from '../Settings/mfaStepUp';
import ToggleSwitch from '../Settings/ToggleSwitch';
import {
  useMfaEnforcement,
  type Announcement,
  type MfaEnforcementModel,
  type TurnOnError,
} from '../../hooks/auth/useMfaEnforcement';
import { putMfaEnforcement } from '../../services/system/mfaEnforcementApi';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';
// ErrorBanner's rule lives there, and this is mounted from a lazily loaded page.
import '../Settings/MFA.css';
import './MfaEnforcementSetting.css';

export interface MfaEnforcementSettingProps {
  /** The server whose setting this is. Changing it remounts the component's state. */
  serverId: string;
  /**
   * Given when the page that hosts this has edits that leaving it would lose:
   * "Set up verification" opens App Settings, which unmounts Server Settings.
   * Resolves false to stay where it is. Omitted, nothing is asked.
   */
  confirmDiscard?: () => boolean | Promise<boolean>;
}

const ENROLMENT_SENTENCE =
  'Set up an authenticator app or security key on your account to turn this on.';
const UNSUPPORTED_SENTENCE = 'This server version cannot require MFA for dangerous actions yet.';
const NETWORK_TEXT = stepUpBanner({ kind: 'networkError' }) ?? '';

const ANNOUNCEMENTS: Record<Announcement, string> = {
  on: 'MFA enforcement is on.',
  off: 'MFA enforcement is off.',
  enrolment: ENROLMENT_SENTENCE,
};

/** The sentence for a refusal that is not about enrolment or a code. */
function describeFailure(status: number): string {
  switch (status) {
    case 403:
      return "You don't have permission to change this setting.";
    case 429:
      return 'Too many attempts. Try again in a few minutes.';
    default:
      return "Couldn't change this setting. Try again.";
  }
}

function turnOnErrorText(error: TurnOnError): string {
  return error.kind === 'transport' ? NETWORK_TEXT : describeFailure(error.status);
}

const LoadFailedRow: React.FC<{ onRetry: () => void }> = ({ onRetry }) => (
  <div className="settings-row">
    <div className="settings-row-info mfa-enforcement__load-failed">
      <span className="settings-row-hint">Couldn&apos;t load this setting.</span>
      <button type="button" className="step-up__link" onClick={onRetry}>
        Retry
      </button>
    </div>
  </div>
);

interface NoteProps {
  id: string;
  block: 'unsupported' | 'unenrolled';
  onSetUpVerification: () => void;
}

/** Why ON is unavailable. Enrolment can be fixed from here; an old server cannot. */
const BlockNote: React.FC<NoteProps> = ({ id, block, onSetUpVerification }) => (
  <div className="mfa-enforcement__note">
    <p id={id} className="settings-row-hint">
      {block === 'unenrolled' ? ENROLMENT_SENTENCE : UNSUPPORTED_SENTENCE}
    </p>
    {block === 'unenrolled' && (
      <button type="button" className="step-up__link" onClick={onSetUpVerification}>
        Set up verification
      </button>
    )}
  </div>
);

interface RowProps {
  model: MfaEnforcementModel;
  enforcing: boolean;
  onToggle: (next: boolean) => void;
  onSetUpVerification: () => void;
  switchId: string;
}

const EnforcementRow: React.FC<RowProps> = ({
  model,
  enforcing,
  onToggle,
  onSetUpVerification,
  switchId,
}) => {
  const labelId = useId();
  const hintId = useId();
  const noteId = useId();
  const { onBlock, turningOn, error } = model;
  // Only ON is refused up front. aria-disabled, never `disabled`, so the
  // switch stays in the tab order and says why through its description.
  const inert = turningOn || onBlock !== null;

  return (
    <>
      <div className="settings-row" aria-busy={turningOn || undefined}>
        <div className="settings-row-info">
          <label className="settings-row-label" id={labelId} htmlFor={switchId}>
            Require MFA for dangerous actions
          </label>
          <span className="settings-row-hint" id={hintId}>
            Members without MFA lose permissions such as banning, kicking and managing roles,
            channels and settings. Members with MFA confirm with a code first.
          </span>
          {turningOn && <span className="settings-row-hint">Turning on…</span>}
        </div>
        <ToggleSwitch
          id={switchId}
          inputRole="switch"
          ariaLabelledBy={labelId}
          checked={enforcing}
          onChange={(next) => {
            if (!inert) onToggle(next);
          }}
          aria-disabled={inert || undefined}
          aria-describedby={onBlock === null ? hintId : `${hintId} ${noteId}`}
        />
      </div>
      {onBlock !== null && (
        <BlockNote id={noteId} block={onBlock} onSetUpVerification={onSetUpVerification} />
      )}
      {error !== null && <ErrorBanner error={turnOnErrorText(error)} />}
    </>
  );
};

const MfaEnforcementBody: React.FC<MfaEnforcementSettingProps> = ({ serverId, confirmDiscard }) => {
  const model = useMfaEnforcement(serverId);
  const [confirmingOff, setConfirmingOff] = useState(false);
  const switchId = useId();
  const { read, announcement, settleOff } = model;

  if (read.kind === 'pending' || read.kind === 'absent') return null;

  const openSetup = (closeHost?: () => void) =>
    void openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId, section: 'general' },
      confirmDiscard,
      closeHost,
    });

  return (
    <div className="settings-section" id="section-security">
      <h2 className="settings-section-title">Security</h2>
      {read.kind === 'failed' ? (
        <LoadFailedRow onRetry={model.retry} />
      ) : (
        <EnforcementRow
          model={model}
          enforcing={read.enforcing}
          switchId={switchId}
          onToggle={(next) => (next ? model.turnOn() : setConfirmingOff(true))}
          onSetUpVerification={() => openSetup()}
        />
      )}
      {/* Always mounted: a live region announces a change only if it was there first. */}
      <output className="mfa-enforcement__status">
        {announcement === null ? '' : ANNOUNCEMENTS[announcement]}
      </output>
      <DangerousActionStepUpDialog
        isOpen={confirmingOff}
        purpose="servers.mfa_enforcement_disable"
        intro="You're about to stop requiring MFA for dangerous actions on this server."
        primaryLabel="Turn Off"
        busyLabel="Turning off…"
        send={(mfaCode, context) =>
          putMfaEnforcement(serverId, { enabled: false, mfaCode }, context)
        }
        describeFailure={describeFailure}
        onSuccess={() => {
          settleOff();
          setConfirmingOff(false);
        }}
        onClose={() => setConfirmingOff(false)}
        onSetUpVerification={() => openSetup(() => setConfirmingOff(false))}
        focusFallback={() => document.getElementById(switchId)}
      />
    </div>
  );
};

/**
 * The "Require MFA for dangerous actions" switch for one server (#3456 §3.5),
 * as a labelled "Security" subsection (`id="section-security"`).
 *
 * Nothing renders while the first read is out, and nothing at all when the
 * member may not see the setting (the read answers 403 or 404): the owner and
 * a raw-bit Administrator get the switch, and everyone else's Server Settings
 * is as it was. Mount it at the end of General; Server Settings owns the page,
 * and this owns everything about the setting, so it needs no other wiring.
 *
 * Safe inside the page's `<form>`: every button is `type="button"` and the
 * confirmation dialog is portalled out of the DOM subtree.
 *
 * ON is sent with no code and is never optimistic. OFF opens the shared
 * confirmation before it sends anything. Turning ON is refused up front only
 * when the account is KNOWN to have no usable MFA or the server is KNOWN to be
 * too old; an unknown answer leaves it live, because the server decides.
 */
const MfaEnforcementSetting: React.FC<MfaEnforcementSettingProps> = ({
  serverId,
  confirmDiscard,
}) => <MfaEnforcementBody key={serverId} serverId={serverId} confirmDiscard={confirmDiscard} />;

export default MfaEnforcementSetting;
