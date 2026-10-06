import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react';
import ToggleSwitch from './ToggleSwitch';
import { usePrivacyStore } from '../../stores/ui/privacyStore';
import { klipyClient } from '../../services/messaging/gifProvider/klipyClient';
import {
  captureRuntimeServerSelection,
  onRuntimeServerSelectionChange,
  runtimeServerSelectionIsCurrent,
} from '../../services/system/runtimeServerBase';

const subscribeCustomerId = (listener: () => void): (() => void) =>
  klipyClient.subscribeCustomerId(listener);
const getCustomerIdSnapshot = (): string | null => klipyClient.getCurrentCustomerId();

function gifAutoLoadHint(enabled: boolean): string {
  return enabled
    ? 'GIFs in messages render as soon as they enter view. All KLIPY traffic — searches, picks, and media — is routed through Concord servers, so KLIPY never sees your IP address.'
    : 'GIFs in messages show a "Click to load" placeholder until you tap them. All KLIPY traffic is always routed through Concord servers regardless of this setting.';
}

function gifPersonalizationHint(enabled: boolean): string {
  return enabled
    ? 'Concord uses the Personalization ID below to make a separate ID for KLIPY. KLIPY receives that ID with GIF browsing and shares. That links your GIF shares to your Recent list. KLIPY receives neither your Concord account ID nor the ID shown here.'
    : 'GIF browsing uses a temporary ID that changes about every 30 minutes. Recent GIFs are unavailable. KLIPY can still connect requests made with the same temporary ID.';
}

function personalizationIdHint(enabled: boolean): string {
  return enabled
    ? 'This value helps make the ID KLIPY sees. Rotate it to start a new Recent list. Turning personalization off keeps this ID for when you turn it back on.'
    : 'This temporary value changes about every 30 minutes. Rotate it to change it now. Your earlier Recent GIFs are unavailable while personalization is off.';
}

const ROTATION_BUTTON_COPY = {
  idle: { title: 'Generate a new personalization ID', label: 'Rotate' },
  pending: { title: 'Rotating personalization ID', label: 'Rotating…' },
  done: { title: 'Rotate cooldown active', label: 'Rotated' },
} as const;

