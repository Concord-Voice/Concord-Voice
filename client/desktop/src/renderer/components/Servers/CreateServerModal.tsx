import React, { useState } from 'react';
import { useFormState } from '../../hooks/ui/useFormState';
import { useImageUpload } from '../../hooks/messaging/useImageUpload';
import Modal from '../ui/Modal';
import ImageCropEditor from '../ui/ImageCropEditor';
import IconUploadArea from './IconUploadArea';
import BannerUploadArea from './BannerUploadArea';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useServerStore } from '../../stores/chat/serverStore';
import { apiFetch } from '../../services/apiClient';
import { ServerWithRole } from '../../types/server';
import {
  MAX_ICON_SIZE,
  MAX_BANNER_SIZE,
  ALLOWED_TYPES,
  validateServerName,
  type ServerFormErrors,
} from './serverConstants';
import { ServerNameField, ServerFormBanners } from './ServerNameField';
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
    const newErrors = validateServerName(name);
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
          hint="PNG, JPEG, GIF, WebP — max 5MB. Optional."
        />

        <ServerNameField
          inputId="create-server-name"
          name={name}
          error={errors.name}
          disabled={isSubmitting}
          autoFocus
          onChange={(value) => {
            setName(value);
            if (errors.name) setErrors((prev) => ({ ...prev, name: undefined }));
          }}
        />

        <ServerFormBanners generalError={errors.general} successMessage={successMessage} />

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
