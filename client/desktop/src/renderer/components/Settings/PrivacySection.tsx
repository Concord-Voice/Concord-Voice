import React, { useState, useCallback, useEffect } from 'react';
import { useUserStore } from '../../stores/auth/userStore';
import { usePrivacyStore } from '../../stores/ui/privacyStore';
import { useClientConfigStore } from '../../stores/ui/clientConfigStore';
import CollapsibleSection from './CollapsibleSection';
import DMPrivacyControls from './DMPrivacyControls';
import FriendRequestPrivacyControls from './FriendRequestPrivacyControls';
import ContentSafetyControls from './ContentSafetyControls';
import SearchVisibilityControls from './SearchVisibilityControls';
import PresenceSettingsSection from './PresenceSettingsSection';
import ActivityHistoryCard from './ActivityHistoryCard';
import PresenceHistorySection from '../Profile/PresenceHistorySection';
import ToggleSwitch from './ToggleSwitch';
import PurgeFenceStepUpDialog from './PurgeFenceStepUpDialog';
import {
  setDraftContentProtection,
  useDraftContentProtection,
  useDraftContentProtectionApplying,
  useDraftContentProtectionLoaded,
} from '../../hooks/ui/useDraftSettings';

// #1354: the purge dead-end card navigates here by control id
// (`settingsNavStore.requestFocus('privacy', 'requireAuthBeforePurge')`),
// so the switch id and the label it points at are a cross-component contract.
const PURGE_AUTH_LABEL_ID = 'requireAuthBeforePurge-label';
const CONTENT_PROTECTION_LABEL_ID = 'contentProtection-label';
const CONTENT_PROTECTION_HINT_ID = 'contentProtection-hint';

interface PrivacySectionProps {
  onActivityHistoryAvailabilityChange?: (available: boolean) => void;
}

