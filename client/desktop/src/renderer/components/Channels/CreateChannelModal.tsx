import React, { useEffect, useRef, useState } from 'react';
import { useFormState } from '../../hooks/useFormState';
import { NAME_MAX, validateChannelName, getChannelTypeIcon } from '../../utils/channelHelpers';
import ChannelEmojiField from './ChannelEmojiField';
import Modal from '../ui/Modal';
import CustomSelect from '../ui/CustomSelect';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useServerStore } from '../../stores/serverStore';
import { useChannelStore } from '../../stores/channelStore';
import { useUserStore } from '../../stores/userStore';
import { apiFetch } from '../../services/apiClient';
import { e2eeService } from '../../services/e2eeService';
import { useAuthStore } from '../../stores/authStore';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
  type RuntimeServerSelection,
} from '../../services/runtimeServerBase';
import { Channel } from '../../types/chat';
import './CreateChannelModal.css';

interface CreateChannelModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSuccess?: (channel: Channel) => void;
}

interface FormErrors {
  name?: string;
  type?: string;
  general?: string;
}

interface MemberPublicKey {
  user_id: string;
  public_key: string;
  key_version: number;
}

interface GeneratedWrappedKeys {
  wrappedKeys: Record<string, string>;
  wrappedKeyVersions: Record<string, number>;
}

const MAX_WRAPPED_KEYS_PER_REQUEST = 500;
const MAX_RATE_LIMIT_RETRIES = 3;
const RATE_LIMIT_RETRY_FALLBACK_MS = 60_000;
const MAX_RATE_LIMIT_RETRY_DELAY_MS = 60_000;

interface KeyDistributionOwner {
  authGeneration: number;
  serverSelection: RuntimeServerSelection;
  signal: AbortSignal;
}

interface CreatedChannelWithKeys {
  channel: Channel;
  linkedTextChannel?: Channel;
  remainingWrappedKeys: [string, string][];
  wrappedKeyVersions: Record<string, number>;
}

interface CreateChannelWithKeysParams {
  serverID: string;
  name: string;
  emoji: string;
  type: 'text' | 'voice' | 'bulletin';
  groupId: string;
  currentUserId?: string;
  owner: KeyDistributionOwner;
}

function keyDistributionOwnerIsCurrent(owner: KeyDistributionOwner): boolean {
  return (
    !owner.signal.aborted &&
    useAuthStore.getState().authGeneration === owner.authGeneration &&
    runtimeServerSelectionIsCurrent(owner.serverSelection)
  );
}

function waitForRateLimitRetry(delay: number, signal: AbortSignal): Promise<boolean> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve(false);
      return;
    }
    const timeout = setTimeout(finish, delay, true);
    function finish(shouldRetry: boolean) {
      clearTimeout(timeout);
      signal.removeEventListener('abort', onAbort);
      resolve(shouldRetry);
    }
    function onAbort() {
      finish(false);
    }
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

function rateLimitRetryDelay(response: Response): number {
  const retryAfterHeader = response.headers.get('Retry-After');
  const retryAfterSeconds = retryAfterHeader === null ? Number.NaN : Number(retryAfterHeader);
  return Number.isFinite(retryAfterSeconds) && retryAfterSeconds >= 0
    ? Math.min(MAX_RATE_LIMIT_RETRY_DELAY_MS, Math.max(1000, retryAfterSeconds * 1000))
    : RATE_LIMIT_RETRY_FALLBACK_MS;
}

/** Generate wrapped E2EE channel keys for the current server members. */
async function generateWrappedKeysForMembers(
  members: MemberPublicKey[]
): Promise<GeneratedWrappedKeys> {
  const memberPublicKeys = new Map<string, string>();
  const wrappedKeyVersions: Record<string, number> = {};
  for (const member of members) {
    if (!member.public_key || !Number.isInteger(member.key_version) || member.key_version < 1) {
      continue;
    }
    memberPublicKeys.set(member.user_id, member.public_key);
    wrappedKeyVersions[member.user_id] = member.key_version;
  }

  if (memberPublicKeys.size === 0) {
    throw new Error('No member public keys available for E2EE channel creation');
  }

  const wrappedKeyMap = await e2eeService.createChannelKeys(memberPublicKeys);
  return { wrappedKeys: Object.fromEntries(wrappedKeyMap), wrappedKeyVersions };
}

