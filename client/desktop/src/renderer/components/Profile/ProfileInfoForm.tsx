import React, { useState, useEffect } from 'react';
import { useUserStore, UpdateProfileData } from '../../stores/userStore';
import { useImageUpload } from '../../hooks/useImageUpload';
import { useEntitlement } from '../../hooks/useEntitlement';
import { formatFileSize } from '../../utils/attachmentCrypto';
import LoadingSpinner from '../Auth/LoadingSpinner';
import Modal from '../ui/Modal';
import ImageCropEditor from '../ui/ImageCropEditor';
import {
  ALLOWED_TYPES,
  USERNAME_MIN,
  USERNAME_MAX,
  DISPLAY_NAME_MAX,
  BIO_MAX,
  MAX_LINKS,
  type ProfileFormErrors,
} from './profileConstants';

/** Premium uplift factor for the size-upsell copy (UX hint only). */
const PREMIUM_IMAGE_MULTIPLIER = 2;
/** Free username-change cadence (1 year) in seconds — when the entitlement
 *  matches this, the L8 note advertises the premium (3-month) cadence. */
const FREE_USERNAME_INTERVAL_SECONDS = 31_536_000;

const ProfileInfoForm: React.FC = () => {
  const user = useUserStore((state) => state.user);
  const updateProfile = useUserStore((state) => state.updateProfile);

  // L8/L9 (#1301): informational-only premium caps. The username-change cadence
  // and avatar/banner size limits are server-authoritative; these only drive UX
  // hints, never a hard client block.
  const usernameChangeIntervalSeconds = useEntitlement((e) => e.usernameChangeIntervalSeconds);
  const maxAvatarBytes = useEntitlement((e) => e.maxAvatarBytes);
  const maxBannerBytes = useEntitlement((e) => e.maxBannerBytes);
  // L9: a non-modal inline upsell banner for an over-limit avatar/banner pick.
  const [avatarUpsell, setAvatarUpsell] = useState<string | null>(null);
  const [bannerUpsell, setBannerUpsell] = useState<string | null>(null);
  // Whether the free yearly cadence applies (drives the L8 premium upsell copy).
  const onFreeUsernameCadence = usernameChangeIntervalSeconds >= FREE_USERNAME_INTERVAL_SECONDS;

  // Profile form state
  const [username, setUsername] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [bio, setBio] = useState('');
  const [links, setLinks] = useState<string[]>([]);
  const [profileErrors, setProfileErrors] = useState<ProfileFormErrors>({});
  const [isProfileSubmitting, setIsProfileSubmitting] = useState(false);
  const [profileSuccess, setProfileSuccess] = useState<string | null>(null);
  const [showUsernameConfirm, setShowUsernameConfirm] = useState(false);

  // Image upload hooks (declared after setProfileErrors to avoid TDZ confusion)
  const avatar = useImageUpload({
    maxSize: maxAvatarBytes,
    allowedTypes: ALLOWED_TYPES,
    onError: (msg) => setProfileErrors((prev) => ({ ...prev, avatar: msg })),
    initialUrl: user?.avatar_url,
  });

  const header = useImageUpload({
    maxSize: maxBannerBytes,
    allowedTypes: ALLOWED_TYPES,
    onError: (msg) => setProfileErrors((prev) => ({ ...prev, header: msg })),
    initialUrl: user?.header_image_url,
  });

  // Username change cooldown
  const usernameChangeEligibleAt = user?.username_change_eligible_at
    ? new Date(user.username_change_eligible_at)
    : null;
  /* eslint-disable @eslint-react/purity -- new Date() reads the current time to check cooldown; this is a pure computation with no observable side effect */
  const isUsernameOnCooldown = usernameChangeEligibleAt
    ? usernameChangeEligibleAt > new Date()
    : false;
  /* eslint-enable @eslint-react/purity -- end of new Date() cooldown check */

  // Extract stable reset functions so the init effect can depend on them
  // without churning on every render (useImageUpload returns a fresh object
  // each render; reset itself is memoized with [] deps inside the hook).
  const avatarReset = avatar.reset;
  const headerReset = header.reset;

  // Initialize form from user store.
  //
  // Rationale for the eslint-disable below:
  //  - Fields are read as a snapshot-at-switch so that an in-place mutation
  //    in the store (e.g., presence propagation, avatar upload, or this
  //    form's own save) does NOT reset the user's unsaved edits.
  //  - avatarReset / headerReset are stable useCallback references from
  //    useImageUpload (extracted at :59-60) and don't change between
  //    renders, so they're safe to include without causing churn.
  //  - The lint rule sees `user.username`, `user.display_name`, etc. inside
  //    the effect and demands `user` in deps. We deliberately depart from
  //    that: keying on `user?.id` is the load-bearing invariant.
  // Mirrors the snapshot-on-switch pattern at
  // ServerSettingsPage.tsx:200-225.
  useEffect(() => {
    if (user) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets username from user prop when user identity changes (snapshot-on-switch); not a render loop
      setUsername(user.username);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets displayName from user prop when user identity changes; not a render loop
      setDisplayName(user.display_name || '');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets bio from user prop when user identity changes; not a render loop
      setBio(user.bio || '');
      avatarReset(user.avatar_url || null);
      headerReset(user.header_image_url || null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets links from user prop when user identity changes; not a render loop
      setLinks(user.links || []);
    }
    // eslint-disable-next-line @eslint-react/exhaustive-deps -- snapshot-on-switch; see block comment above effect
  }, [user?.id, avatarReset, headerReset]);

  // Link handlers
  const handleAddLink = () => {
    if (links.length < MAX_LINKS) {
      setLinks([...links, '']);
    }
  };

  const handleLinkChange = (index: number, value: string) => {
    const updated = [...links];
    updated[index] = value;
    setLinks(updated);
    // Clear error for this link
    if (profileErrors[`link_${index}`]) {
      setProfileErrors((prev) => ({ ...prev, [`link_${index}`]: undefined }));
    }
  };

  const handleRemoveLink = (index: number) => {
    setLinks(links.filter((_, i) => i !== index));
  };

  // Profile form validation
  const validateProfileForm = (): boolean => {
    const errors: ProfileFormErrors = {};

    const trimmedUsername = username.trim();
    if (!trimmedUsername) {
      errors.username = 'Username is required';
    } else if (trimmedUsername.length < USERNAME_MIN) {
      errors.username = `Username must be at least ${USERNAME_MIN} characters`;
    } else if (trimmedUsername.length > USERNAME_MAX) {
      errors.username = `Username must be at most ${USERNAME_MAX} characters`;
    } else if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]*[a-zA-Z0-9]$/.test(trimmedUsername)) {
      errors.username =
        'Letters, numbers, periods, underscores, and hyphens only. Must start and end with a letter or number.';
    } else if (/[._-]{2,}/.test(trimmedUsername)) {
      errors.username =
        'Username cannot contain consecutive special characters (periods, underscores, hyphens).';
    }

    if (displayName.length > DISPLAY_NAME_MAX) {
      errors.displayName = `Display name must be at most ${DISPLAY_NAME_MAX} characters`;
    }

    if (bio.length > BIO_MAX) {
      errors.bio = `Bio must be at most ${BIO_MAX} characters`;
    }

    for (let i = 0; i < links.length; i++) {
      const link = links[i].trim();
      if (link && !link.startsWith('http://') && !link.startsWith('https://')) {
        errors[`link_${i}`] = 'Links must start with http:// or https://';
      }
    }

    setProfileErrors(errors);
    return Object.keys(errors).length === 0;
  };

  // Execute the profile update API call
  const doProfileUpdate = async () => {
    setIsProfileSubmitting(true);

    try {
      const updates: UpdateProfileData = {};

      if (username.trim().toLowerCase() !== user?.username) {
        updates.username = username.trim();
      }
      if (displayName !== (user?.display_name || '')) {
        updates.display_name = displayName.trim() || null;
      }
      if (bio !== (user?.bio || '')) {
        updates.bio = bio.trim() || null;
      }
      if (avatar.removed) {
        updates.avatar_url = ''; // Empty string signals "remove" to backend
      } else if (avatar.imageUrl && avatar.imageUrl !== user?.avatar_url) {
        updates.avatar_url = avatar.imageUrl;
      }
      if (header.removed) {
        updates.header_image_url = ''; // Empty string signals "remove" to backend
      } else if (header.imageUrl && header.imageUrl !== user?.header_image_url) {
        updates.header_image_url = header.imageUrl;
      }

      const filteredLinks = links.filter((l) => l.trim());
      const currentLinks = user?.links || [];
      if (JSON.stringify(filteredLinks) !== JSON.stringify(currentLinks)) {
        updates.links = filteredLinks;
      }

      if (Object.keys(updates).length === 0) {
        setProfileErrors({ general: 'No changes to save' });
        setIsProfileSubmitting(false);
        return;
      }

      await updateProfile(updates);
      setProfileSuccess('Profile updated successfully!');

      setTimeout(() => setProfileSuccess(null), 3000);
    } catch (error) {
      setProfileErrors({
        general: error instanceof Error ? error.message : 'Failed to update profile',
      });
    } finally {
      setIsProfileSubmitting(false);
    }
  };

  // Profile form submit — shows confirmation if username is changing
  const handleProfileSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setProfileErrors({});
    setProfileSuccess(null);

    if (!validateProfileForm()) return;

    // If username is changing, require explicit confirmation
    if (username.trim().toLowerCase() !== user?.username) {
      setShowUsernameConfirm(true);
      return;
    }

    await doProfileUpdate();
  };

  // Confirmed username change — proceed with the update
  const handleUsernameChangeConfirm = async () => {
    setShowUsernameConfirm(false);
    await doProfileUpdate();
  };

  // Check if profile form has unsaved changes
  const hasProfileChanges = (() => {
    if (!user) return false;
    if (username.trim() !== user.username) return true;
    if (displayName !== (user.display_name || '')) return true;
    if (bio !== (user.bio || '')) return true;
    if (avatar.removed) return true;
    if (avatar.imageUrl && avatar.imageUrl !== user.avatar_url) return true;
    if (header.removed) return true;
    if (header.imageUrl && header.imageUrl !== user.header_image_url) return true;
    const filteredLinks = links.filter((l) => l.trim());
    const currentLinks = user.links || [];
    if (JSON.stringify(filteredLinks) !== JSON.stringify(currentLinks)) return true;
    return false;
  })();

  // Profile form cancel
  const handleProfileCancel = () => {
    if (user) {
      setUsername(user.username);
      setDisplayName(user.display_name || '');
      setBio(user.bio || '');
      avatar.reset(user.avatar_url || null);
      header.reset(user.header_image_url || null);
      setLinks(user.links || []);
      setProfileErrors({});
      setProfileSuccess(null);
    }
  };

  const formatDate = (dateStr?: string) => {
    if (!dateStr) return '';
    return new Date(dateStr).toLocaleDateString('en-US', {
      year: 'numeric',
      month: 'long',
      day: 'numeric',
    });
  };

  /**
   * L9 (#1301): build the size-upsell banner text for an over-limit image, or
   * clear it when within the limit. Informational — does NOT block; the file
   * still flows to the upload hook (whose own validation decides acceptance).
   */
  const makeImageUpsell = (file: File | undefined, limit: number): string | null => {
    if (!file || file.size <= limit) return null;
    return `This file is ${formatFileSize(file.size)}. Free limit ${formatFileSize(
      limit
    )}. Premium raises it to ${formatFileSize(limit * PREMIUM_IMAGE_MULTIPLIER)}.`;
  };

  const handleAvatarFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    setAvatarUpsell(makeImageUpsell(e.target.files?.[0], maxAvatarBytes));
    avatar.handleChange(e);
  };

  const handleBannerFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    setBannerUpsell(makeImageUpsell(e.target.files?.[0], maxBannerBytes));
    header.handleChange(e);
  };

  return (
    <form className="profile-section" onSubmit={handleProfileSubmit}>
      <h2 className="profile-section-title">Profile Information</h2>

      {/* Avatar */}
      <div className="profile-avatar-section">
        <button
          type="button"
          className="profile-avatar-upload"
          onClick={avatar.handleClick}
          onKeyDown={avatar.handleKeyDown}
          aria-label="Upload avatar"
        >
          {(() => {
            if (avatar.preview) {
              return (
                <img src={avatar.preview} alt="Avatar preview" className="profile-avatar-preview" />
              );
            }
            if (user) {
              return (
                <span className="profile-avatar-initial">
                  {user.username.charAt(0).toUpperCase()}
                </span>
              );
            }
            return (
              <div className="profile-avatar-placeholder">
                <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
                  <path
                    d="M23 19a2 2 0 01-2 2H3a2 2 0 01-2-2V8a2 2 0 012-2h4l2-3h6l2 3h4a2 2 0 012 2z"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                  <circle cx="12" cy="13" r="4" stroke="currentColor" strokeWidth="1.5" />
                </svg>
                <span className="profile-avatar-placeholder-text">Upload</span>
              </div>
            );
          })()}
        </button>
        <div className="profile-avatar-actions">
          <span className="profile-avatar-actions-label">
            Click to upload an avatar (PNG, JPEG, GIF, WebP &mdash; max{' '}
            {formatFileSize(maxAvatarBytes)})
          </span>
          {avatar.preview && (
            <button
              type="button"
              className="profile-avatar-remove-btn"
              onClick={avatar.handleRemove}
            >
              Remove avatar
            </button>
          )}
        </div>
        <input
          ref={avatar.fileInputRef}
          type="file"
          accept="image/png,image/jpeg,image/gif,image/webp"
          onChange={handleAvatarFileChange}
          hidden
        />
      </div>
      {profileErrors.avatar && <span className="form-error">{profileErrors.avatar}</span>}
      {/* L9: non-modal avatar size upsell banner (#1301). */}
      {avatarUpsell && (
        <output className="image-upsell-banner">
          <span>{avatarUpsell}</span>
          <button
            type="button"
            className="image-upsell-dismiss"
            aria-label="Dismiss"
            onClick={() => setAvatarUpsell(null)}
          >
            ×
          </button>
        </output>
      )}

      {/* Header Image */}
      <div className="profile-header-image-section">
        <span className="form-label">Profile Banner</span>
        <button
          type="button"
          className="profile-header-upload"
          onClick={header.handleClick}
          onKeyDown={header.handleKeyDown}
          aria-label="Upload header image"
        >
          {header.preview ? (
            <img src={header.preview} alt="Header preview" className="profile-header-preview" />
          ) : (
            <div className="profile-header-placeholder">
              <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
                <path
                  d="M23 19a2 2 0 01-2 2H3a2 2 0 01-2-2V8a2 2 0 012-2h4l2-3h6l2 3h4a2 2 0 012 2z"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
                <circle cx="12" cy="13" r="4" stroke="currentColor" strokeWidth="1.5" />
              </svg>
              <span>Click to upload a banner image</span>
            </div>
          )}
        </button>
        <div className="profile-header-actions">
          <span className="profile-avatar-actions-label">
            PNG, JPEG, GIF, WebP &mdash; max {formatFileSize(maxBannerBytes)}. Recommended:
            600&times;120 or wider.
          </span>
          {header.preview && (
            <button
              type="button"
              className="profile-avatar-remove-btn"
              onClick={header.handleRemove}
            >
              Remove banner
            </button>
          )}
        </div>
        <input
          ref={header.fileInputRef}
          type="file"
          accept="image/png,image/jpeg,image/gif,image/webp"
          onChange={handleBannerFileChange}
          hidden
        />
      </div>
      {profileErrors.header && <span className="form-error">{profileErrors.header}</span>}
      {/* L9: non-modal banner size upsell banner (#1301). */}
      {bannerUpsell && (
        <output className="image-upsell-banner">
          <span>{bannerUpsell}</span>
          <button
            type="button"
            className="image-upsell-dismiss"
            aria-label="Dismiss"
            onClick={() => setBannerUpsell(null)}
          >
            ×
          </button>
        </output>
      )}

      {/* Username */}
      <div className="form-group">
        <label htmlFor="profile-username" className="form-label">
          Username
        </label>
        <input
          id="profile-username"
          type="text"
          className={`form-input ${profileErrors.username ? 'error' : ''}`}
          value={username}
          onChange={(e) => {
            setUsername(e.target.value);
            if (profileErrors.username)
              setProfileErrors((prev) => ({ ...prev, username: undefined }));
          }}
          disabled={isProfileSubmitting || isUsernameOnCooldown}
          maxLength={USERNAME_MAX}
        />
        {profileErrors.username && <span className="form-error">{profileErrors.username}</span>}
        {isUsernameOnCooldown && usernameChangeEligibleAt ? (
          <span className="form-hint form-hint-warning">
            Username changes are limited to once per year. Change again on{' '}
            {formatDate(usernameChangeEligibleAt.toISOString())}.
            {/* L8 (#1301): premium cadence upsell — informational note. */}
            {onFreeUsernameCadence && (
              <span className="username-cadence-upsell"> Premium: every 3 months.</span>
            )}
          </span>
        ) : (
          <span className="form-hint">
            {username.trim().length}/{USERNAME_MAX} characters. Can be changed once per year.
            {/* L8 (#1301): premium cadence upsell — informational note. */}
            {onFreeUsernameCadence && (
              <span className="username-cadence-upsell"> Premium: every 3 months.</span>
            )}
          </span>
        )}
      </div>

      {/* Display Name */}
      <div className="form-group">
        <label htmlFor="profile-display-name" className="form-label">
          Display Name
        </label>
        <input
          id="profile-display-name"
          type="text"
          className={`form-input ${profileErrors.displayName ? 'error' : ''}`}
          placeholder="How you want to be displayed"
          value={displayName}
          onChange={(e) => {
            setDisplayName(e.target.value);
            if (profileErrors.displayName)
              setProfileErrors((prev) => ({ ...prev, displayName: undefined }));
          }}
          disabled={isProfileSubmitting}
          maxLength={DISPLAY_NAME_MAX}
        />
        {profileErrors.displayName && (
          <span className="form-error">{profileErrors.displayName}</span>
        )}
        <span className="form-hint">Optional</span>
      </div>

      {/* Bio */}
      <div className="form-group">
        <label htmlFor="profile-bio" className="form-label">
          About
        </label>
        <textarea
          id="profile-bio"
          className={`form-input profile-bio-textarea ${profileErrors.bio ? 'error' : ''}`}
          placeholder="Tell us a bit about yourself"
          value={bio}
          onChange={(e) => {
            setBio(e.target.value);
            if (profileErrors.bio) setProfileErrors((prev) => ({ ...prev, bio: undefined }));
          }}
          disabled={isProfileSubmitting}
          maxLength={BIO_MAX}
        />
        {profileErrors.bio && <span className="form-error">{profileErrors.bio}</span>}
        <span className="form-hint">
          {bio.length}/{BIO_MAX} characters
        </span>
      </div>

      {/* Links */}
      <div className="form-group">
        <span className="form-label">Links</span>
        <div className="profile-links-list">
          {links.map((link, index) => {
            const linkErrorKey = `link_${index}`;
            return (
              <div key={linkErrorKey} className="profile-link-row">
                <input
                  type="url"
                  className={`form-input ${profileErrors[linkErrorKey] ? 'error' : ''}`}
                  placeholder="https://example.com"
                  value={link}
                  onChange={(e) => handleLinkChange(index, e.target.value)}
                  disabled={isProfileSubmitting}
                />
                <button
                  type="button"
                  className="profile-link-remove"
                  onClick={() => handleRemoveLink(index)}
                  disabled={isProfileSubmitting}
                  aria-label="Remove link"
                >
                  <svg width="14" height="14" viewBox="0 0 14 14" fill="none">
                    <path
                      d="M10.5 3.5L3.5 10.5M3.5 3.5l7 7"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                    />
                  </svg>
                </button>
                {profileErrors[linkErrorKey] && (
                  <span className="form-error" style={{ position: 'absolute' }}>
                    {profileErrors[linkErrorKey]}
                  </span>
                )}
              </div>
            );
          })}
          {links.length < MAX_LINKS && (
            <button
              type="button"
              className="profile-link-add"
              onClick={handleAddLink}
              disabled={isProfileSubmitting}
            >
              + Add link
            </button>
          )}
        </div>
        <span className="form-hint">
          {links.length}/{MAX_LINKS} links
        </span>
      </div>

      {/* General Error */}
      {profileErrors.general && (
        <div className="form-error-banner">
          <span>{profileErrors.general}</span>
        </div>
      )}

      {/* Success Message */}
      {profileSuccess && (
        <div className="form-success-banner">
          <span>{profileSuccess}</span>
        </div>
      )}

      {/* Actions */}
      <div className="profile-actions">
        <button
          type="button"
          className="profile-cancel-btn"
          onClick={handleProfileCancel}
          disabled={isProfileSubmitting || !hasProfileChanges}
        >
          Cancel
        </button>
        <button
          type="submit"
          className="profile-save-btn"
          disabled={isProfileSubmitting || !!profileSuccess}
        >
          {isProfileSubmitting ? (
            <>
              Saving...
              <LoadingSpinner size="small" inline />
            </>
          ) : (
            'Save Changes'
          )}
        </button>
      </div>

      {/* Member since */}
      {user?.created_at && (
        <div className="profile-member-since">Member since {formatDate(user.created_at)}</div>
      )}

      {/* Username change confirmation dialog */}
      <Modal
        isOpen={showUsernameConfirm}
        onClose={() => setShowUsernameConfirm(false)}
        title="Change Username"
        width="small"
      >
        <div className="username-change-confirm">
          <p>
            Are you sure you want to change your username from <strong>{user?.username}</strong> to{' '}
            <strong>{username.trim()}</strong>?
          </p>
          <p className="username-change-cooldown-notice">
            You will not be able to change your username again for 365 days.
          </p>
          <div className="username-change-actions">
            <button
              type="button"
              className="profile-cancel-btn"
              onClick={() => setShowUsernameConfirm(false)}
            >
              Cancel
            </button>
            <button
              type="button"
              className="profile-save-btn"
              onClick={handleUsernameChangeConfirm}
            >
              Confirm Change
            </button>
          </div>
        </div>
      </Modal>

      {/* Avatar Crop Editor */}
      <ImageCropEditor
        isOpen={avatar.showCrop}
        onClose={avatar.handleCropCancel}
        onConfirm={avatar.handleCropConfirm}
        imageFile={avatar.pendingFile}
        title="Crop Avatar"
        cropShape={{ type: 'circle' }}
        output={{ width: 512, height: 512, quality: 0.9 }}
        upload={{ endpoint: '/api/v1/media/upload/avatar' }}
      />

      {/* Banner Crop Editor */}
      <ImageCropEditor
        isOpen={header.showCrop}
        onClose={header.handleCropCancel}
        onConfirm={header.handleCropConfirm}
        imageFile={header.pendingFile}
        title="Crop Banner"
        cropShape={{ type: 'rectangle' }}
        output={{ width: 1200, height: 240, quality: 0.9 }}
        upload={{ endpoint: '/api/v1/media/upload/banner' }}
      />
    </form>
  );
};

export default ProfileInfoForm;
