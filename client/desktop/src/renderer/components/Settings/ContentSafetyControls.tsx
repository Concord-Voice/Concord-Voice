import { useCallback, useEffect, useRef, useState } from 'react';
import ToggleSwitch from './ToggleSwitch';
import { usePrivacyStore } from '../../stores/privacyStore';
import { klipyClient } from '../../services/gifProvider/klipyClient';

function gifAutoLoadHint(enabled: boolean): string {
  return enabled
    ? 'GIFs in messages render as soon as they enter view. All KLIPY traffic — searches, picks, and media — is routed through Concord servers, so KLIPY never sees your IP address.'
    : 'GIFs in messages show a "Click to load" placeholder until you tap them. All KLIPY traffic is always routed through Concord servers regardless of this setting.';
}

function gifPersonalizationHint(enabled: boolean): string {
  return enabled
    ? 'Concord sends a stable per-device ID to KLIPY so the GIF picker can show you personalized recent and trending results. The ID is not tied to your Concord account or any personally identifiable information.'
    : 'GIF picker results are not personalized. A rotating ephemeral ID is used so KLIPY cannot build a persistent profile. The Recent tab is hidden because it requires a stable ID to function.';
}

function personalizationIdHint(enabled: boolean): string {
  return enabled
    ? 'Your stable per-device identifier sent to KLIPY. Rotating it clears your personalization history.'
    : 'Ephemeral ID — rotates automatically every 30 minutes. Rotating manually generates a new one immediately.';
}

const ContentSafetyControls = () => {
  const privacySettings = usePrivacyStore((s) => s.settings);
  const updatePrivacy = usePrivacyStore((s) => s.updatePrivacy);
  const [displayedCustomerId, setDisplayedCustomerId] = useState<string | null>(null);
  const [isRotatingId, setIsRotatingId] = useState(false);
  const rotationCooldownRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: refreshes displayedCustomerId when GIF provider personalization setting changes; not a render loop
    setDisplayedCustomerId(klipyClient.getCurrentCustomerId());
  }, [privacySettings.sharePersonalizationWithGifProvider]);

  const handleRotateCustomerId = useCallback(async () => {
    setIsRotatingId(true);
    try {
      const newId = await klipyClient.rotateCustomerId();
      setDisplayedCustomerId(newId);
    } finally {
      rotationCooldownRef.current = setTimeout(() => setIsRotatingId(false), 3_000);
    }
  }, []);

  useEffect(() => {
    return () => {
      if (rotationCooldownRef.current !== null) {
        clearTimeout(rotationCooldownRef.current);
      }
    };
  }, []);

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
          onChange={(v) => updatePrivacy({ sharePersonalizationWithGifProvider: v })}
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
        </div>
        <button
          type="button"
          className="settings-rotate-id-btn"
          onClick={handleRotateCustomerId}
          disabled={isRotatingId}
          title={isRotatingId ? 'Rotate cooldown active' : 'Generate a new personalization ID'}
        >
          {isRotatingId ? 'Rotated' : 'Rotate'}
        </button>
      </div>
    </>
  );
};

export default ContentSafetyControls;
