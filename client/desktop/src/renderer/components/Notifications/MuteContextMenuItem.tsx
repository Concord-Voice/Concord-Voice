import React, { useState, useCallback } from 'react';
import ContextMenu from '../ui/ContextMenu';
import {
  useNotificationPrefsStore,
  isEntryCurrentlyMuted,
} from '../../stores/notificationPrefsStore';
import {
  setMutePreference,
  mutedUntilFromDuration,
  MUTE_DURATION_LABELS,
  type MuteDuration,
  type MuteTargetType,
} from '../../services/notificationPrefsService';
import { errorMessage } from '../../utils/redactError';

/* ------------------------------------------------------------------ */
/* MuteContextMenuItem                                                */
/*                                                                    */
/* A self-contained, reusable context-menu item that handles the      */
/* full mute/unmute lifecycle for any target type (server, channel,   */
/* DM conversation). Drop it into a ContextMenu and it:               */
/*                                                                    */
/*   - Reads current mute state from notificationPrefsStore           */
/*   - Renders either "Mute …" or "Unmute …" based on that state      */
/*   - Mute click opens a duration submenu (15m / 1h / 8h / 24h /     */
/*     indefinite); picking one fires the optimistic update + PUT     */
/*   - Unmute click is one-shot — no submenu, just immediate clear    */
/*                                                                    */
/* Keeping this logic in one place means the three call sites         */
/* (server / channel / DM context menus) stay simple and the duration */
/* set / labels / behavior can never drift between them.              */
/* ------------------------------------------------------------------ */

const SUBMENU_CLOSE_ANIM_MS = 150;

const ORDERED_DURATIONS: MuteDuration[] = ['15m', '1h', '8h', '24h', 'indefinite'];

interface MuteContextMenuItemProps {
  targetType: MuteTargetType;
  targetId: string;
  /**
   * Human-readable kind for the label, e.g. "Server", "Channel", "Conversation".
   * Used to produce "Mute Server" / "Unmute Conversation".
   */
  kindLabel: string;
  /** Called after a successful mute/unmute so the parent menu can close. */
  onAction: () => void;
}

const MuteContextMenuItem: React.FC<MuteContextMenuItemProps> = ({
  targetType,
  targetId,
  kindLabel,
  onAction,
}) => {
  // Subscribe to the relevant map so the trigger label updates instantly
  // when a mute toggles. We pick the map by targetType so DM-menu items
  // don't re-render when a server mute changes, and vice versa.
  const mapForType = useNotificationPrefsStore((s) => {
    if (targetType === 'server') return s.mutedServers;
    if (targetType === 'channel') return s.mutedChannels;
    return s.mutedDMs;
  });
  const entry = mapForType.get(targetId);
  const isMuted = isEntryCurrentlyMuted(entry);

  const [showPicker, setShowPicker] = useState(false);
  const [pickerClosing, setPickerClosing] = useState(false);

  // Synchronous click handlers: the underlying setMutePreference returns a
  // Promise, but the optimistic-update path means the store is already
  // mutated locally before the network call lands. A failed network call
  // is non-fatal (the local pref still reflects user intent) and we log it
  // via the catch chain rather than awaiting it — which lets these stay
  // pure (e: MouseEvent) => void handlers, avoiding the
  // no-misused-promises / @typescript-eslint conflict that would otherwise
  // force a `void promise()` discard.
  const handleMuteTriggerClick = useCallback(() => {
    if (isMuted) {
      // Already muted → no submenu, just unmute outright. Calling setMute
      // with muted=false (rather than removeMute) keeps the row around as
      // an explicit "I do not want a mute here" — which is what defeats a
      // parent server's mute in the channel-override case.
      setMutePreference(targetType, targetId, false, null).catch((err: unknown) => {
        console.warn('Failed to unmute target:', targetType, errorMessage(err));
      });
      onAction();
      return;
    }
    // Toggle the duration picker (animate the close if it was open).
    if (showPicker) {
      setPickerClosing(true);
      setTimeout(() => {
        setShowPicker(false);
        setPickerClosing(false);
      }, SUBMENU_CLOSE_ANIM_MS);
    } else {
      setShowPicker(true);
    }
  }, [isMuted, showPicker, targetType, targetId, onAction]);

  const handleDurationPick = useCallback(
    (duration: MuteDuration) => {
      setMutePreference(targetType, targetId, true, mutedUntilFromDuration(duration)).catch(
        (err: unknown) => {
          console.warn('Failed to mute target:', targetType, errorMessage(err));
        }
      );
      onAction();
    },
    [targetType, targetId, onAction]
  );

  const muteIcon = (
    // Bell-with-slash for "Mute"; plain bell for "Unmute".
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
      <path
        d="M4 6a4 4 0 018 0v3l1 1.5h-10l1-1.5V6z"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
      />
      <path
        d="M6.5 13a1.5 1.5 0 003 0"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
      />
      {!isMuted && (
        // Diagonal slash drawn only when the action would mute (i.e. the
        // user is about to silence things). Reading from the icon: "this
        // click will turn notifications off."
        <path d="M2 2l12 12" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
      )}
    </svg>
  );

  return (
    <div className="ctx-menu-item-wrapper">
      <ContextMenu.Item
        icon={muteIcon}
        label={`${isMuted ? 'Unmute' : 'Mute'} ${kindLabel}`}
        hasSubMenu={!isMuted}
        onClick={handleMuteTriggerClick}
      />

      {showPicker && !isMuted && (
        <ContextMenu.SubMenu closing={pickerClosing}>
          {ORDERED_DURATIONS.map((d) => (
            <ContextMenu.Item
              key={d}
              icon={<span style={{ width: 16 }} />}
              label={MUTE_DURATION_LABELS[d]}
              onClick={() => handleDurationPick(d)}
            />
          ))}
        </ContextMenu.SubMenu>
      )}
    </div>
  );
};

export default MuteContextMenuItem;