async function fetchMemberPublicKeys(
  serverID: string,
  signal: AbortSignal
): Promise<MemberPublicKey[]> {
  const response = await apiFetch(`/api/v1/servers/${serverID}/member-public-keys`, { signal });
  if (!response.ok) {
    throw new Error('Failed to fetch member public keys');
  }
  const data = (await response.json()) as { members?: MemberPublicKey[] };
  return Array.isArray(data.members) ? data.members : [];
}

function createWrappedKeyBatch(
  remainingWrappedKeys: [string, string][],
  wrappedKeyVersions: Record<string, number>,
  start: number
) {
  const wrappedKeys = Object.fromEntries(
    remainingWrappedKeys.slice(start, start + MAX_WRAPPED_KEYS_PER_REQUEST)
  );
  return {
    wrappedKeys,
    wrappedKeyVersions: Object.fromEntries(
      Object.keys(wrappedKeys).map((userID) => [userID, wrappedKeyVersions[userID]])
    ),
  };
}

async function distributeWrappedKeyBatch(
  channelID: string,
  wrappedKeys: Record<string, string>,
  wrappedKeyVersions: Record<string, number>,
  owner: KeyDistributionOwner
): Promise<boolean> {
  let retries = 0;
  while (keyDistributionOwnerIsCurrent(owner)) {
    const response = await apiFetch(`/api/v1/channels/${channelID}/keys`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        wrapped_keys: wrappedKeys,
        wrapped_key_versions: wrappedKeyVersions,
        key_version: 1,
      }),
      signal: owner.signal,
    });
    if (!keyDistributionOwnerIsCurrent(owner)) return false;
    if (response.ok) return true;
    if (response.status !== 429) {
      const data = (await response.json().catch(() => ({}))) as { error?: string };
      throw new Error(data.error || 'Failed to distribute channel keys to all members');
    }
    if (retries++ >= MAX_RATE_LIMIT_RETRIES) {
      throw new Error('Channel key distribution is rate limited; try again later');
    }
    if (!(await waitForRateLimitRetry(rateLimitRetryDelay(response), owner.signal))) return false;
  }
  return false;
}

async function distributeRemainingWrappedKeys(
  channelIDs: string[],
  remainingWrappedKeys: [string, string][],
  wrappedKeyVersions: Record<string, number>,
  owner: KeyDistributionOwner
): Promise<boolean> {
  for (let index = 0; index < remainingWrappedKeys.length; index += MAX_WRAPPED_KEYS_PER_REQUEST) {
    const wrappedKeyBatch = createWrappedKeyBatch(remainingWrappedKeys, wrappedKeyVersions, index);
    for (const channelID of channelIDs) {
      if (
        !(await distributeWrappedKeyBatch(
          channelID,
          wrappedKeyBatch.wrappedKeys,
          wrappedKeyBatch.wrappedKeyVersions,
          owner
        ))
      )
        return false;
    }
  }
  return true;
}

async function createChannelWithKeys(
  params: CreateChannelWithKeysParams
): Promise<CreatedChannelWithKeys | null> {
  if (!e2eeService.isInitialized) {
    throw new Error('Setting up secure messaging — try again in a moment.');
  }
  const members = await fetchMemberPublicKeys(params.serverID, params.owner.signal);
  if (!keyDistributionOwnerIsCurrent(params.owner)) return null;
  const { wrappedKeys, wrappedKeyVersions } = await generateWrappedKeysForMembers(members);
  if (!keyDistributionOwnerIsCurrent(params.owner)) return null;
  if (!params.currentUserId || !wrappedKeys[params.currentUserId]) {
    throw new Error('Your public key is unavailable for secure channel creation');
  }
  const otherWrappedKeys = Object.entries(wrappedKeys).filter(
    ([userID]) => userID !== params.currentUserId
  );
  const initialWrappedKeys = Object.fromEntries([
    [params.currentUserId, wrappedKeys[params.currentUserId]],
    ...otherWrappedKeys.slice(0, MAX_WRAPPED_KEYS_PER_REQUEST - 1),
  ]);
  const response = await apiFetch('/api/v1/channels', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      server_id: params.serverID,
      name: params.name.trim(),
      type: params.type,
      emoji: params.emoji.trim() || undefined,
      group_id: params.groupId || undefined,
      wrapped_keys: initialWrappedKeys,
      wrapped_key_versions: Object.fromEntries(
        Object.keys(initialWrappedKeys).map((userID) => [userID, wrappedKeyVersions[userID]])
      ),
    }),
    signal: params.owner.signal,
  });
  if (!keyDistributionOwnerIsCurrent(params.owner)) return null;
  if (!response.ok) {
    const data = (await response.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error || 'Failed to create channel');
  }
  const data = (await response.json().catch(() => ({}))) as {
    channel: Channel;
    linked_text_channel?: Channel;
  };
  if (!keyDistributionOwnerIsCurrent(params.owner)) return null;
  if (!data.channel) throw new Error('Failed to create channel');
  return {
    channel: data.channel,
    linkedTextChannel: data.linked_text_channel,
    remainingWrappedKeys: otherWrappedKeys.slice(MAX_WRAPPED_KEYS_PER_REQUEST - 1),
    wrappedKeyVersions,
  };
}

