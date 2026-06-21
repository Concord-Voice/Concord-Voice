import React, { useState } from 'react';
import { useUserStore } from '../../stores/userStore';
import PasswordStrength from '../Auth/PasswordStrength';
import LoadingSpinner from '../Auth/LoadingSpinner';
import type { PasswordFormErrors } from './profileConstants';

const PasswordChangeForm: React.FC = () => {
  const changePassword = useUserStore((state) => state.changePassword);

  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [passwordErrors, setPasswordErrors] = useState<PasswordFormErrors>({});
  const [isPasswordSubmitting, setIsPasswordSubmitting] = useState(false);
  const [passwordSuccess, setPasswordSuccess] = useState<string | null>(null);

  const handlePasswordSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const errors: PasswordFormErrors = {};

    if (!currentPassword) errors.currentPassword = 'Current password is required';
    if (!newPassword) {
      errors.newPassword = 'New password is required';
    } else if (newPassword.length < 12) {
      errors.newPassword = 'Password must be at least 12 characters';
    }
    if (!confirmPassword) {
      errors.confirmPassword = 'Please confirm your new password';
    } else if (newPassword !== confirmPassword) {
      errors.confirmPassword = 'Passwords do not match';
    }

    if (Object.keys(errors).length > 0) {
      setPasswordErrors(errors);
      return;
    }

    setIsPasswordSubmitting(true);
    setPasswordErrors({});
    setPasswordSuccess(null);

    const result = await changePassword(currentPassword, newPassword);

    if (result.success) {
      setPasswordSuccess('Password changed successfully!');
      setCurrentPassword('');
      setNewPassword('');
      setConfirmPassword('');
      setTimeout(() => setPasswordSuccess(null), 3000);
    } else {
      setPasswordErrors({ general: result.error || 'Failed to change password' });
    }

    setIsPasswordSubmitting(false);
  };

  return (
    <form className="profile-section" onSubmit={handlePasswordSubmit}>
      <h2 className="profile-section-title">Change Password</h2>

      {/* Current Password */}
      <div className="form-group">
        <label htmlFor="current-password" className="form-label">
          Current Password
        </label>
        <input
          id="current-password"
          type="password"
          className={`form-input ${passwordErrors.currentPassword ? 'error' : ''}`}
          placeholder="Enter your current password"
          value={currentPassword}
          onChange={(e) => {
            setCurrentPassword(e.target.value);
            if (passwordErrors.currentPassword)
              setPasswordErrors((prev) => ({ ...prev, currentPassword: undefined }));
          }}
          disabled={isPasswordSubmitting}
        />
        {passwordErrors.currentPassword && (
          <span className="form-error">{passwordErrors.currentPassword}</span>
        )}
      </div>

      {/* New Password */}
      <div className="form-group">
        <label htmlFor="new-password" className="form-label">
          New Password
        </label>
        <input
          id="new-password"
          type="password"
          className={`form-input ${passwordErrors.newPassword ? 'error' : ''}`}
          placeholder="Enter your new password"
          value={newPassword}
          onChange={(e) => {
            setNewPassword(e.target.value);
            if (passwordErrors.newPassword)
              setPasswordErrors((prev) => ({ ...prev, newPassword: undefined }));
          }}
          disabled={isPasswordSubmitting}
        />
        {passwordErrors.newPassword && (
          <span className="form-error">{passwordErrors.newPassword}</span>
        )}
        <PasswordStrength password={newPassword} />
      </div>

      {/* Confirm New Password */}
      <div className="form-group">
        <label htmlFor="confirm-new-password" className="form-label">
          Confirm New Password
        </label>
        <input
          id="confirm-new-password"
          type="password"
          className={`form-input ${passwordErrors.confirmPassword ? 'error' : ''}`}
          placeholder="Confirm your new password"
          value={confirmPassword}
          onChange={(e) => {
            setConfirmPassword(e.target.value);
            if (passwordErrors.confirmPassword)
              setPasswordErrors((prev) => ({ ...prev, confirmPassword: undefined }));
          }}
          disabled={isPasswordSubmitting}
        />
        {passwordErrors.confirmPassword && (
          <span className="form-error">{passwordErrors.confirmPassword}</span>
        )}
      </div>

      {/* Password Error */}
      {passwordErrors.general && (
        <div className="form-error-banner">
          <span>{passwordErrors.general}</span>
        </div>
      )}

      {/* Password Success */}
      {passwordSuccess && (
        <div className="form-success-banner">
          <span>{passwordSuccess}</span>
        </div>
      )}

      {/* Password Action */}
      <div className="profile-actions">
        <button
          type="submit"
          className="profile-password-btn"
          disabled={isPasswordSubmitting || !!passwordSuccess}
        >
          {isPasswordSubmitting ? (
            <>
              Changing...
              <LoadingSpinner size="small" inline />
            </>
          ) : (
            'Change Password'
          )}
        </button>
      </div>
    </form>
  );
};

export default PasswordChangeForm;
