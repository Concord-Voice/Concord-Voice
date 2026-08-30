import React, { useCallback, useEffect, useMemo, useRef, useState, type RefObject } from 'react';
import { presenceOverrideSyncService } from '../../services/system/presenceOverrideSync';
import { useFriendOrgStore, type FriendCategory } from '../../stores/chat/friendOrgStore';
import { useFriendStore, type Friend } from '../../stores/chat/friendStore';
import { usePresenceOverrideStore } from '../../stores/ui/presenceOverrideStore';
import { comparePresenceOverrideUserIds } from '../../utils/presenceOverrides';
import './PresenceExceptions.css';

const EXPLANATION =
  'Category selections add current members. Later category changes do not update saved exceptions automatically.';

const dialogCancelsOnEscape = (() => {
  if (typeof document === 'undefined') return true;
  return globalThis.navigator !== undefined && !/jsdom/i.test(globalThis.navigator.userAgent ?? '');
})();

export interface PresenceExceptionModalProps {
  returnFocusRef: RefObject<HTMLButtonElement | null>;
  onDismiss: () => void;
  onSaved: (message: string) => void;
  onOpenCategoryManager: () => void;
}

function friendLabel(friend: Friend): string {
  return friend.displayName?.trim() || friend.username;
}

function matchesFriend(friend: Friend, query: string): boolean {
  return (
    friendLabel(friend).toLocaleLowerCase().includes(query) ||
    friend.username.toLocaleLowerCase().includes(query)
  );
}

function currentCategoryMemberIds(
  category: FriendCategory,
  knownFriendIds: ReadonlySet<string>
): string[] {
  return category.memberIds.filter((userId) => knownFriendIds.has(userId));
}

function categoryChecked(memberIds: readonly string[], draft: ReadonlySet<string>): boolean {
  return memberIds.length > 0 && memberIds.every((id) => draft.has(id));
}

function categoryPartiallyChecked(
  memberIds: readonly string[],
  draft: ReadonlySet<string>
): boolean {
  const selectedCount = memberIds.filter((id) => draft.has(id)).length;
  return selectedCount > 0 && selectedCount < memberIds.length;
}

