import React from 'react';
import { createPortal } from 'react-dom';
import { ServerWithRole } from '../../types/server';

export interface ServerHoverTooltipProps {
  server: ServerWithRole;
  /** Bounding rect of the hovered element (from getBoundingClientRect). */
  rect: DOMRect;
  voiceCount: number;
  showUnread: boolean;
  /** 'below' — main server-bar rail icons; 'right' — folder dropdown items. */
  placement: 'below' | 'right';
}

/**
 * Fixed-position server stats tooltip, portaled to document.body so it
 * escapes overflow:hidden containers. Shared by ServerBar (rail icons) and
 * FolderBar (folder dropdown items) — styles live in ServerBar.css under the
 * existing server-bar-tooltip-* classes.
 */
const ServerHoverTooltip: React.FC<ServerHoverTooltipProps> = ({
  server,
  rect,
  voiceCount,
  showUnread,
  placement,
}) => {
  const style: React.CSSProperties =
    placement === 'below'
      ? {
          position: 'fixed',
          top: rect.bottom + 8,
          left: rect.left + rect.width / 2,
          transform: 'translateX(-50%)',
        }
      : {
          position: 'fixed',
          top: rect.top + rect.height / 2,
          left: rect.right + 8,
          transform: 'translateY(-50%)',
        };

  return createPortal(
    <div className="server-bar-tooltip-fixed" style={style}>
      <span className="server-bar-tooltip-name">{server.name}</span>
      <div className="server-bar-tooltip-stats">
        <span>{server.member_count ?? 0} Members</span>
        <span className="server-bar-tooltip-dot" />
        <span>{server.online_count ?? 0} Online</span>
      </div>
      <div className="server-bar-tooltip-stats">
        <span
          className={`server-bar-tooltip-voice${voiceCount > 0 ? ' server-bar-tooltip-voice--active' : ''}`}
        >
          {voiceCount} In Voice
        </span>
      </div>
      {showUnread && <span className="server-bar-tooltip-unread">Unread notifications</span>}
    </div>,
    document.body
  );
};

export default ServerHoverTooltip;
