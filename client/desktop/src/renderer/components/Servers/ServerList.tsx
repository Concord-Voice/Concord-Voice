import React, { useEffect } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { useNavigate, useLocation } from 'react-router-dom';
import { useAuthStore } from '../../stores/authStore';
import { useServerStore } from '../../stores/serverStore';
import { useUnreadStore } from '../../stores/unreadStore';

import { ServerWithRole } from '../../types/server';
import './ServerList.css';

interface ServerListProps {
  onOpenActionModal: () => void;
  onContextMenu: (server: ServerWithRole, position: { x: number; y: number }) => void;
}

const ServerList: React.FC<ServerListProps> = ({ onOpenActionModal, onContextMenu }) => {
  const accessToken = useAuthStore((state) => state.accessToken);
  const servers = useServerStore((state) => state.servers);
  const activeServerId = useServerStore((state) => state.activeServerId);
  const isLoading = useServerStore((state) => state.isLoading);
  const error = useServerStore((state) => state.error);
  const fetchServers = useServerStore((state) => state.fetchServers);
  const setActiveServer = useServerStore((state) => state.setActiveServer);
  const navigate = useNavigate();
  const location = useLocation();
  const serverUnreadSet = useUnreadStore((state) => state.serverUnreadSet);

  useEffect(() => {
    if (accessToken) {
      fetchServers();
    }
  }, [accessToken, fetchServers]);

  const handleContextMenu = (e: React.MouseEvent, server: ServerWithRole) => {
    e.preventDefault();
    e.stopPropagation();
    onContextMenu(server, { x: e.clientX, y: e.clientY });
  };

  return (
    <div className="server-list">
      {/* Loading skeletons */}
      {isLoading && servers.length === 0 && (
        <div className="server-list-skeletons">
          <div className="server-skeleton" />
          <div className="server-skeleton" />
          <div className="server-skeleton" />
        </div>
      )}

      {/* Error state */}
      {error && servers.length === 0 && !isLoading && (
        <button
          className="server-error-btn"
          onClick={() => fetchServers()}
          title={`Failed to load servers: ${error}. Click to retry.`}
          aria-label="Retry loading servers"
        >
          <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
            <path
              d="M10 2L10 6M10 14L10 18M2 10L6 10M14 10L18 10"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
            />
            <circle cx="10" cy="10" r="7" stroke="currentColor" strokeWidth="1.5" />
            <path
              d="M10 7v4M10 13v.5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
          </svg>
        </button>
      )}

      {/* Server icons */}
      {servers.map((server) => {
        const isActive = activeServerId === server.id;
        const hasUnread = !isActive && serverUnreadSet.has(server.id);
        return (
          <div key={server.id} className="server-icon-wrapper">
            {isActive && <div className="server-active-pill" />}
            {hasUnread && <div className="server-unread-pill" />}
            <button
              className={`server-icon-btn ${isActive ? 'active' : ''} ${hasUnread ? 'has-unread' : ''}`}
              onClick={() => {
                setActiveServer(server.id);
                if (location.pathname !== '/app') {
                  navigate('/app');
                }
              }}
              onContextMenu={(e) => handleContextMenu(e, server)}
              title={`${server.name} (${server.role})`}
              aria-label={`${server.name} server`}
            >
              {resolveMediaUrl(server.icon_url) ? (
                <img src={resolveMediaUrl(server.icon_url)} alt={server.name} className="server-icon-img" />
              ) : (
                <span className="server-icon-initial">{server.name.charAt(0).toUpperCase()}</span>
              )}
            </button>
            <div className="server-tooltip">
              <span className="server-tooltip-name">{server.name}</span>
              <span className={`server-tooltip-role role-${server.role}`}>{server.role}</span>
            </div>
          </div>
        );
      })}

      {/* Separator */}
      {servers.length > 0 && <div className="server-list-separator" />}

      {/* Add server button */}
      <button
        className="add-server-btn"
        onClick={onOpenActionModal}
        title="Add a Server"
        aria-label="Add a Server"
      >
        <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
          <path d="M10 4v12M4 10h12" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
        </svg>
      </button>
    </div>
  );
};

export default ServerList;
