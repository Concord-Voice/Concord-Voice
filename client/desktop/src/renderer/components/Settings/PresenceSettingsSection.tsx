import React, { useCallback, useEffect, useState } from 'react';
import { useAuthStore } from '../../stores/auth/authStore';
import {
  type PresenceSettings,
  type PresenceTier,
  useRichPresenceStore,
} from '../../stores/ui/richPresenceStore';
import CategoryManagerPanel from '../DirectMessages/CategoryManagerPanel';
import CollapsibleSection from './CollapsibleSection';
import PresenceExceptions from './PresenceExceptions';
import ToggleSwitch from './ToggleSwitch';
import './PresenceSettingsSection.css';

const TIERS: { value: PresenceTier; label: string; hint: string }[] = [
  { value: 0, label: 'Off', hint: 'Nobody can see your custom status.' },
  {
    value: 1,
    label: 'Friends',
    hint: 'Only your friends and eligible friends-of-friends can see your custom status.',
  },
  {
    value: 2,
    label: 'Servers',
    hint: 'Your friends, eligible friends-of-friends, and members of servers you share can see your custom status.',
  },
];

type ActivityCategory = 'serverVoice' | 'privateCall';
type ActivityCopy = {
  title: string;
  detects: string;
  legend: string;
  tiers: readonly { value: PresenceTier; label: string }[];
  detailsHint: string;
};

const ACTIVITY_COPY: Record<ActivityCategory, ActivityCopy> = {
  serverVoice: {
    title: 'Server Voice',
    detects: 'When you join a voice channel in a server.',
    legend: 'Server Voice audience',
    tiers: [
      { value: 0, label: 'Off' },
      { value: 1, label: 'Friends in server' },
      { value: 2, label: 'All in server' },
    ],
    detailsHint: 'Include the channel and server name.',
  },
  privateCall: {
    title: 'Private Call',
    detects: 'When you are in a direct-message or group private call.',
    legend: 'Private Call audience',
    tiers: [
      { value: 0, label: 'Off' },
      { value: 1, label: 'Friends' },
      { value: 2, label: 'Servers' },
    ],
    detailsHint: 'Include the participant count for group calls.',
  },
};

const PRIVATE_CALL_WARNING =
  'Choosing Servers lets people who share a server with you learn that you are in a private call. It never shares participant names.';

function activityAudience(category: ActivityCategory, tier: PresenceTier): string {
  if (category === 'serverVoice') {
    if (tier === 1) {
      return 'Friends—and eligible friends-of-friends—who are in this server and can view this voice channel.';
    }
    if (tier === 2) return 'People in this server who can view this voice channel.';
    return 'Nobody';
  }
  if (tier === 1) {
    return 'People currently in this call, plus your friends and eligible friends-of-friends.';
  }
  if (tier === 2) {
    return 'People currently in this call, plus your friends, eligible friends-of-friends, and people who share a server with you.';
  }
  return 'People currently in this private call.';
}

function activityExample(category: ActivityCategory, tier: PresenceTier, details: boolean): string {
  if (category === 'serverVoice') {
    if (tier === 0) return 'Nothing is broadcast.';
    return details ? 'In voice in #General on Concord' : 'In voice';
  }
  return details ? 'In a group private call (participant count shown)' : 'In a private call';
}

interface ActivityCardProps {
  category: ActivityCategory;
  settings: PresenceSettings;
  disabled: boolean;
  onChangeTier: (category: ActivityCategory, tier: PresenceTier) => void;
  onChangeDetails: (category: ActivityCategory, enabled: boolean) => void;
}

const ActivityCard: React.FC<ActivityCardProps> = ({
  category,
  settings,
  disabled,
  onChangeTier,
  onChangeDetails,
}) => {
  const copy = ACTIVITY_COPY[category];
  const tier = category === 'serverVoice' ? settings.serverVoiceTier : settings.privateCallTier;
  const details =
    category === 'serverVoice' ? settings.serverVoiceShowDetails : settings.privateCallShowDetails;
  const audience = settings.masterEnabled ? activityAudience(category, tier) : 'Nobody';
  const example = settings.masterEnabled
    ? activityExample(category, tier, details)
    : 'Nothing is broadcast.';
  const detailsLabel = `${copy.title} show details`;
  const headingId = `presence-${category}-heading`;

  return (
    <section
      className={`presence-activity-card presence-activity-card-${category}`}
      aria-labelledby={headingId}
    >
      <div className="presence-activity-card-header">
        <h3 id={headingId}>{copy.title}</h3>
        <p>{copy.detects}</p>
      </div>
      <fieldset className="presence-tier-segmented presence-activity-tier-segmented">
        <legend className="presence-tier-legend">{copy.legend}</legend>
        {copy.tiers.map((option) => (
          <button
            type="button"
            key={option.value}
            className={`presence-tier-option ${tier === option.value ? 'active' : ''}`}
            aria-pressed={tier === option.value}
            aria-describedby={
              category === 'privateCall' && option.value === 2
                ? 'private-call-servers-warning'
                : undefined
            }
            disabled={disabled}
            onClick={() => onChangeTier(category, option.value)}
          >
            {option.label}
          </button>
        ))}
      </fieldset>
      <div className="settings-row presence-activity-details-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Show details</span>
          <span className="settings-row-hint">{copy.detailsHint}</span>
        </div>
        <ToggleSwitch
          label={detailsLabel}
          inputRole="switch"
          checked={details}
          disabled={disabled}
          onChange={(enabled) => onChangeDetails(category, enabled)}
        />
      </div>
      <div className="presence-activity-preview" aria-live="polite">
        <div>
          <span className="presence-preview-label">Audience</span>
          <span>{audience}</span>
        </div>
        <div>
          <span className="presence-preview-label">Example output</span>
          <span>{example}</span>
        </div>
      </div>
      {category === 'privateCall' && (
        <p className="presence-activity-warning" id="private-call-servers-warning">
          <span aria-hidden="true">!</span>
          <span>{PRIVATE_CALL_WARNING}</span>
        </p>
      )}
    </section>
  );
};