const PresenceExceptionModal: React.FC<PresenceExceptionModalProps> = ({
  returnFocusRef,
  onDismiss,
  onSaved,
  onOpenCategoryManager,
}) => {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const saveInFlightRef = useRef(false);
  const friends = useFriendStore((state) => state.friends);
  const categories = useFriendOrgStore((state) => state.categories);
  const saving = usePresenceOverrideStore((state) => state.saving);
  const [submitLocked, setSubmitLocked] = useState(false);
  const [intentBase] = useState(() => {
    const state = usePresenceOverrideStore.getState();
    return {
      excludedUserIds: [...state.excludedUserIds],
      appliedVersion: state.appliedVersion,
    };
  });
  const [query, setQuery] = useState('');
  const [draft, setDraft] = useState<Set<string>>(() => new Set(intentBase.excludedUserIds));
  const [localError, setLocalError] = useState<string | null>(null);
  const busy = saving || submitLocked;

  const normalizedQuery = query.trim().toLocaleLowerCase();
  const filteredCategories = useMemo(
    () =>
      categories.filter((category) => category.name.toLocaleLowerCase().includes(normalizedQuery)),
    [categories, normalizedQuery]
  );
  const filteredFriends = useMemo(
    () => friends.filter((friend) => matchesFriend(friend, normalizedQuery)),
    [friends, normalizedQuery]
  );
  const knownFriendIds = useMemo(() => new Set(friends.map((friend) => friend.userId)), [friends]);
  const unavailableCount = [...draft].filter((id) => !knownFriendIds.has(id)).length;

  const restoreFocus = useCallback(() => {
    queueMicrotask(() => returnFocusRef.current?.focus());
  }, [returnFocusRef]);

  const dismiss = useCallback(() => {
    if (busy || saveInFlightRef.current) return;
    onDismiss();
    restoreFocus();
  }, [busy, onDismiss, restoreFocus]);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog || dialog.open) return;
    try {
      dialog.showModal();
    } catch {
      dialog.setAttribute('open', '');
    }
    queueMicrotask(() => searchRef.current?.focus());
  }, []);

  useEffect(() => {
    if (dialogCancelsOnEscape) return;
    const handleEscape = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      event.stopPropagation();
      event.stopImmediatePropagation();
      dismiss();
    };
    document.addEventListener('keydown', handleEscape, true);
    return () => document.removeEventListener('keydown', handleEscape, true);
  }, [dismiss]);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    // Native dialog backdrops are not DOM nodes. Their pointer event targets
    // the dialog itself, while Escape reaches the separate cancel handler.
    const handleBackdropClick = (event: MouseEvent): void => {
      if (event.target === dialog) dismiss();
    };
    dialog.addEventListener('click', handleBackdropClick);
    return () => dialog.removeEventListener('click', handleBackdropClick);
  }, [dismiss]);

  const updateFriend = useCallback((userId: string, selected: boolean) => {
    if (saveInFlightRef.current) return;
    setDraft((current) => {
      const next = new Set(current);
      if (selected) next.add(userId);
      else next.delete(userId);
      return next;
    });
  }, []);

  const updateCategory = useCallback((memberIds: readonly string[], selected: boolean) => {
    if (saveInFlightRef.current) return;
    const memberSnapshot = [...memberIds];
    setDraft((current) => {
      const next = new Set(current);
      for (const userId of memberSnapshot) {
        if (selected) next.add(userId);
        else next.delete(userId);
      }
      return next;
    });
  }, []);

  const openCategoryManager = useCallback(() => {
    if (busy || saveInFlightRef.current) return;
    onOpenCategoryManager();
  }, [busy, onOpenCategoryManager]);

  const handleSave = useCallback(async () => {
    if (busy || saveInFlightRef.current) return;
    saveInFlightRef.current = true;
    setSubmitLocked(true);
    try {
      setLocalError(null);
      const current = usePresenceOverrideStore.getState();
      if (current.appliedVersion !== intentBase.appliedVersion) {
        current.setConflict(true);
        onDismiss();
        restoreFocus();
        return;
      }
      const canonicalDraft = [...draft].sort(comparePresenceOverrideUserIds);
      const saveIsCurrent = await presenceOverrideSyncService.save(canonicalDraft);
      if (saveIsCurrent === false) return;
      const result = usePresenceOverrideStore.getState();
      if (result.saving) return;
      if (result.conflict) {
        onDismiss();
        restoreFocus();
        return;
      }
      if (result.error) {
        setLocalError('Failed to save presence exceptions. Review your selections and try again.');
        return;
      }
      const authoritativeIds = [...result.excludedUserIds].sort(comparePresenceOverrideUserIds);
      if (
        authoritativeIds.length !== canonicalDraft.length ||
        authoritativeIds.some((userId, index) => userId !== canonicalDraft[index])
      ) {
        result.setConflict(true);
        onDismiss();
        restoreFocus();
        return;
      }
      onSaved('Saved Custom Status exceptions.');
      onDismiss();
      restoreFocus();
    } finally {
      saveInFlightRef.current = false;
      setSubmitLocked(false);
    }
  }, [busy, draft, intentBase.appliedVersion, onDismiss, onSaved, restoreFocus]);

  const handleCancel = useCallback(
    (event: React.SyntheticEvent<HTMLDialogElement, Event>) => {
      event.preventDefault();
      event.stopPropagation();
      dismiss();
    },
    [dismiss]
  );

  let categoryContent: React.ReactNode;
  if (categories.length === 0) {
    categoryContent = (
      <div className="presence-exception-empty">
        <p>You have no friend categories yet.</p>
        <button
          type="button"
          className="presence-exception-button presence-exception-button--secondary"
          disabled={busy}
          onClick={openCategoryManager}
        >
          Manage categories
        </button>
      </div>
    );
  } else if (filteredCategories.length === 0) {
    categoryContent = <p className="presence-exception-empty">No matching categories.</p>;
  } else {
    categoryContent = (
      <div className="presence-exception-options">
        {filteredCategories.map((category) => {
          const memberIds = currentCategoryMemberIds(category, knownFriendIds);
          const checked = categoryChecked(memberIds, draft);
          const partiallyChecked = categoryPartiallyChecked(memberIds, draft);
          const memberLabel = `${memberIds.length} ${memberIds.length === 1 ? 'current member' : 'current members'}`;
          return (
            <label
              key={category.id}
              className="presence-exception-option"
              aria-label={`${category.name} ${memberLabel}`}
            >
              <input
                ref={(input) => {
                  if (input) input.indeterminate = partiallyChecked;
                }}
                type="checkbox"
                checked={checked}
                disabled={busy || memberIds.length === 0}
                onChange={(event) => updateCategory(memberIds, event.target.checked)}
              />
              <span>
                <strong>{category.name}</strong> <small>{memberLabel}</small>
              </span>
            </label>
          );
        })}
      </div>
    );
  }

  return (
    <dialog
      ref={dialogRef}
      className="presence-exception-dialog"
      aria-modal="true"
      aria-labelledby="presence-exception-title"
      aria-describedby="presence-exception-description"
      onCancel={handleCancel}
    >
      <div className="presence-exception-dialog__panel">
        <header className="presence-exception-dialog__header">
          <div>
            <h2 id="presence-exception-title">Custom Status exceptions</h2>
            <p id="presence-exception-description">
              Choose friends who should not receive your Custom Status.
            </p>
          </div>
        </header>

        <div className="presence-exception-dialog__body">
          <label className="presence-exception-search">
            <span>Search friends and categories</span>
            <input
              ref={searchRef}
              type="search"
              value={query}
              onChange={(event) => {
                if (!saveInFlightRef.current) setQuery(event.target.value);
              }}
              disabled={busy}
            />
          </label>

          <fieldset className="presence-exception-group">
            <legend>Categories</legend>
            {categoryContent}
          </fieldset>

          <p className="presence-exception-explanation">{EXPLANATION}</p>

          <fieldset className="presence-exception-group">
            <legend>Friends</legend>
            {filteredFriends.length === 0 ? (
              <p className="presence-exception-empty">No matching friends.</p>
            ) : (
              <div className="presence-exception-options presence-exception-options--friends">
                {filteredFriends.map((friend) => (
                  <label
                    key={friend.userId}
                    className="presence-exception-option"
                    aria-label={`${friendLabel(friend)} @${friend.username}`}
                  >
                    <input
                      type="checkbox"
                      checked={draft.has(friend.userId)}
                      disabled={busy}
                      onChange={(event) => updateFriend(friend.userId, event.target.checked)}
                    />
                    <span>
                      <strong>{friendLabel(friend)}</strong> <small>@{friend.username}</small>
                    </span>
                  </label>
                ))}
              </div>
            )}
          </fieldset>

          {unavailableCount > 0 && (
            <p className="presence-exception-unavailable">
              {unavailableCount}{' '}
              {unavailableCount === 1
                ? 'unavailable person remains selected.'
                : 'unavailable people remain selected.'}
            </p>
          )}
          {localError && (
            <div role="alert" className="presence-exception-alert">
              {localError}
            </div>
          )}
        </div>

        <footer className="presence-exception-dialog__actions">
          <span aria-live="polite">
            {draft.size} {draft.size === 1 ? 'person selected' : 'people selected'}
          </span>
          <div>
            <button
              type="button"
              className="presence-exception-button presence-exception-button--secondary"
              disabled={busy}
              onClick={dismiss}
            >
              Cancel
            </button>
            <button
              type="button"
              className="presence-exception-button presence-exception-button--primary"
              aria-label="Save exceptions"
              disabled={busy}
              onClick={handleSave}
            >
              {busy ? 'Saving...' : 'Save exceptions'}
            </button>
          </div>
        </footer>
      </div>
    </dialog>
  );
};

export default PresenceExceptionModal;
