import ToggleSwitch from './ToggleSwitch';
import { usePrivacyStore, DMPrivacyLevel } from '../../stores/privacyStore';

const DM_LEVEL_TITLES: Record<DMPrivacyLevel, string> = {
  0: 'No One',
  1: 'Friends Only',
  2: 'Friends + Server Members',
  3: 'Allow All',
};

const DM_LEVEL_DESCRIPTIONS: Record<DMPrivacyLevel, string> = {
  0: "Hermit mode. Nobody can DM you. You've chosen peace and silence. Existing DMs are frozen solid.",
  1: "Inner circle only. If they're not on your friends list, they're talking to a wall.",
  2: 'The social butterfly. Friends and anyone you share a server with can slide into your DMs.',
  3: 'Open season. Anyone on the platform can message you. Bold move.',
};

const DM_FOF_HINTS: Record<number, string> = {
  0: 'DMs are disabled — this setting has no effect.',
  3: 'Everyone can already DM you — friends-of-friends is included.',
};

function computeFofChecked(dmLevel: DMPrivacyLevel, storeSetting: boolean): boolean {
  if (dmLevel === 3) return true;
  if (dmLevel === 0) return false;
  return storeSetting;
}

function isFofDisabled(dmLevel: DMPrivacyLevel): boolean {
  return dmLevel === 0 || dmLevel === 3;
}

interface DMPrivacyControlsProps {
  localDmLevel: DMPrivacyLevel;
  setDmPrivacyLevel: (level: DMPrivacyLevel) => void;
}

const DMPrivacyControls = ({ localDmLevel, setDmPrivacyLevel }: DMPrivacyControlsProps) => {
  const privacySettings = usePrivacyStore((s) => s.settings);
  const updatePrivacy = usePrivacyStore((s) => s.updatePrivacy);

  return (
    <>
      <div className="settings-tier-slider-container">
        <span className="settings-row-label">Who Can DM You</span>
        <div className="settings-tier-labels">
          {(['No One', 'Friends', 'Friends + Server', 'Everyone'] as const).map((label, i) => (
            <button
              type="button"
              key={label}
              className={`settings-tier-label ${localDmLevel === i ? 'active' : ''}`}
              aria-pressed={localDmLevel === i}
              onClick={() => setDmPrivacyLevel(i as DMPrivacyLevel)}
            >
              {label}
            </button>
          ))}
        </div>
        <div className="settings-tier-track dm-privacy-track">
          <div className="settings-tier-ticks">
            {[0, 1, 2, 3].map((level) => (
              <span
                key={`dm-tick-${level}`}
                className={`settings-tier-tick ${localDmLevel === level ? 'active' : ''}`}
              />
            ))}
          </div>
          <input
            type="range"
            className="settings-tier-slider"
            min={0}
            max={3}
            step={1}
            value={localDmLevel}
            onChange={(e) => setDmPrivacyLevel(Number(e.target.value) as DMPrivacyLevel)}
          />
        </div>
        <div className="settings-tier-description">
          <span>{DM_LEVEL_TITLES[localDmLevel]}</span>
          <span>{DM_LEVEL_DESCRIPTIONS[localDmLevel]}</span>
        </div>
      </div>

      <div className={`settings-row ${isFofDisabled(localDmLevel) ? 'settings-row-disabled' : ''}`}>
        <div className="settings-row-info">
          <span className="settings-row-label">Allow Friends-of-Friends</span>
          <span className="settings-row-hint">
            {DM_FOF_HINTS[localDmLevel] ??
              'Also allow DMs from people who share a mutual friend with you'}
          </span>
          <span className="settings-row-hint">
            This also expands who can see your custom status at the Friends tier.
          </span>
        </div>
        <ToggleSwitch
          checked={computeFofChecked(localDmLevel, privacySettings.dmFriendsOfFriends)}
          onChange={(v) => {
            if (!isFofDisabled(localDmLevel)) {
              updatePrivacy({ dmFriendsOfFriends: v });
            }
          }}
          disabled={isFofDisabled(localDmLevel)}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Auto-Accept Friend Requests from Codes</span>
          <span className="settings-row-hint">
            Automatically accept when someone uses your friend code
          </span>
        </div>
        <ToggleSwitch
          checked={privacySettings.autoAcceptFriendCodes}
          onChange={(v) => updatePrivacy({ autoAcceptFriendCodes: v })}
        />
      </div>
    </>
  );
};

export default DMPrivacyControls;
