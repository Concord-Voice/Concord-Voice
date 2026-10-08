import React from 'react';
import { Info, X } from 'lucide-react';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import { useSettingsOverlayStore } from '../../stores/ui/settingsOverlayStore';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';
import './MfaRestrictedNotice.css';

interface MfaRestrictedNoticeProps {
  serverId: string;
  /** The rail has no room for a sentence; the context-menu item covers it there. */
  compact: boolean;
  /**
   * The server-name heading in the sidebar header. Dismissing removes the
   * button that had focus, so focus moves here instead of falling to `<body>`.
   */
  returnFocusRef: React.RefObject<HTMLElement | null>;
}

/**
 * The banner under the channel sidebar's server header for a member whose
 * dangerous permissions the server is withholding until they enrol in MFA
 * (#3456 §3.6). It lives outside Server Settings on purpose: a masked owner or
 * Administrator cannot open Settings (F5), so Settings is the one place it
 * could not be reached from.
 *
 * Dismissible per server for the app session; the context-menu item
 * (`MfaRestrictedMenuItem`) is not, so the way in survives a dismissal. Like
 * the item, it is known only for the active server (R15).
 */
const MfaRestrictedNotice: React.FC<MfaRestrictedNoticeProps> = ({
  serverId,
  compact,
  returnFocusRef,
}) => {
  const restricted = usePermissionStore((s) => s.mfaRestrictedByServer[serverId] === true);
  const dismissed = useSettingsOverlayStore((s) => s.dismissedMfaNotices[serverId] === true);
  const dismissMfaNotice = useSettingsOverlayStore((s) => s.dismissMfaNotice);
  if (compact || !restricted || dismissed) return null;

  return (
    <div className="mfa-restricted-notice">
      <Info size={16} className="mfa-restricted-notice__glyph" aria-hidden="true" />
      <div className="mfa-restricted-notice__body">
        <p className="mfa-restricted-notice__text">
          Some of your permissions on this server need MFA.
        </p>
        <button
          type="button"
          className="mfa-restricted-notice__link"
          onClick={() => {
            void openVerificationSetup({ returnTo: { kind: 'chat' } });
          }}
        >
          Set up verification
        </button>
      </div>
      <button
        type="button"
        className="mfa-restricted-notice__dismiss"
        aria-label="Dismiss MFA notice"
        onClick={() => {
          returnFocusRef.current?.focus();
          dismissMfaNotice(serverId);
        }}
      >
        <X size={14} aria-hidden="true" />
      </button>
    </div>
  );
};

export default MfaRestrictedNotice;
