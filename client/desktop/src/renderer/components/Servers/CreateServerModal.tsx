import React, { useState } from 'react';
import { useFormState } from '../../hooks/useFormState';
import { useImageUpload } from '../../hooks/useImageUpload';
import Modal from '../ui/Modal';
import ImageCropEditor from '../ui/ImageCropEditor';
import IconUploadArea from './IconUploadArea';
import BannerUploadArea from './BannerUploadArea';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useServerStore } from '../../stores/serverStore';
import { apiFetch } from '../../services/apiClient';
import { ServerWithRole } from '../../types/server';
import {
  MAX_ICON_SIZE,
  MAX_BANNER_SIZE,
  ALLOWED_TYPES,
  NAME_MIN,
  NAME_MAX,
  type ServerFormErrors,
} from './serverConstants';
import './CreateServerModal.css';

interface CreateServerModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSuccess: (server: ServerWithRole) => void;
}

const CreateServerModal: React.FC<CreateServerModalProps> = ({ isOpen, onClose, onSuccess }) => {
  const [name, setName] = useState('');
  const {
    errors,
    setErrors,
    isSubmitting,
    setIsSubmitting,
    successMessage,
    setSuccessMessage,
    reset: resetFormState,
  } = useFormState<ServerFormErrors>();

  const icon = useImageUpload({
    maxSize: MAX_ICON_SIZE,
    allowedTypes: ALLOWED_TYPES,
    onError: (msg) => setErrors((prev) => ({ ...prev, icon: msg })),
  });

  const banner = useImageUpload({
    maxSize: MAX_BANNER_SIZE,
    allowedTypes: ALLOWED_TYPES,
    onError: (msg) => setErrors((prev) => ({ ...prev, banner: msg })),
  });

  const resetForm = () => {
    setName('');
    icon.reset();
    banner.reset();
    resetFormState();
  };

  const handleClose = () => {
    if (!isSubmitting) {
      resetForm();
      onClose();
    }
  };

  const validateForm = (): boolean => {
    const newErrors: ServerFormErrors = {};
    const trimmed = name.trim();

    if (!trimmed) {
      newErrors.name = 'Server name is required';
    } else if (trimmed.length < NAME_MIN) {
      newErrors.name = `Server name must be at least ${NAME_MIN} characters`;
    } else if (trimmed.length > NAME_MAX) {
      newErrors.name = `Server name must be at most ${NAME_MAX} characters`;
    }

    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErrors({});
    setSuccessMessage(null);

    if (!validateForm()) return;

    setIsSubmitting(true);

    try {
      const body: { name: string; icon_url?: string; banner_url?: string } = {
        name: name.trim(),
      };
      if (icon.imageUrl) body.icon_url = icon.imageUrl;
      if (banner.imageUrl) body.banner_url = banner.imageUrl;

      const response = await apiFetch('/api/v1/servers', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      const data = await response.json();

      if (!response.ok) {
        throw new Error(data.error || 'Failed to create server');
      }

      setSuccessMessage('Server created successfully!');

      const serverWithRole: ServerWithRole = {
        ...data.server,
        role: data.role as ServerWithRole['role'],
      };
      useServerStore.getState().addServer(serverWithRole);

      setTimeout(() => {
        onSuccess(serverWithRole);
        resetForm();
        onClose();
      }, 800);
    } catch (error) {
      setErrors({
        general:
          error instanceof Error ? error.message : 'Failed to create server. Please try again.',
      });
      setIsSubmitting(false);
    }
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Create a Server" width="medium">
      <form className="create-server-form" onSubmit={handleSubmit}>
        <IconUploadArea
          preview={icon.preview}
          error={errors.icon}
          onClick={icon.handleClick}
          onKeyDown={icon.handleKeyDown}
          onRemove={icon.handleRemove}
          onFileChange={icon.handleChange}
          fileInputRef={icon.fileInputRef}
        />

        <BannerUploadArea
          preview={banner.preview}
          error={errors.banner}
          onClick={banner.handleClick}
          onKeyDown={banner.handleKeyDown}
          onRemove={banner.handleRemove}
          onFileChange={banner.handleChange}
          fileInputRef={banner.fileInputRef}
          hint="PNG, JPEG, GIF, WebP — max 2MB. Optional."
        />

        {/* Server Name */}
        <div className="form-group">
          <label htmlFor="create-server-name" className="form-label">
            Server Name
          </label>
          <input
            id="create-server-name"
            type="text"
            className={`form-input ${errors.name ? 'error' : ''}`}
            placeholder="My Awesome Server"
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              if (errors.name) setErrors((prev) => ({ ...prev, name: undefined }));
            }}
            disabled={isSubmitting}
            autoFocus
            maxLength={NAME_MAX}
          />
          {errors.name && <span className="form-error">{errors.name}</span>}
          <span className="form-hint">
            {name.trim().length}/{NAME_MAX} characters
          </span>
        </div>

        {/* General Error */}
        {errors.general && (
          <div className="form-error-banner">
            <span>{errors.general}</span>
          </div>
        )}

        {/* Success Message */}
        {successMessage && (
          <div className="form-success-banner">
            <span>{successMessage}</span>
          </div>
        )}

        {/* Buttons */}
        <div className="create-server-actions">
          <button
            type="button"
            className="create-server-cancel-btn"
            onClick={handleClose}
            disabled={isSubmitting}
          >
            Cancel
          </button>
          <button
            type="submit"
            className="create-server-submit-btn"
            disabled={isSubmitting || !!successMessage}
          >
            {isSubmitting ? (
              <>
                Creating...
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              'Create Server'
            )}
          </button>
        </div>
      </form>

      <ImageCropEditor
        isOpen={icon.showCrop}
        onClose={icon.handleCropCancel}
        onConfirm={icon.handleCropConfirm}
        imageFile={icon.pendingFile}
        title="Crop Server Icon"
        cropShape={{ type: 'circle' }}
        output={{ width: 512, height: 512, quality: 0.9 }}
      />

      <ImageCropEditor
        isOpen={banner.showCrop}
        onClose={banner.handleCropCancel}
        onConfirm={banner.handleCropConfirm}
        imageFile={banner.pendingFile}
        title="Crop Server Banner"
        cropShape={{ type: 'rectangle' }}
        output={{ width: 1200, height: 240, quality: 0.9 }}
      />
    </Modal>
  );
};

export default CreateServerModal;