const CreateChannelModal: React.FC<CreateChannelModalProps> = ({ isOpen, onClose, onSuccess }) => {
  const [name, setName] = useState('');
  const [emoji, setEmoji] = useState('');
  const [type, setType] = useState<'text' | 'voice' | 'bulletin'>('text');
  const [groupId, setGroupId] = useState<string>('');
  const [hasPartialChannel, setHasPartialChannel] = useState(false);
  const {
    errors,
    setErrors,
    isSubmitting,
    setIsSubmitting,
    successMessage,
    setSuccessMessage,
    reset: resetFormState,
  } = useFormState<FormErrors>();
  const activeServerId = useServerStore((state) => state.activeServerId);
  const addChannel = useChannelStore((state) => state.addChannel);
  const channelGroups = useChannelStore((state) => state.channelGroups);
  const currentUserId = useUserStore((state) => state.user?.id);
  const submissionControllerRef = useRef<AbortController | null>(null);
  const submissionOwnerRef = useRef<KeyDistributionOwner | null>(null);
  const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (!isOpen) submissionControllerRef.current?.abort();
  }, [isOpen]);

  useEffect(() => {
    const unsubscribe = useAuthStore.subscribe((auth) => {
      const owner = submissionOwnerRef.current;
      if (owner && auth.authGeneration !== owner.authGeneration) {
        submissionControllerRef.current?.abort();
      }
    });
    return () => {
      unsubscribe();
      submissionControllerRef.current?.abort();
      if (closeTimerRef.current) clearTimeout(closeTimerRef.current);
    };
  }, []);

  const resetForm = () => {
    setName('');
    setEmoji('');
    setType('text');
    setGroupId('');
    setHasPartialChannel(false);
    resetFormState();
  };

  const handleClose = () => {
    if (isSubmitting && !hasPartialChannel) return;

    submissionControllerRef.current?.abort();
    if (closeTimerRef.current !== null) {
      clearTimeout(closeTimerRef.current);
      closeTimerRef.current = null;
    }
    resetForm();
    onClose();
  };

  const validateForm = (): boolean => {
    const newErrors: FormErrors = {};
    const nameError = validateChannelName(name);
    if (nameError) {
      newErrors.name = nameError;
    }
    if (!type) {
      newErrors.type = 'Channel type is required';
    }
    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    if (!validateForm() || !activeServerId) {
      return;
    }

    submissionControllerRef.current?.abort();
    const controller = new AbortController();
    submissionControllerRef.current = controller;
    const owner: KeyDistributionOwner = {
      authGeneration: useAuthStore.getState().authGeneration,
      serverSelection: captureRuntimeServerSelection(),
      signal: controller.signal,
    };
    submissionOwnerRef.current = owner;
    const isCurrent = () => keyDistributionOwnerIsCurrent(owner);
    setIsSubmitting(true);
    setErrors({});

    let channelCreated = false;
    try {
      const created = await createChannelWithKeys({
        serverID: activeServerId,
        name,
        emoji,
        type,
        groupId,
        currentUserId,
        owner,
      });
      if (!created) return;

      channelCreated = true;
      setHasPartialChannel(true);

      addChannel(created.channel);
      onSuccess?.(created.channel);
      const channelIDs = [
        created.channel.id,
        ...(created.linkedTextChannel ? [created.linkedTextChannel.id] : []),
      ];
      if (
        !(await distributeRemainingWrappedKeys(
          channelIDs,
          created.remainingWrappedKeys,
          created.wrappedKeyVersions,
          owner
        ))
      ) {
        return;
      }
      if (!isCurrent()) return;

      // Show success message
      setSuccessMessage('Channel created successfully!');

      // Close modal after short delay
      closeTimerRef.current = setTimeout(() => {
        closeTimerRef.current = null;
        resetForm();
        onClose();
      }, 1000);
    } catch (error) {
      if (!isCurrent()) return;
      const fallbackMessage = channelCreated
        ? 'key distribution failed'
        : 'Failed to create channel';
      const errorMessage = error instanceof Error ? error.message : fallbackMessage;
      setErrors({
        general: channelCreated ? `Channel created, but ${errorMessage}` : errorMessage,
      });
    } finally {
      if (submissionControllerRef.current === controller) {
        submissionControllerRef.current = null;
        submissionOwnerRef.current = null;
        setIsSubmitting(false);
      }
    }
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Create Channel">
      <form onSubmit={handleSubmit} className="create-channel-form">
        {/* General Error */}
        {errors.general && <div className="channel-form-error-banner">{errors.general}</div>}

        {/* Success Message */}
        {successMessage && <div className="channel-form-success-banner">{successMessage}</div>}

        {/* Channel Name */}
        <div className="channel-form-group">
          <label htmlFor="channel-name" className="channel-form-label">
            Channel Name <span className="required">*</span>
          </label>
          <input
            id="channel-name"
            type="text"
            className={`channel-form-input ${errors.name ? 'error' : ''}`}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="general-chat"
            maxLength={NAME_MAX}
            disabled={isSubmitting}
            autoFocus
          />
          {errors.name && <span className="channel-form-error">{errors.name}</span>}
          <span className="channel-form-hint">
            {name.length}/{NAME_MAX} characters
          </span>
        </div>

        <ChannelEmojiField emoji={emoji} onChange={setEmoji} disabled={isSubmitting} />

        {/* Channel Type */}
        <div className="channel-form-group">
          <span className="channel-form-label">
            Channel Type <span className="required">*</span>
          </span>
          <div className="channel-type-selector">
            <button
              type="button"
              className={`channel-type-option ${type === 'text' ? 'selected' : ''}`}
              onClick={() => setType('text')}
              disabled={isSubmitting}
            >
              <span className="channel-type-icon">{getChannelTypeIcon('text')}</span>
              <div className="channel-type-info">
                <span className="channel-type-name">Text</span>
                <span className="channel-type-desc">Send messages, images, and files</span>
              </div>
            </button>

            <button
              type="button"
              className={`channel-type-option ${type === 'voice' ? 'selected' : ''}`}
              onClick={() => setType('voice')}
              disabled={isSubmitting}
            >
              <span className="channel-type-icon">{getChannelTypeIcon('voice')}</span>
              <div className="channel-type-info">
                <span className="channel-type-name">Voice</span>
                <span className="channel-type-desc">Voice and video conversations</span>
              </div>
            </button>

            <button
              type="button"
              className={`channel-type-option ${type === 'bulletin' ? 'selected' : ''}`}
              onClick={() => setType('bulletin')}
              disabled={isSubmitting}
            >
              <span className="channel-type-icon">{getChannelTypeIcon('bulletin')}</span>
              <div className="channel-type-info">
                <span className="channel-type-name">Bulletin</span>
                <span className="channel-type-desc">Important announcements only</span>
              </div>
            </button>
          </div>
          {errors.type && <span className="channel-form-error">{errors.type}</span>}
          <span className="channel-form-hint">Channel type cannot be changed after creation</span>
        </div>

        {/* Channel Group */}
        {channelGroups.length > 0 && (
          <div className="channel-form-group">
            <label htmlFor="channel-group" className="channel-form-label">
              Channel Group
            </label>
            <CustomSelect
              options={[
                { value: '', label: 'Uncategorized' },
                ...channelGroups.map((g) => ({ value: g.id, label: g.name })),
              ]}
              value={groupId}
              onChange={(v) => setGroupId(v)}
              disabled={isSubmitting}
              className="channel-form-input"
            />
            <span className="channel-form-hint">Organize this channel into a group</span>
          </div>
        )}

        {/* Submit Button */}
        <div className="form-actions">
          <button
            type="button"
            className="btn btn-secondary"
            onClick={handleClose}
            disabled={isSubmitting && !hasPartialChannel}
          >
            Cancel
          </button>
          <button
            type="submit"
            className="btn btn-primary"
            disabled={isSubmitting || hasPartialChannel || !activeServerId}
          >
            {isSubmitting ? (
              <>
                <LoadingSpinner size="small" />
                <span>Creating...</span>
              </>
            ) : (
              'Create Channel'
            )}
          </button>
        </div>
      </form>
    </Modal>
  );
};

export default CreateChannelModal;
