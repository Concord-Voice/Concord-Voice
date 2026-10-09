import ToggleSwitch from './ToggleSwitch';
import { usePrivacyStore } from '../../stores/ui/privacyStore';

const SearchVisibilityControls = () => {
  const privacySettings = usePrivacyStore((s) => s.settings);
  const updatePrivacy = usePrivacyStore((s) => s.updatePrivacy);

  return (
    <>
      <h3 className="settings-subsection-title" style={{ marginTop: 20 }}>
        Search Visibility
      </h3>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Searchable by Username</span>
          <span className="settings-row-hint">
            Show your profile in Add Friend search. When this is off, people in your servers can
            still see you in member lists, and your existing friends can see you in their Friends
            lists.
          </span>
        </div>
        <ToggleSwitch
          checked={privacySettings.searchableByUsername}
          onChange={(v) => updatePrivacy({ searchableByUsername: v })}
        />
      </div>

      <div className="settings-row settings-row-disabled">
        <div className="settings-row-info">
          <span className="settings-row-label">Searchable by Email</span>
          <span className="settings-row-hint">Planned. Email search is not available yet.</span>
        </div>
        <ToggleSwitch
          checked={privacySettings.searchableByEmail}
          onChange={() => undefined}
          disabled
        />
      </div>

      <div className="settings-row settings-row-disabled">
        <div className="settings-row-info">
          <span className="settings-row-label">Searchable by Phone Number</span>
          <span className="settings-row-hint">
            Planned. Phone number search is not available yet.
          </span>
        </div>
        <ToggleSwitch
          checked={privacySettings.searchableByPhone}
          onChange={() => undefined}
          disabled
        />
      </div>
    </>
  );
};

export default SearchVisibilityControls;