const ContentSafetyControls = () => {
  const privacySettings = usePrivacyStore((s) => s.settings);
  const privacyLoaded = usePrivacyStore((s) => s.loaded);
  const privacyError = usePrivacyStore((s) => s.error);
  const fetchPrivacy = usePrivacyStore((s) => s.fetchPrivacy);
  const updatePrivacy = usePrivacyStore((s) => s.updatePrivacy);
  const displayedCustomerId = useSyncExternalStore(subscribeCustomerId, getCustomerIdSnapshot);
  const [rotationState, setRotationState] = useState<'idle' | 'pending' | 'done'>('idle');
  const rotationButtonCopy = ROTATION_BUTTON_COPY[rotationState];
  const [rotationError, setRotationError] = useState<string | null>(null);
  const [idLoadFailed, setIdLoadFailed] = useState(false);
  const rotationCooldownRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const contextEpochRef = useRef(0);

  const invalidateIdUi = useCallback(() => {
    contextEpochRef.current += 1;
    if (rotationCooldownRef.current !== null) {
      clearTimeout(rotationCooldownRef.current);
      rotationCooldownRef.current = null;
    }
    /* eslint-disable @eslint-react/set-state-in-effect -- a mode or server change must clear the prior rotation result */
    setRotationState('idle');
    setRotationError(null);
    setIdLoadFailed(false);
    /* eslint-enable @eslint-react/set-state-in-effect -- reset complete */
  }, []);

  useEffect(() => {
    let active = true;
    invalidateIdUi();
    if (!privacyLoaded) {
      // The pre-fetch ON value is a placeholder, not the account's preference.
      klipyClient.setPersonalizationEnabled(false);
      return;
    }
    klipyClient.setPersonalizationEnabled(privacySettings.sharePersonalizationWithGifProvider);
    const loadId = (): void => {
      const selection = captureRuntimeServerSelection();
      const contextEpoch = contextEpochRef.current;
      void klipyClient
        .getCustomerID()
        .then((id) => {
          if (
            active &&
            contextEpoch === contextEpochRef.current &&
            runtimeServerSelectionIsCurrent(selection) &&
            !id
          ) {
            setIdLoadFailed(true);
          }
        })
        .catch(() => {
          if (
            active &&
            contextEpoch === contextEpochRef.current &&
            runtimeServerSelectionIsCurrent(selection)
          ) {
            setIdLoadFailed(true);
          }
        });
    };
    loadId();
    const unsubscribeServer = onRuntimeServerSelectionChange(() => {
      invalidateIdUi();
      loadId();
    });
    return () => {
      active = false;
      contextEpochRef.current += 1;
      unsubscribeServer();
    };
  }, [invalidateIdUi, privacyLoaded, privacySettings.sharePersonalizationWithGifProvider]);

  const handleRotateCustomerId = useCallback(async () => {
    const contextEpoch = ++contextEpochRef.current;
    setRotationError(null);
    setRotationState('pending');
    try {
      await klipyClient.rotateCustomerId();
      if (contextEpoch !== contextEpochRef.current) return;
      setIdLoadFailed(false);
      setRotationState('done');
      rotationCooldownRef.current = setTimeout(() => setRotationState('idle'), 3_000);
    } catch {
      if (contextEpoch !== contextEpochRef.current) return;
      setRotationError('Could not rotate the Personalization ID. Please try again.');
      setRotationState('idle');
    }
  }, []);

  useEffect(() => {
    return () => {
      if (rotationCooldownRef.current !== null) {
        clearTimeout(rotationCooldownRef.current);
      }
    };
  }, []);

  if (!privacyLoaded) {
    return (
      <>
        <h3 className="settings-subsection-title" style={{ marginTop: 20 }}>
          Content Safety
        </h3>
        {privacyError ? (
          <div role="alert" className="settings-row-hint">
            Could not load privacy settings.{' '}
            <button type="button" onClick={() => void fetchPrivacy()}>
              Retry
            </button>
          </div>
        ) : (
          <output className="settings-row-hint">Loading privacy settings…</output>
        )}
      </>
    );
  }

  return (
    <>
      <h3 className="settings-subsection-title" style={{ marginTop: 20 }}>
        Content Safety
      </h3>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Allow Embedded Content</span>
          <span className="settings-row-hint">
            Render link previews, image thumbnails, and other embedded content in messages.{' '}
            <strong>GIFs from KLIPY are controlled separately below.</strong> When disabled, only
            the raw message text is shown — no external requests are made for previews, protecting
            your IP address from off-app tracking beacons. Server moderators with the Manage All
            Messages permission can also suppress embeds on individual messages regardless of this
            setting.
          </span>
        </div>
        <ToggleSwitch
          checked={privacySettings.allowEmbeddedContent}
          onChange={(v) => updatePrivacy({ allowEmbeddedContent: v })}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Load GIFs from KLIPY automatically</span>
          <span className="settings-row-hint">
            {gifAutoLoadHint(privacySettings.loadGifsAutomatically)}
          </span>
        </div>
        <ToggleSwitch
          checked={privacySettings.loadGifsAutomatically}
          onChange={(v) => updatePrivacy({ loadGifsAutomatically: v })}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Share GIF personalization with provider</span>
          <span className="settings-row-hint">
            {gifPersonalizationHint(privacySettings.sharePersonalizationWithGifProvider)}
          </span>
        </div>
        <ToggleSwitch
          checked={privacySettings.sharePersonalizationWithGifProvider}
          onChange={(v) => {
            invalidateIdUi();
            updatePrivacy({ sharePersonalizationWithGifProvider: v });
          }}
        />
      </div>

      <div className="settings-row settings-row-child">
        <div className="settings-row-info">
          <span className="settings-row-label">Personalization ID</span>
          <span className="settings-row-hint">
            {personalizationIdHint(privacySettings.sharePersonalizationWithGifProvider)}
          </span>
          {displayedCustomerId && (
            <span className="settings-estimated-bitrate settings-klipy-id-chip">
              {displayedCustomerId}
            </span>
          )}
          {!displayedCustomerId && idLoadFailed && (
            <span role="alert">Could not load the Personalization ID. Please try Rotate.</span>
          )}
          {rotationError && <span role="alert">{rotationError}</span>}
        </div>
        <button
          type="button"
          className="settings-rotate-id-btn"
          onClick={handleRotateCustomerId}
          disabled={rotationState !== 'idle'}
          title={rotationButtonCopy.title}
        >
          {rotationButtonCopy.label}
        </button>
      </div>
    </>
  );
};

export default ContentSafetyControls;
