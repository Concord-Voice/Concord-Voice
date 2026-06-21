import ToggleSwitch from './ToggleSwitch';
import { usePrivacyStore } from '../../stores/privacyStore';

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
            Allow others to find you by searching your username
          </span>
        </div>
        <ToggleSwitch
          checked={privacySettings.searchableByUsername}
          onChange={(v) => updatePrivacy({ searchableByUsername: v })}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Searchable by Email</span>
          <span className="settings-row-hint">Allow others to find you by email address</span>
        </div>
        <ToggleSwitch
          checked={privacySettings.searchableByEmail}
          onChange={(v) => updatePrivacy({ searchableByEmail: v })}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Searchable by Phone Number</span>
          <span className="settings-row-hint">Allow others to find you by phone number</span>
        </div>
        <ToggleSwitch
          checked={privacySettings.searchableByPhone}
          onChange={(v) => updatePrivacy({ searchableByPhone: v })}
        />
      </div>
    </>
  );
};

export default SearchVisibilityControls;
