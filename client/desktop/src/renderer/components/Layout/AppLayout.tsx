import React, { useCallback, useMemo, useRef } from 'react';
import { useLayoutStore } from '../../stores/layoutStore';
import { createResizeKeyHandler } from '../../utils/resizeKeyboard';
import './AppLayout.css';

interface AppLayoutProps {
  serverBar: React.ReactNode;
  folderBar: React.ReactNode;
  channelPanel: React.ReactNode;
  chatArea: React.ReactNode;
  memberSpace: React.ReactNode;
  /** Force the channel panel column to be visible regardless of pin state */
  forceChannelPin?: boolean;
  /** Force the member panel column to expanded mode regardless of layoutStore state */
  forceMemberExpanded?: boolean;
}

const AppLayout: React.FC<AppLayoutProps> = ({
  serverBar,
  folderBar,
  channelPanel,
  chatArea,
  memberSpace,
  forceChannelPin = false,
  forceMemberExpanded = false,
}) => {
  const channelPanelPinned = useLayoutStore((s) => s.channelPanelPinned);
  const memberPanelMode = useLayoutStore((s) => s.memberPanelMode);
  const setServerBarHeight = useLayoutStore((s) => s.setServerBarHeight);
  const setFolderBarHeight = useLayoutStore((s) => s.setFolderBarHeight);
  const serverBarHeight = useLayoutStore((s) => s.serverBarHeight);
  const folderBarHeight = useLayoutStore((s) => s.folderBarHeight);
  const interfaceLocked = useLayoutStore((s) => s.interfaceLocked);

  const startYRef = useRef(0);
  const startHeightRef = useRef(0);

  const makeResizeHandler = useCallback(
    (setter: (h: number) => void) => (e: React.MouseEvent) => {
      e.preventDefault();
      startYRef.current = e.clientY;
      const parentEl = (e.target as HTMLElement).previousElementSibling;
      startHeightRef.current = parentEl ? parentEl.getBoundingClientRect().height : 48;

      const onMove = (ev: MouseEvent) => {
        const delta = ev.clientY - startYRef.current;
        setter(startHeightRef.current + delta);
      };
      const onUp = () => {
        document.removeEventListener('mousemove', onMove);
        document.removeEventListener('mouseup', onUp);
        document.body.style.cursor = '';
        document.body.style.userSelect = '';
      };
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
      document.body.style.cursor = 'row-resize';
      document.body.style.userSelect = 'none';
    },
    []
  );

  const handleServerBarResizeKeyDown = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'vertical',
        direction: 'grow',
        min: 36,
        max: 64,
        getValue: () => serverBarHeight,
        setValue: setServerBarHeight,
      }),
    [serverBarHeight, setServerBarHeight]
  );

  const handleFolderBarResizeKeyDown = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'vertical',
        direction: 'grow',
        min: 24,
        max: 48,
        getValue: () => folderBarHeight,
        setValue: setFolderBarHeight,
      }),
    [folderBarHeight, setFolderBarHeight]
  );

  return (
    <div
      className="app-layout"
      data-channel-pinned={String(channelPanelPinned || forceChannelPin)}
      data-member-mode={forceMemberExpanded ? 'expanded' : memberPanelMode}
    >
      <div
        className="layout-server-bar"
        data-context-area="servers"
        style={{ height: serverBarHeight }}
      >
        {serverBar}
      </div>
      {/* Resize handles are removed when the interface is locked (#188),
          freezing the current bar heights. The `serverbar-h` grid row is `auto`,
          so it collapses to 0 when the handle is absent. */}
      {!interfaceLocked && (
        <button
          type="button"
          className="layout-server-bar-resize layout-resize-handle-h"
          onMouseDown={makeResizeHandler(setServerBarHeight)}
          onKeyDown={handleServerBarResizeKeyDown}
          tabIndex={0}
          aria-label="Resize server bar"
        />
      )}
      <div className="layout-folder-bar" style={{ height: folderBarHeight }}>
        {folderBar}
      </div>
      {!interfaceLocked && (
        <button
          type="button"
          className="layout-folder-bar-resize layout-resize-handle-h"
          onMouseDown={makeResizeHandler(setFolderBarHeight)}
          onKeyDown={handleFolderBarResizeKeyDown}
          tabIndex={0}
          aria-label="Resize folder bar"
        />
      )}
      <div className="layout-channel-panel" data-context-area="channels">
        {channelPanel}
      </div>
      <div className="layout-chat-area" data-context-area="chat">
        {chatArea}
      </div>
      <div className="layout-member-space" data-context-area="members">
        {memberSpace}
      </div>
    </div>
  );
};

export default AppLayout;
