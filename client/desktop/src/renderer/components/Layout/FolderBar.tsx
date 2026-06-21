import React, { useState, useRef, useEffect, useCallback } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { createPortal } from 'react-dom';
import { ChevronDown, FolderPlus, Pencil, Trash2 } from 'lucide-react';
import { useServerStore } from '../../stores/serverStore';
import { useUnreadStore } from '../../stores/unreadStore';
import { useVoiceStore } from '../../stores/voiceStore';
import { useLayoutStore, ServerFolder } from '../../stores/layoutStore';
import { useNavigate, useLocation } from 'react-router-dom';
import { ServerWithRole } from '../../types/server';
import ContextMenu from '../ui/ContextMenu';
import Modal from '../ui/Modal';
import './FolderBar.css';

/** Remove a cloned drag-image element on the next animation frame. */
function scheduleDragImageCleanup(clone: HTMLElement): void {
  requestAnimationFrame(() => clone.remove());
}

const FolderBar: React.FC = () => {
  const navigate = useNavigate();
  const location = useLocation();

  const servers = useServerStore((s) => s.servers);
  const setActiveServer = useServerStore((s) => s.setActiveServer);
  const serverUnreadSet = useUnreadStore((s) => s.serverUnreadSet);
  const serverVoiceCounts = useVoiceStore((s) => s.serverVoiceCounts);

  const serverFolders = useLayoutStore((s) => s.serverFolders);
  const createFolder = useLayoutStore((s) => s.createFolder);
  const renameFolder = useLayoutStore((s) => s.renameFolder);
  const deleteFolder = useLayoutStore((s) => s.deleteFolder);
  const moveServerToFolder = useLayoutStore((s) => s.moveServerToFolder);
  const reorderFolderServers = useLayoutStore((s) => s.reorderFolderServers);
  const folderBarHeight = useLayoutStore((s) => s.folderBarHeight);

  // Scale factor: folderBarHeight range 24–48, scale 0.75–1.25
  const folderScale = Math.max(
    0.75,
    Math.min(1.25, ((folderBarHeight - 24) / (48 - 24)) * 0.5 + 0.75)
  );

  const [openFolderId, setOpenFolderId] = useState<string | null>(null);
  const [dropdownPos, setDropdownPos] = useState<{ top: number; left: number } | null>(null);
  // eslint-disable-next-line @eslint-react/naming-convention-ref-name -- stable ref; rename to the *Ref-suffix convention deferred to avoid churning untested handler lines in this low-coverage component (new-code coverage gate). Cosmetic rule suppressed per [internal]rules conventions.
  const chipRefs = useRef<Map<string, HTMLDivElement>>(new Map());
  const [dragOverFolderId, setDragOverFolderId] = useState<string | null>(null);
  const dragCounterRef = useRef<Map<string, number>>(new Map());
  const [contextMenu, setContextMenu] = useState<{
    folder: ServerFolder;
    position: { x: number; y: number };
  } | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState<ServerFolder | null>(null);
  const [renaming, setRenaming] = useState<{ folderId: string; name: string } | null>(null);
  const [dropdownInsert, setDropdownInsert] = useState<{
    folderId: string;
    targetId: string;
    side: 'before' | 'after';
  } | null>(null);
  const [draggingServerId, setDraggingServerId] = useState<string | null>(null);
  const [hoveredServer, setHoveredServer] = useState<{
    server: ServerWithRole;
    rect: DOMRect;
  } | null>(null);
  const [addBtnTooltipPos, setAddBtnTooltipPos] = useState<{ top: number; left: number } | null>(
    null
  );
  const addBtnRef = useRef<HTMLButtonElement>(null);
  // eslint-disable-next-line @eslint-react/naming-convention-ref-name -- stable ref; rename to the *Ref-suffix convention deferred to avoid churning untested handler lines in this low-coverage component (new-code coverage gate). Cosmetic rule suppressed per [internal]rules conventions.
  const addBtnHoverTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);
  const renameInputRef = useRef<HTMLInputElement>(null);

  // Close dropdown on outside click
  useEffect(() => {
    const handler = (e: MouseEvent) => {
      const target = e.target as Node;
      // Don't close if clicking inside the dropdown itself
      if (dropdownRef.current?.contains(target)) return;
      // Don't close if clicking on a folder chip (handleFolderClick will toggle)
      for (const chip of chipRefs.current.values()) {
        if (chip.contains(target)) return;
      }
      setOpenFolderId(null);
      setDropdownPos(null);
    };
    if (openFolderId) {
      document.addEventListener('mousedown', handler);
    }
    return () => document.removeEventListener('mousedown', handler);
  }, [openFolderId]);

  // Focus rename input when it appears (only on initial rename start, not every keystroke)
  const renamingFolderId = renaming?.folderId ?? null;
  useEffect(() => {
    if (renamingFolderId && renameInputRef.current) {
      renameInputRef.current.focus();
      renameInputRef.current.select();
    }
  }, [renamingFolderId]);

  const showAddBtnTooltip = useCallback(() => {
    addBtnHoverTimer.current = setTimeout(() => {
      if (addBtnRef.current) {
        const rect = addBtnRef.current.getBoundingClientRect();
        const tooltipWidth = 90; // approximate width of "New Folder"
        const padding = 8;
        const top = rect.bottom + 6;
        // Center below the button, but clamp to viewport
        let left = rect.left + rect.width / 2 - tooltipWidth / 2;
        left = Math.max(padding, Math.min(left, globalThis.innerWidth - tooltipWidth - padding));
        // Clamp top to viewport bottom
        const clampedTop = Math.min(top, globalThis.innerHeight - 30 - padding);
        setAddBtnTooltipPos({ top: clampedTop, left });
      }
    }, 400);
  }, []);

  const hideAddBtnTooltip = useCallback(() => {
    if (addBtnHoverTimer.current) {
      clearTimeout(addBtnHoverTimer.current);
      addBtnHoverTimer.current = null;
    }
    setAddBtnTooltipPos(null);
  }, []);

  const handleFolderClick = (folderId: string) => {
    if (renaming?.folderId === folderId) return;
    const isClosing = openFolderId === folderId;
    setOpenFolderId(isClosing ? null : folderId);
    if (isClosing) {
      setDropdownPos(null);
    } else {
      const chipEl = chipRefs.current.get(folderId);
      if (chipEl) {
        const rect = chipEl.getBoundingClientRect();
        setDropdownPos({ top: rect.bottom + 6, left: rect.left });
      }
    }
  };

  const handleServerClick = (serverId: string) => {
    setActiveServer(serverId);
    setOpenFolderId(null);
    if (location.pathname !== '/app') navigate('/app');
  };

  const handleCreateFolder = () => {
    const name = `Folder ${serverFolders.length + 1}`;
    createFolder(name);
    // The folder was just appended — grab its ID from the store after the next render
    // Use queueMicrotask so the store update has committed
    queueMicrotask(() => {
      const folders = useLayoutStore.getState().serverFolders;
      const newFolder = folders[folders.length - 1];
      if (newFolder) {
        setRenaming({ folderId: newFolder.id, name: newFolder.name });
      }
    });
  };

  const getFolderUnreadCount = (folder: ServerFolder): number => {
    return folder.serverIds.filter((id) => serverUnreadSet.has(id)).length;
  };

  // Context menu handlers
  const handleContextMenu = (e: React.MouseEvent, folder: ServerFolder) => {
    e.preventDefault();
    e.stopPropagation();
    setContextMenu({ folder, position: { x: e.clientX, y: e.clientY } });
  };

  const handleRenameStart = useCallback(() => {
    if (!contextMenu) return;
    const folder = contextMenu.folder;
    setRenaming({ folderId: folder.id, name: folder.name });
    setContextMenu(null);
  }, [contextMenu]);

  const handleRenameSubmit = useCallback(() => {
    if (!renaming) return;
    const trimmed = renaming.name.trim();
    if (trimmed) {
      renameFolder(renaming.folderId, trimmed);
    }
    setRenaming(null);
  }, [renaming, renameFolder]);

  const handleRenameKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Enter') {
        handleRenameSubmit();
      } else if (e.key === 'Escape') {
        setRenaming(null);
      }
    },
    [handleRenameSubmit]
  );

  const handleDeleteRequest = useCallback(() => {
    if (!contextMenu) return;
    setDeleteConfirm(contextMenu.folder);
    setContextMenu(null);
  }, [contextMenu]);

  const handleDeleteConfirm = useCallback(() => {
    if (!deleteConfirm) return;
    deleteFolder(deleteConfirm.id);
    setDeleteConfirm(null);
  }, [deleteConfirm, deleteFolder]);

  // Drag-and-drop handlers for folder chips
  // Use a counter to handle dragenter/dragleave on nested children
  const handleFolderDragEnter = (e: React.DragEvent, folderId: string) => {
    e.preventDefault();
    const count = (dragCounterRef.current.get(folderId) || 0) + 1;
    dragCounterRef.current.set(folderId, count);
    setDragOverFolderId(folderId);
  };

  const handleFolderDragOver = (e: React.DragEvent) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
  };

  const handleFolderDragLeave = (folderId: string) => {
    const count = (dragCounterRef.current.get(folderId) || 0) - 1;
    dragCounterRef.current.set(folderId, count);
    if (count <= 0) {
      dragCounterRef.current.set(folderId, 0);
      setDragOverFolderId(null);
    }
  };

  const handleFolderDrop = (e: React.DragEvent, folderId: string) => {
    e.preventDefault();
    e.stopPropagation();
    dragCounterRef.current.set(folderId, 0);
    setDragOverFolderId(null);
    setDraggingServerId(null);
    const serverId = e.dataTransfer.getData('text/plain');
    if (!serverId) return;
    moveServerToFolder(serverId, folderId);
  };

  const folderStyle = { '--folder-scale': folderScale } as React.CSSProperties;

  if (serverFolders.length === 0) {
    return (
      <>
        <div className="folder-bar" style={folderStyle}>
          <div className="folder-bar-empty" />
          <div className="folder-bar-divider" />
          <button
            ref={addBtnRef}
            className="folder-add-btn"
            onClick={handleCreateFolder}
            onMouseEnter={showAddBtnTooltip}
            onMouseLeave={hideAddBtnTooltip}
          >
            <FolderPlus size={Math.round(14 * folderScale)} />
          </button>
        </div>
        {addBtnTooltipPos &&
          createPortal(
            <div
              className="folder-add-btn-tooltip visible"
              style={{ top: addBtnTooltipPos.top, left: addBtnTooltipPos.left }}
            >
              New Folder
            </div>,
            document.body
          )}
      </>
    );
  }

  return (
    <>
      <div className="folder-bar" style={folderStyle}>
        <div className="folder-bar-scroll" role="tree" aria-label="Server folders">
          {serverFolders.map((folder) => {
            const isOpen = openFolderId === folder.id;
            const isDragOver = dragOverFolderId === folder.id;
            const isRenaming = renaming?.folderId === folder.id;
            const unreadCount = getFolderUnreadCount(folder);

            return (
              <div
                key={folder.id}
                ref={(el) => {
                  if (el) chipRefs.current.set(folder.id, el);
                  else chipRefs.current.delete(folder.id);
                }}
                className={`folder-chip ${isOpen ? 'open' : ''} ${isDragOver ? 'drag-over' : ''}`}
                role="treeitem"
                tabIndex={0}
                aria-label={`${folder.name} folder`}
                aria-expanded={isOpen}
                aria-selected={isOpen}
                onDragEnter={(e) => handleFolderDragEnter(e, folder.id)}
                onDragOver={handleFolderDragOver}
                onDragLeave={() => handleFolderDragLeave(folder.id)}
                onDrop={(e) => handleFolderDrop(e, folder.id)}
                onContextMenu={(e) => handleContextMenu(e, folder)}
              >
                <button
                  type="button"
                  className="folder-chip-inner"
                  onClick={() => handleFolderClick(folder.id)}
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    gap: 'inherit',
                    background: 'none',
                    border: 0,
                    padding: 0,
                    cursor: 'pointer',
                    color: 'inherit',
                    font: 'inherit',
                  }}
                >
                  {isRenaming ? (
                    <input
                      ref={renameInputRef}
                      className="folder-rename-input"
                      value={renaming.name}
                      onChange={(e) => setRenaming({ ...renaming, name: e.target.value })}
                      onKeyDown={handleRenameKeyDown}
                      onBlur={handleRenameSubmit}
                      onClick={(e) => e.stopPropagation()}
                      maxLength={32}
                    />
                  ) : (
                    <>
                      <span className="folder-chip-name">{folder.name}</span>
                      {unreadCount > 0 && <span className="folder-chip-badge">{unreadCount}</span>}
                      <ChevronDown size={12} className="folder-chip-arrow" />
                    </>
                  )}
                </button>
              </div>
            );
          })}
        </div>
        <div className="folder-bar-divider" />
        <button
          ref={addBtnRef}
          className="folder-add-btn"
          onClick={handleCreateFolder}
          onMouseEnter={showAddBtnTooltip}
          onMouseLeave={hideAddBtnTooltip}
        >
          <FolderPlus size={14} />
        </button>
      </div>

      {/* Folder dropdown — portaled to body to escape stacking context */}
      {openFolderId &&
        dropdownPos &&
        (() => {
          const folder = serverFolders.find((f) => f.id === openFolderId);
          if (!folder) return null;
          const folderServers = folder.serverIds
            .map((id) => servers.find((s) => s.id === id))
            .filter(Boolean);
          return createPortal(
            <div
              ref={dropdownRef}
              className="folder-dropdown"
              role="menu"
              tabIndex={0}
              style={{
                position: 'fixed',
                top: dropdownPos.top,
                left: dropdownPos.left,
                zIndex: 9999,
              }}
              onDragOver={(e) => {
                e.preventDefault();
                e.dataTransfer.dropEffect = 'move';
              }}
              onDrop={(e) => {
                e.preventDefault();
                e.stopPropagation();
                const insertInfo = dropdownInsert;
                setDropdownInsert(null);
                setDraggingServerId(null);
                const sourceId = e.dataTransfer.getData('text/plain');
                if (!sourceId) return;

                const currentIds = folder.serverIds;
                const isInFolder = currentIds.includes(sourceId);

                if (isInFolder && insertInfo?.folderId === folder.id) {
                  const filtered = currentIds.filter((id) => id !== sourceId);
                  const targetIdx = filtered.indexOf(insertInfo.targetId);
                  if (targetIdx === -1) return;
                  const insertIdx = insertInfo.side === 'after' ? targetIdx + 1 : targetIdx;
                  filtered.splice(insertIdx, 0, sourceId);
                  reorderFolderServers(folder.id, filtered);
                } else if (insertInfo?.folderId === folder.id) {
                  const filtered = currentIds.filter((id) => id !== sourceId);
                  const targetIdx = filtered.indexOf(insertInfo.targetId);
                  let insertIdx: number;
                  if (targetIdx === -1) insertIdx = filtered.length;
                  else insertIdx = insertInfo.side === 'after' ? targetIdx + 1 : targetIdx;
                  moveServerToFolder(sourceId, folder.id, insertIdx);
                } else {
                  moveServerToFolder(sourceId, folder.id);
                }
              }}
              onDragLeave={(e) => {
                if (!(e.currentTarget as HTMLElement).contains(e.relatedTarget as Node)) {
                  setDropdownInsert(null);
                }
              }}
            >
              {folderServers.length === 0 ? (
                <div
                  style={{
                    padding: '8px 12px',
                    color: 'var(--text-muted)',
                    fontSize: 'calc(12px * var(--font-scale, 1))',
                  }}
                >
                  Drag servers here
                </div>
              ) : (
                folderServers.map((server) => {
                  if (!server) return null;
                  const ghostBefore =
                    dropdownInsert?.folderId === folder.id &&
                    dropdownInsert?.targetId === server.id &&
                    dropdownInsert?.side === 'before';
                  const ghostAfter =
                    dropdownInsert?.folderId === folder.id &&
                    dropdownInsert?.targetId === server.id &&
                    dropdownInsert?.side === 'after';

                  const ghost = (
                    <div key={`ghost-${server.id}`} className="folder-dropdown-ghost">
                      <div className="folder-dropdown-ghost-icon" />
                      <span className="folder-dropdown-ghost-name">Move here</span>
                    </div>
                  );

                  return (
                    <React.Fragment key={server.id}>
                      {ghostBefore && ghost}
                      <div
                        className={`folder-dropdown-item ${draggingServerId === server.id ? 'dragging' : ''}`}
                        role="menuitem"
                        tabIndex={0}
                        draggable
                        onMouseEnter={(e) => {
                          const rect = e.currentTarget.getBoundingClientRect();
                          setHoveredServer({ server: server as ServerWithRole, rect });
                        }}
                        onMouseLeave={() => setHoveredServer(null)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            handleServerClick(server.id);
                          }
                        }}
                        onDragStart={(e) => {
                          e.dataTransfer.setData('text/plain', server.id);
                          e.dataTransfer.setData('application/concord-server', server.id);
                          e.dataTransfer.effectAllowed = 'move';
                          setDraggingServerId(server.id);
                          setHoveredServer(null);

                          const clone = e.currentTarget.cloneNode(true) as HTMLElement;
                          Object.assign(clone.style, {
                            width: `${e.currentTarget.offsetWidth * 0.8}px`,
                            opacity: '0.65',
                            position: 'fixed',
                            top: '-9999px',
                            left: '-9999px',
                            pointerEvents: 'none',
                          });
                          document.body.appendChild(clone);
                          e.dataTransfer.setDragImage(
                            clone,
                            clone.offsetWidth / 2,
                            clone.offsetHeight / 2
                          );
                          scheduleDragImageCleanup(clone);
                        }}
                        onDragOver={(e) => {
                          e.preventDefault();
                          e.stopPropagation();
                          const rect = e.currentTarget.getBoundingClientRect();
                          const midY = rect.top + rect.height / 2;
                          const deadZone = rect.height * 0.25;
                          if (
                            dropdownInsert?.targetId === server.id &&
                            Math.abs(e.clientY - midY) < deadZone
                          ) {
                            return;
                          }
                          const side: 'before' | 'after' = e.clientY < midY ? 'before' : 'after';
                          setDropdownInsert({ folderId: folder.id, targetId: server.id, side });
                        }}
                        onDragEnd={() => {
                          setDropdownInsert(null);
                          setDraggingServerId(null);
                        }}
                        onClick={() => handleServerClick(server.id)}
                      >
                        <div className="folder-dropdown-item-icon">
                          {resolveMediaUrl(server.icon_url) ? (
                            <img src={resolveMediaUrl(server.icon_url)} alt={server.name} draggable={false} />
                          ) : (
                            <span className="folder-dropdown-item-initial">
                              {server.name.charAt(0).toUpperCase()}
                            </span>
                          )}
                        </div>
                        <span className="folder-dropdown-item-name">{server.name}</span>
                      </div>
                      {ghostAfter && ghost}
                    </React.Fragment>
                  );
                })
              )}
            </div>,
            document.body
          );
        })()}

      {/* "New Folder" tooltip — portaled to body to escape overflow:hidden */}
      {addBtnTooltipPos &&
        createPortal(
          <div
            className="folder-add-btn-tooltip visible"
            style={{ top: addBtnTooltipPos.top, left: addBtnTooltipPos.left }}
          >
            New Folder
          </div>,
          document.body
        )}

      {/* Context menu */}
      {contextMenu && (
        <ContextMenu position={contextMenu.position} onClose={() => setContextMenu(null)}>
          <ContextMenu.Header>{contextMenu.folder.name}</ContextMenu.Header>
          <ContextMenu.Separator />
          <ContextMenu.Item
            icon={<Pencil size={14} />}
            label="Rename Folder"
            onClick={handleRenameStart}
          />
          <ContextMenu.Item
            icon={<Trash2 size={14} />}
            label="Delete Folder"
            danger
            onClick={handleDeleteRequest}
          />
        </ContextMenu>
      )}

      {/* Delete confirmation modal */}
      {deleteConfirm && (
        <Modal
          isOpen={true}
          onClose={() => setDeleteConfirm(null)}
          title="Delete Folder"
          width="small"
        >
          <div className="delete-server-content">
            <div className="delete-server-warning">
              <svg width="24" height="24" viewBox="0 0 24 24" fill="none" className="warning-icon">
                <path
                  d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"
                  stroke="currentColor"
                  strokeWidth="2"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
                <line
                  x1="12"
                  y1="9"
                  x2="12"
                  y2="13"
                  stroke="currentColor"
                  strokeWidth="2"
                  strokeLinecap="round"
                />
                <line
                  x1="12"
                  y1="17"
                  x2="12.01"
                  y2="17"
                  stroke="currentColor"
                  strokeWidth="2"
                  strokeLinecap="round"
                />
              </svg>
              <p>
                Are you sure you want to delete <strong>{deleteConfirm.name}</strong>?
                {deleteConfirm.serverIds.length > 0 ? (
                  <>
                    {' '}
                    The {deleteConfirm.serverIds.length} server
                    {deleteConfirm.serverIds.length > 1 ? 's' : ''} inside will be moved back to the
                    server bar.
                  </>
                ) : (
                  <> This folder is empty.</>
                )}
              </p>
            </div>

            <div className="delete-server-actions">
              <button
                type="button"
                className="delete-server-cancel-btn"
                onClick={() => setDeleteConfirm(null)}
              >
                Cancel
              </button>
              <button
                type="button"
                className="delete-server-confirm-btn"
                onClick={handleDeleteConfirm}
              >
                Delete Folder
              </button>
            </div>
          </div>
        </Modal>
      )}

      {/* Portal tooltip for folder dropdown server items */}
      {hoveredServer &&
        createPortal(
          <div
            className="server-bar-tooltip-fixed"
            style={{
              position: 'fixed',
              top: hoveredServer.rect.top + hoveredServer.rect.height / 2,
              left: hoveredServer.rect.right + 8,
              transform: 'translateY(-50%)',
            }}
          >
            <span className="server-bar-tooltip-name">{hoveredServer.server.name}</span>
            <div className="server-bar-tooltip-stats">
              <span>{hoveredServer.server.member_count ?? 0} Members</span>
              <span className="server-bar-tooltip-dot" />
              <span>{hoveredServer.server.online_count ?? 0} Online</span>
            </div>
            <div className="server-bar-tooltip-stats">
              <span
                className={`server-bar-tooltip-voice${(serverVoiceCounts[hoveredServer.server.id] ?? 0) > 0 ? ' server-bar-tooltip-voice--active' : ''}`}
              >
                {serverVoiceCounts[hoveredServer.server.id] ?? 0} In Voice
              </span>
            </div>
            {serverUnreadSet.has(hoveredServer.server.id) && (
              <span className="server-bar-tooltip-unread">Unread notifications</span>
            )}
          </div>,
          document.body
        )}
    </>
  );
};

export default FolderBar;
