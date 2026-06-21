import React, { useRef, useCallback, useEffect, useState } from 'react';
import { Pin, ChevronRight } from 'lucide-react';
import { useLayoutStore } from '../../stores/layoutStore';
import { useSettingsStore } from '../../stores/settingsStore';
import { useResizablePanel } from '../../hooks/useResizablePanel';
import UserPanel from '../User/UserPanel';

interface ChannelPanelProps {
  header: React.ReactNode;
  children: React.ReactNode;
  /** When true, panel is forced into pinned mode and the unpin button is hidden. */
  forcePin?: boolean;
}

const ChannelPanel: React.FC<ChannelPanelProps> = ({ header, children, forcePin = false }) => {
  const channelPanelPinned = useLayoutStore((s) => s.channelPanelPinned);
  const channelPanelHoverVisible = useLayoutStore((s) => s.channelPanelHoverVisible);
  const toggleChannelPin = useLayoutStore((s) => s.toggleChannelPin);
  const showChannelPanelHover = useLayoutStore((s) => s.showChannelPanelHover);
  const hideChannelPanelHover = useLayoutStore((s) => s.hideChannelPanelHover);
  const interfaceLocked = useLayoutStore((s) => s.interfaceLocked);
  const reduceAnimations = useSettingsStore((s) => s.appearance.reduceAnimations);

  const panel = useResizablePanel({
    defaultWidth: 240,
    minWidth: 180,
    maxWidth: 400,
    side: 'left',
    storageKey: 'concord:channelPanelWidth',
  });

  const [isHovered, setIsHovered] = useState(false);
  const hideTimeoutRef = useRef<NodeJS.Timeout | null>(null);

  const clearHideTimeout = useCallback(() => {
    if (hideTimeoutRef.current) {
      clearTimeout(hideTimeoutRef.current);
      hideTimeoutRef.current = null;
    }
  }, []);

  const startHideTimeout = useCallback(() => {
    clearHideTimeout();
    hideTimeoutRef.current = setTimeout(
      () => {
        setIsHovered(false);
        hideChannelPanelHover();
      },
      reduceAnimations ? 200 : 1000
    );
  }, [clearHideTimeout, hideChannelPanelHover, reduceAnimations]);

  const handleMouseEnter = useCallback(() => {
    clearHideTimeout();
    setIsHovered(true);
  }, [clearHideTimeout]);

  const handleMouseLeave = useCallback(() => {
    if (!channelPanelPinned) {
      startHideTimeout();
    }
  }, [channelPanelPinned, startHideTimeout]);

  // Lip handle hover — reveal the panel by reusing the existing hover machinery.
  const handleLipMouseEnter = useCallback(() => {
    clearHideTimeout();
    setIsHovered(true);
    showChannelPanelHover();
  }, [clearHideTimeout, showChannelPanelHover]);

  // Cleanup timeout on unmount
  useEffect(() => {
    return () => clearHideTimeout();
  }, [clearHideTimeout]);

  const effectivelyPinned = channelPanelPinned || forcePin;

  // Pin/unpin toggle (#188). Hidden when the panel is force-pinned or the
  // interface is locked (the pin state is frozen then). The `.pinned` class
  // drives the accent-colour + 45° rotation that makes the lock-open state
  // unmistakable — all styling lives in CSS now (was inline).
  const pinButton =
    forcePin || interfaceLocked ? null : (
      <button
        type="button"
        className={`channel-panel-pin-btn ${effectivelyPinned ? 'pinned' : ''}`}
        onClick={toggleChannelPin}
        title={effectivelyPinned ? 'Unpin panel' : 'Pin panel open'}
        aria-label={effectivelyPinned ? 'Unpin panel' : 'Pin panel open'}
        aria-pressed={effectivelyPinned}
      >
        <Pin size={14} />
      </button>
    );

  // Pinned mode — standard sidebar
  if (effectivelyPinned) {
    return (
      <>
        <div
          className="channels-sidebar"
          style={{ width: panel.width, display: 'flex', flexDirection: 'column', height: '100%' }}
        >
          <div className="channels-sidebar-header">
            {header}
            {pinButton}
          </div>
          {children}
          <UserPanel />
        </div>
        {/* Resize handle — removed when the interface is locked (#188), freezing
            the current width. */}
        {!interfaceLocked && (
          <button
            type="button"
            className="layout-resize-handle"
            onMouseDown={panel.onMouseDown}
            onKeyDown={panel.onKeyDown}
            tabIndex={0}
            aria-label="Resize channel panel"
          />
        )}
      </>
    );
  }

  // Unpinned mode — overlay dropdown + discoverable edge lip
  const showOverlay = channelPanelHoverVisible || isHovered;

  return (
    <>
      {/* Edge "lip" handle (#188) — a visible affordance at the app edge where
          the panel collapsed to, replacing the previously invisible ServerBar
          hover zone as the discovery cue. Hover reveals (peeks) the panel.
          Click re-pins it — UNLESS the interface is locked, in which case the
          lip STAYS visible (a closed panel still needs a way to be peeked) but
          a click only reveals/peeks rather than re-pinning, since the pin state
          is frozen while locked. Hidden only while the panel is already
          showing. */}
      {!showOverlay && (
        <button
          type="button"
          className="channel-panel-lip"
          onMouseEnter={handleLipMouseEnter}
          onClick={interfaceLocked ? handleLipMouseEnter : toggleChannelPin}
          title="Reveal channel panel"
          aria-label="Reveal channel panel"
        >
          <ChevronRight size={14} />
        </button>
      )}
      <div
        className={`channel-panel-overlay ${showOverlay ? 'slide-in' : 'slide-out'}`}
        style={{ width: panel.width }}
        onMouseEnter={handleMouseEnter}
        onMouseLeave={handleMouseLeave}
      >
        <div className="channels-sidebar-header">
          {header}
          {pinButton}
        </div>
        {children}
      </div>
    </>
  );
};

export default ChannelPanel;
