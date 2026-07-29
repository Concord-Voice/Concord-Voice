/**
 * ConnectionStatus - Visual indicator of WebSocket connection state
 */

import React from 'react';
import { useChatStore } from '../../stores/chatStore';
import './ConnectionStatus.css';

interface ConnectionStatusProps {
  compact?: boolean;
}

const ConnectionStatus: React.FC<ConnectionStatusProps> = ({ compact = false }) => {
  const connectionState = useChatStore((s) => s.connectionState);
  const connectionClientId = useChatStore((s) => s.connectionClientId);

  if (connectionState === 'connected') {
    return (
      <div
        className={`connection-status connected${compact ? ' connection-status--compact' : ''}`}
        title={`Connected (${connectionClientId})`}
      >
        <span className="status-dot"></span>
        {!compact && <span className="status-text">Connected</span>}
      </div>
    );
  }

  if (connectionState === 'connecting') {
    return (
      <div
        className={`connection-status connecting${compact ? ' connection-status--compact' : ''}`}
        title="Connecting to server..."
      >
        <span className="status-dot"></span>
        {!compact && <span className="status-text">Connecting</span>}
      </div>
    );
  }

  return (
    <div
      className={`connection-status disconnected${compact ? ' connection-status--compact' : ''}`}
      title="Disconnected from server"
    >
      <span className="status-dot"></span>
      {!compact && <span className="status-text">Offline</span>}
    </div>
  );
};

export default ConnectionStatus;