const PrivacySection: React.FC<PrivacySectionProps> = ({ onActivityHistoryAvailabilityChange }) => {
  const userId = useUserStore((s) => s.user?.id ?? null);
  const activityHistoryCapability = useClientConfigStore(
    (state) => state.activityHistoryCapability
  );
  const privacySettings = usePrivacyStore((s) => s.settings);
  const fetchPrivacy = usePrivacyStore((s) => s.fetchPrivacy);
  const updatePrivacy = usePrivacyStore((s) => s.updatePrivacy);
  const privacyLoaded = usePrivacyStore((s) => s.loaded);
  const localDmLevel = usePrivacyStore((s) => s.pendingDmPrivacyLevel ?? s.settings.dmPrivacyLevel);
  const localFriendRequestMode = usePrivacyStore(
    (s) => s.pendingFriendRequestMode ?? s.settings.allowFriendRequestsFrom
  );
  const setDmPrivacyLevel = usePrivacyStore((s) => s.scheduleDMPrivacy);
  const setFriendRequestMode = usePrivacyStore((s) => s.scheduleFriendRequestPrivacy);
  const dmSaveError = usePrivacyStore((s) => s.dmSaveError);
  const friendRequestError = usePrivacyStore((s) => s.friendRequestError);
  const contentProtection = useDraftContentProtection();
  const contentProtectionLoaded = useDraftContentProtectionLoaded();
  const contentProtectionApplying = useDraftContentProtectionApplying();
  const [contentProtectionPlatform, setContentProtectionPlatform] = useState<string | null>(null);
  const [activityHistoryControlsVisible, setActivityHistoryControlsVisible] = useState(false);
  const [presenceHistoryVisible, setPresenceHistoryVisible] = useState(false);
  const [activityHistoryControlsAvailable, setActivityHistoryControlsAvailable] = useState(false);
  const [presenceHistoryAvailable, setPresenceHistoryAvailable] = useState(false);
  const activityHistoryAvailable = activityHistoryControlsAvailable || presenceHistoryAvailable;
  const activityHistoryVisible =
    activityHistoryCapability.status !== 'confirmed-unsupported' ||
    activityHistoryControlsVisible ||
    presenceHistoryVisible;
  useEffect(() => {
    onActivityHistoryAvailabilityChange?.(activityHistoryAvailable);
  }, [activityHistoryAvailable, onActivityHistoryAvailabilityChange]);
  useEffect(() => {
    let mounted = true;
    void globalThis.electron
      ?.getPlatform?.()
      .then((platform) => {
        if (mounted) setContentProtectionPlatform(platform);
      })
      .catch(() => {
        if (mounted) setContentProtectionPlatform(null);
      });
    return () => {
      mounted = false;
    };
  }, []);

  // #1354: a toggle-only PATCH against a control-plane that predates the field
  // comes back 400. The store maps that one shape to the skew copy; surfacing it
  // here is what stops the flip from failing silently.
  const [purgeAuthError, setPurgeAuthError] = useState<string | null>(null);
  const [purgeFenceStepUpOpen, setPurgeFenceStepUpOpen] = useState(false);

  const setRequireAuthBeforePurge = useCallback(
    async (next: boolean) => {
      setPurgeAuthError(null);
      // #2765: turning the fence OFF is step-up gated, so the dialog owns that
      // transition end-to-end — it collects the factors and sends the PATCH.
      // Nothing goes out from here, and the switch stays bound to the store, so
      // it does not flip until the server accepts. Turning it ON is ungated.
      if (!next) {
        setPurgeFenceStepUpOpen(true);
        return;
      }
      try {
        await updatePrivacy({ requireAuthBeforePurge: true });
      } catch (err) {
        setPurgeAuthError(err instanceof Error ? err.message : 'Failed to update privacy settings');
      }
    },
    [updatePrivacy]
  );

  useEffect(() => {
    void fetchPrivacy();
  }, [fetchPrivacy]);

  const renderContentProtectionControl = () =>
    contentProtectionLoaded &&
    (contentProtectionPlatform === 'darwin' || contentProtectionPlatform === 'win32') && (
      <div className="settings-row">
        <div className="settings-row-info">
          <label
            className="settings-row-label"
            id={CONTENT_PROTECTION_LABEL_ID}
            htmlFor="contentProtection"
          >
            Protect Concord windows from screen capture
          </label>
          <span className="settings-row-hint" id={CONTENT_PROTECTION_HINT_ID}>
            When applied, Concord asks macOS or Windows to prevent its main and picture-in-picture
            call windows from being captured. Your operating system may not block every capture
            method.
          </span>
        </div>
        <ToggleSwitch
          id="contentProtection"
          checked={contentProtection}
          onChange={setDraftContentProtection}
          disabled={contentProtectionApplying}
          ariaLabelledBy={CONTENT_PROTECTION_LABEL_ID}
          aria-describedby={CONTENT_PROTECTION_HINT_ID}
          inputRole="switch"
        />
      </div>
    );

  return (
    <>
      <CollapsibleSection id="section-privacy-settings" title="Privacy Controls">
        <p className="settings-section-description">
          Control who can message you and how others can find you.
        </p>

        {renderContentProtectionControl()}

        <DMPrivacyControls
          localDmLevel={localDmLevel}
          setDmPrivacyLevel={setDmPrivacyLevel}
          saveError={dmSaveError}
          isLoaded={privacyLoaded}
        />

        <FriendRequestPrivacyControls
          localMode={localFriendRequestMode}
          setMode={setFriendRequestMode}
          saveError={friendRequestError}
          isLoaded={privacyLoaded}
        />

        <div className="settings-row">
          <div className="settings-row-info">
            <label
              className="settings-row-label"
              id={PURGE_AUTH_LABEL_ID}
              htmlFor="requireAuthBeforePurge"
            >
              Require authentication before purging
            </label>
            <span className="settings-row-hint">
              Ask for your password before purging messages in a direct message or group chat.
            </span>
            {!privacySettings.requireAuthBeforePurge && (
              <span className="settings-row-hint" role="alert">
                Without this, anyone with access to your unlocked account can permanently purge your
                message history.
              </span>
            )}
            {purgeAuthError && (
              <span className="settings-row-hint" role="alert">
                {purgeAuthError}
              </span>
            )}
          </div>
          <ToggleSwitch
            id="requireAuthBeforePurge"
            checked={privacySettings.requireAuthBeforePurge}
            onChange={(v) => void setRequireAuthBeforePurge(v)}
            ariaLabelledBy={PURGE_AUTH_LABEL_ID}
            inputRole="switch"
          />
        </div>

        <PurgeFenceStepUpDialog
          open={purgeFenceStepUpOpen}
          onClose={() => setPurgeFenceStepUpOpen(false)}
        />

        <ContentSafetyControls />
        <SearchVisibilityControls />
      </CollapsibleSection>

      <PresenceSettingsSection />

      <div hidden={!activityHistoryVisible}>
        <CollapsibleSection id="section-presence-history" title="Activity History">
          <ActivityHistoryCard
            onVisibilityChange={setActivityHistoryControlsVisible}
            onAvailabilityChange={setActivityHistoryControlsAvailable}
          />
          <PresenceHistorySection
            userId={userId}
            onVisibilityChange={setPresenceHistoryVisible}
            onAvailabilityChange={setPresenceHistoryAvailable}
          />
        </CollapsibleSection>
      </div>
    </>
  );
};

export default PrivacySection;