const PresenceSettingsSection: React.FC = () => {
  const authenticated = useAuthStore((s) => s.accessToken !== null);
  const authGeneration = useAuthStore((s) => s.authGeneration);
  const settings = useRichPresenceStore((s) => s.presenceSettings);
  const confirmedSettings = useRichPresenceStore((s) => s.confirmedPresenceSettings);
  const loading = useRichPresenceStore((s) => s.presenceSettingsLoading);
  const saving = useRichPresenceStore((s) => s.presenceSettingsSaving);
  const customStatusSaving = useRichPresenceStore((s) => s.customStatusSaving);
  const error = useRichPresenceStore((s) => s.presenceSettingsError);
  const hydrate = useRichPresenceStore((s) => s.hydratePresenceSettings);
  const update = useRichPresenceStore((s) => s.updatePresenceSettings);
  const [categoryManagerOpen, setCategoryManagerOpen] = useState(false);

  useEffect(() => {
    if (authenticated) void hydrate();
  }, [authenticated, authGeneration, hydrate]);

  const controlsDisabled = confirmedSettings === null || loading || saving;
  const tier = settings.customTextTier;
  const activeHint = TIERS.find((option) => option.value === tier)?.hint ?? '';

  const handleChangeActivityTier = useCallback(
    (category: ActivityCategory, nextTier: PresenceTier) => {
      if (category === 'serverVoice') void update({ serverVoiceTier: nextTier });
      else void update({ privateCallTier: nextTier });
    },
    [update]
  );

  const handleChangeActivityDetails = useCallback(
    (category: ActivityCategory, enabled: boolean) => {
      if (category === 'serverVoice') void update({ serverVoiceShowDetails: enabled });
      else void update({ privateCallShowDetails: enabled });
    },
    [update]
  );

  const handleRetry = useCallback(() => {
    if (
      loading ||
      saving ||
      useRichPresenceStore.getState().presenceSettingsLoading ||
      useRichPresenceStore.getState().presenceSettingsSaving ||
      useRichPresenceStore.getState().customStatusSaving
    ) {
      return;
    }
    void hydrate();
  }, [hydrate, loading, saving]);

  return (
    <CollapsibleSection id="section-presence-settings" title="Rich Presence">
      <p className="settings-section-description">Control who can see activity from Concord.</p>

      <div className="settings-row presence-master-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Share Rich Presence</span>
          <span className="settings-row-hint">
            Turn this off to stop all Rich Presence broadcasts without changing your saved choices.
          </span>
        </div>
        <ToggleSwitch
          label="Share Rich Presence"
          inputRole="switch"
          checked={settings.masterEnabled}
          disabled={controlsDisabled}
          onChange={(enabled) => void update({ masterEnabled: enabled })}
        />
      </div>

      <p className="presence-activity-info">
        Show details changes what allowed people can see. It never expands who can see it.
      </p>

      <div className="presence-activity" aria-busy={loading || saving}>
        <ActivityCard
          category="serverVoice"
          settings={settings}
          disabled={controlsDisabled}
          onChangeTier={handleChangeActivityTier}
          onChangeDetails={handleChangeActivityDetails}
        />
        <ActivityCard
          category="privateCall"
          settings={settings}
          disabled={controlsDisabled}
          onChangeTier={handleChangeActivityTier}
          onChangeDetails={handleChangeActivityDetails}
        />
      </div>

      {loading && <output className="settings-status">Loading Rich Presence settings…</output>}
      {saving && <output className="settings-status">Saving Rich Presence settings…</output>}
      {error && (
        <div className="settings-error" role="alert">
          {error}
        </div>
      )}
      {error && confirmedSettings === null && (
        <button
          type="button"
          className="presence-retry-button"
          onClick={handleRetry}
          disabled={loading || saving || customStatusSaving}
        >
          Try again
        </button>
      )}

      <h3 className="settings-subsection-title">Custom Status</h3>
      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Who Can See Your Custom Status</span>
          <span className="settings-row-hint">{activeHint}</span>
        </div>
        <fieldset className="presence-tier-segmented">
          <legend className="presence-tier-legend">Custom status visibility</legend>
          {TIERS.map((option) => (
            <button
              type="button"
              key={option.value}
              className={`presence-tier-option ${tier === option.value ? 'active' : ''}`}
              aria-pressed={tier === option.value}
              disabled={controlsDisabled}
              onClick={() => void update({ customTextTier: option.value })}
            >
              {option.label}
            </button>
          ))}
        </fieldset>
      </div>

      <PresenceExceptions
        categoryManagerOpen={categoryManagerOpen}
        onOpenCategoryManager={() => setCategoryManagerOpen(true)}
      />

      {categoryManagerOpen && (
        <CategoryManagerPanel onClose={() => setCategoryManagerOpen(false)} />
      )}
    </CollapsibleSection>
  );
};

export default PresenceSettingsSection;
