import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { presenceOverrideSyncService } from '../../services/system/presenceOverrideSync';
import { useFriendStore, type Friend } from '../../stores/chat/friendStore';
import { usePresenceOverrideStore } from '../../stores/ui/presenceOverrideStore';
import { comparePresenceOverrideUserIds } from '../../utils/policy/presenceOverrides';
import PresenceExceptionModal from './PresenceExceptionModal';
import './PresenceExceptions.css';

interface PresenceExceptionsProps {
  categoryManagerOpen: boolean;
  onOpenCategoryManager: () => void;
}

interface RetryRemoval {
  excludedUserIds: string[];
  label: string;
  baseAppliedVersion: number;
  nextFocusUserId: string | null;
}

const SAVE_ERROR = 'Failed to save presence exceptions';

function displayName(friend: Friend): string {
  return friend.displayName?.trim() || friend.username;
}

function conflictFocusUserId(
  preferredUserId: string | null,
  authoritativeUserIds: readonly string[]
): string | null {
  if (preferredUserId && authoritativeUserIds.includes(preferredUserId)) {
    return preferredUserId;
  }
  return authoritativeUserIds[0] ?? null;
}

const PresenceExceptions: React.FC<PresenceExceptionsProps> = ({
  categoryManagerOpen,
  onOpenCategoryManager,
}) => {
  const excludedUserIds = usePresenceOverrideStore((state) => state.excludedUserIds);
  const appliedVersion = usePresenceOverrideStore((state) => state.appliedVersion);
  const loading = usePresenceOverrideStore((state) => state.loading);
  const saving = usePresenceOverrideStore((state) => state.saving);
  const conflict = usePresenceOverrideStore((state) => state.conflict);
  const error = usePresenceOverrideStore((state) => state.error);
  const friends = useFriendStore((state) => state.friends);
  const friendsHydrated = useFriendStore((state) => state.friendsHydrated);
  const friendsLoading = useFriendStore((state) => state.isLoading);
  const friendsError = useFriendStore((state) => state.error);
  const fetchFriends = useFriendStore((state) => state.fetchFriends);
  const [editorOpen, setEditorOpen] = useState(false);
  const [announcement, setAnnouncement] = useState('');
  const [retryRemoval, setRetryRemoval] = useState<RetryRemoval | null>(null);
  const [removalLocked, setRemovalLocked] = useState(false);
  const [removalFocusRequest, setRemovalFocusRequest] = useState<{
    userId: string | null;
  } | null>(null);
  const removalInFlightRef = useRef(false);
  const addButtonRef = useRef<HTMLButtonElement>(null);
  const removeButtonMapRef = useRef(new Map<string, HTMLButtonElement>());
  const handledRemovalFocusRequestRef = useRef<typeof removalFocusRequest>(null);
  const managerWasOpenRef = useRef(categoryManagerOpen);
  const busy = saving || removalLocked;
  const friendRosterReady = friendsHydrated && !friendsLoading;
  let friendRosterErrorMessage = 'Friend list unavailable. Retry before editing exceptions.';
  if (friendsHydrated) {
    friendRosterErrorMessage = 'Friend list refresh failed. Showing the last loaded list.';
  }

  const friendsByUserId = useMemo(
    () => new Map(friends.map((friend) => [friend.userId, friend])),
    [friends]
  );

  useEffect(() => {
    if (!friendsHydrated && !friendsLoading && friendsError === null) {
      fetchFriends();
    }
  }, [fetchFriends, friendsError, friendsHydrated, friendsLoading]);

  useEffect(() => {
    if (managerWasOpenRef.current && !categoryManagerOpen) {
      addButtonRef.current?.focus();
    }
    managerWasOpenRef.current = categoryManagerOpen;
  }, [categoryManagerOpen]);

  useEffect(() => {
    if (!removalFocusRequest || handledRemovalFocusRequestRef.current === removalFocusRequest) {
      return;
    }
    const nextRemoveButton = removalFocusRequest.userId
      ? removeButtonMapRef.current.get(removalFocusRequest.userId)
      : undefined;
    (nextRemoveButton ?? addButtonRef.current)?.focus();
    handledRemovalFocusRequestRef.current = removalFocusRequest;
  }, [excludedUserIds, removalFocusRequest]);

  const saveRemoval = useCallback(async (removal: RetryRemoval) => {
    const current = usePresenceOverrideStore.getState();
    if (current.saving || removalInFlightRef.current) return;
    removalInFlightRef.current = true;
    setRemovalLocked(true);
    try {
      setAnnouncement('');
      if (current.appliedVersion !== removal.baseAppliedVersion) {
        current.setConflict(true);
        setRetryRemoval(null);
        setRemovalFocusRequest({
          userId: conflictFocusUserId(removal.nextFocusUserId, current.excludedUserIds),
        });
        return;
      }
      setRetryRemoval(removal);
      const saveIsCurrent = await presenceOverrideSyncService.save(removal.excludedUserIds);
      if (saveIsCurrent === false) return;
      const result = usePresenceOverrideStore.getState();
      if (result.saving) return;
      if (result.conflict) {
        setRetryRemoval(null);
        setRemovalFocusRequest({
          userId: conflictFocusUserId(removal.nextFocusUserId, result.excludedUserIds),
        });
        return;
      }
      if (result.error) return;
      const authoritativeIds = [...result.excludedUserIds].sort(comparePresenceOverrideUserIds);
      if (
        authoritativeIds.length !== removal.excludedUserIds.length ||
        authoritativeIds.some((userId, index) => userId !== removal.excludedUserIds[index])
      ) {
        result.setConflict(true);
        setRetryRemoval(null);
        setRemovalFocusRequest({
          userId: conflictFocusUserId(removal.nextFocusUserId, authoritativeIds),
        });
        return;
      }
      setRetryRemoval(null);
      setAnnouncement(`Removed exception for ${removal.label}.`);
      setRemovalFocusRequest({ userId: removal.nextFocusUserId });
    } finally {
      removalInFlightRef.current = false;
      setRemovalLocked(false);
    }
  }, []);

  const startRemovalSave = useCallback(
    (removal: RetryRemoval) => {
      saveRemoval(removal).catch(() => {
        usePresenceOverrideStore.getState().setError(SAVE_ERROR);
      });
    },
    [saveRemoval]
  );

  const refreshFriendRoster = useCallback(() => {
    if (!friendsLoading) {
      fetchFriends();
    }
  }, [fetchFriends, friendsLoading]);

  const removeUser = useCallback(
    (userId: string, label: string) => {
      if (!friendRosterReady) return;
      const removedIndex = excludedUserIds.indexOf(userId);
      const reduced = excludedUserIds
        .filter((excludedId) => excludedId !== userId)
        .sort(comparePresenceOverrideUserIds);
      const nextFocusUserId =
        excludedUserIds[removedIndex + 1] ?? excludedUserIds[removedIndex - 1] ?? null;
      startRemovalSave({
        excludedUserIds: reduced,
        label,
        baseAppliedVersion: appliedVersion,
        nextFocusUserId,
      });
    },
    [appliedVersion, excludedUserIds, friendRosterReady, startRemovalSave]
  );

  const openEditor = useCallback(() => {
    if (loading || busy || !friendRosterReady || categoryManagerOpen || removalInFlightRef.current)
      return;
    setAnnouncement('');
    if (retryRemoval) {
      usePresenceOverrideStore.getState().setError(null);
    }
    setRetryRemoval(null);
    setEditorOpen(true);
  }, [busy, categoryManagerOpen, friendRosterReady, loading, retryRemoval]);

  const handOffToCategoryManager = useCallback(() => {
    setEditorOpen(false);
    queueMicrotask(onOpenCategoryManager);
  }, [onOpenCategoryManager]);

  const countLabel = `${excludedUserIds.length === 1 ? 'person' : 'people'}`;

  let exceptionStateContent: React.ReactNode;
  if (loading) {
    exceptionStateContent = <p className="presence-exceptions__state">Loading exceptions...</p>;
  } else if (excludedUserIds.length === 0) {
    exceptionStateContent = <p className="presence-exceptions__state">No exceptions yet.</p>;
  } else {
    exceptionStateContent = (
      <ul className="presence-exceptions__list">
        {excludedUserIds.map((userId) => {
          const friend = friendsByUserId.get(userId);
          let label: string;
          let accessibleLabel: string;
          if (friend) {
            label = displayName(friend);
            accessibleLabel = label;
          } else if (friendRosterReady) {
            label = 'Unavailable person';
            accessibleLabel = 'unavailable person';
          } else if (friendsError) {
            label = 'Person details unavailable';
            accessibleLabel = 'person while details are unavailable';
          } else {
            label = 'Loading person details...';
            accessibleLabel = 'person while details load';
          }
          return (
            <li key={userId}>
              <span>
                <strong>{label}</strong>
                {friend && <small>@{friend.username}</small>}
              </span>
              <button
                ref={(button) => {
                  if (button) removeButtonMapRef.current.set(userId, button);
                  else removeButtonMapRef.current.delete(userId);
                }}
                type="button"
                className="presence-exception-remove"
                aria-label={`Remove ${accessibleLabel}`}
                disabled={busy || !friendRosterReady}
                onClick={() => removeUser(userId, label)}
              >
                Remove
              </button>
            </li>
          );
        })}
      </ul>
    );
  }

  return (
    <div className="presence-exceptions">
      <details className="presence-exceptions__details">
        <summary>
          Exceptions - {excludedUserIds.length} {countLabel}
        </summary>
        <div className="presence-exceptions__content">
          <p className="presence-exceptions__hint">
            These friends will not receive your Custom Status, even when the visibility tier would
            otherwise include them.
          </p>

          {exceptionStateContent}

          {!editorOpen && conflict && (
            <div role="alert" className="presence-exception-alert">
              Exceptions changed on another device. Review the current list and try again.
            </div>
          )}
          {!editorOpen && error && !conflict && (
            <div role="alert" className="presence-exception-alert">
              <span>{error}</span>
              {retryRemoval && (
                <button
                  type="button"
                  className="presence-exception-button presence-exception-button--secondary"
                  disabled={busy}
                  onClick={() => startRemovalSave(retryRemoval)}
                >
                  Retry removal
                </button>
              )}
            </div>
          )}
          {friendsError && (
            <div role="alert" className="presence-exception-alert">
              <span>{friendRosterErrorMessage}</span>
              <button
                type="button"
                className="presence-exception-button presence-exception-button--secondary"
                disabled={friendsLoading}
                onClick={refreshFriendRoster}
              >
                Retry friend list
              </button>
            </div>
          )}
          <div className="presence-exceptions__actions">
            <button
              ref={addButtonRef}
              type="button"
              className="presence-exception-button presence-exception-button--secondary"
              disabled={loading || busy || !friendRosterReady || categoryManagerOpen}
              onClick={openEditor}
            >
              Add exceptions
            </button>
          </div>
        </div>
      </details>

      <div role="status" aria-live="polite" className="presence-exceptions__live">
        {announcement}
      </div>

      {editorOpen && !categoryManagerOpen && (
        <PresenceExceptionModal
          returnFocusRef={addButtonRef}
          onDismiss={() => setEditorOpen(false)}
          onSaved={setAnnouncement}
          onOpenCategoryManager={handOffToCategoryManager}
        />
      )}
    </div>
  );
};

export default PresenceExceptions;
