import React, { useState, useEffect, useRef, useCallback } from 'react';
import { X } from 'lucide-react';
import EmojiPicker from '../EmojiPicker/LazyEmojiPicker';
import { useFriendOrgStore } from '../../stores/friendOrgStore';
import './CategoryManagerPanel.css';

interface CategoryManagerPanelProps {
  onClose: () => void;
}

const DEFAULT_COLOR = '#99aab5';

/**
 * Create/edit/rename/delete surface for friend categories (#324). Modeled on
 * RoleEditorPanel: a left category list + a right editor with name, the
 * <input type="color"> + hex block, and the EmojiPicker block. All mutations go
 * through friendOrgStore (no server call — the store change drives the sync
 * watcher); deletion opens a confirm noting members move back to Online/Offline.
 */
const CategoryManagerPanel: React.FC<CategoryManagerPanelProps> = ({ onClose }) => {
  const categories = useFriendOrgStore((s) => s.categories);
  const createCategory = useFriendOrgStore((s) => s.createCategory);
  const renameCategory = useFriendOrgStore((s) => s.renameCategory);
  const setCategoryStyle = useFriendOrgStore((s) => s.setCategoryStyle);
  const deleteCategory = useFriendOrgStore((s) => s.deleteCategory);

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [editName, setEditName] = useState('');
  const [editColor, setEditColor] = useState(DEFAULT_COLOR);
  const [editEmoji, setEditEmoji] = useState('');
  const [showEmojiPicker, setShowEmojiPicker] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const emojiPickerRef = useRef<HTMLDivElement>(null);

  const selected = categories.find((c) => c.id === selectedId) || null;

  useEffect(() => {
    if (selected) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editName from the selected category when the selection changes; not a render loop
      setEditName(selected.name);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editColor from the selected category when the selection changes; not a render loop
      setEditColor(selected.color || DEFAULT_COLOR);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editEmoji from the selected category when the selection changes; not a render loop
      setEditEmoji(selected.emoji || '');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: closes the emoji picker when the selection changes; not a render loop
      setShowEmojiPicker(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: closes the delete confirm when the selection changes; not a render loop
      setConfirmDelete(false);
    }
    // eslint-disable-next-line @eslint-react/exhaustive-deps -- keyed on selectedId (stable) NOT `selected` (a fresh categories.find() reference each render); re-running on a `categories` refresh would clobber the user's unsaved in-progress edits
  }, [selectedId]);

  const handleCreate = useCallback(() => {
    const id = createCategory('New Category', '', null);
    setSelectedId(id);
  }, [createCategory]);

  const handleSave = useCallback(() => {
    if (!selectedId) return;
    renameCategory(selectedId, editName);
    setCategoryStyle(selectedId, { emoji: editEmoji, color: editColor });
  }, [selectedId, editName, editEmoji, editColor, renameCategory, setCategoryStyle]);

  const handleConfirmDelete = useCallback(() => {
    if (!selectedId) return;
    deleteCategory(selectedId);
    setSelectedId(null);
    setConfirmDelete(false);
  }, [selectedId, deleteCategory]);

  return (
    <dialog className="category-manager-overlay" open aria-label="Manage Categories">
      <div className="category-manager">
        <div className="category-manager-header">
          <h3>Manage Categories</h3>
          <button
            type="button"
            className="category-manager-close-btn"
            aria-label="Close"
            onClick={onClose}
          >
            <X size={18} />
          </button>
        </div>

        <div className="roles-layout">
          <div className="roles-list">
            {categories.map((cat) => (
              <button
                key={cat.id}
                type="button"
                className={`role-item ${selectedId === cat.id ? 'selected' : ''}`}
                onClick={() => setSelectedId(cat.id)}
              >
                <span
                  className="role-color-dot"
                  style={{ backgroundColor: cat.color || DEFAULT_COLOR }}
                />
                {cat.emoji && <span className="category-item-emoji">{cat.emoji}</span>}
                <span style={cat.color ? { color: cat.color } : undefined}>{cat.name}</span>
              </button>
            ))}
            <button type="button" className="create-role-btn" onClick={handleCreate}>
              + New Category
            </button>
          </div>

          <div className="role-editor">
            {selected ? (
              <>
                <div className="form-group">
                  <label htmlFor="category-editor-name" className="form-label">
                    Category Name
                  </label>
                  <input
                    id="category-editor-name"
                    type="text"
                    className="form-input"
                    value={editName}
                    onChange={(e) => setEditName(e.target.value)}
                  />
                </div>

                <div className="form-group">
                  <label htmlFor="category-editor-color" className="form-label">
                    Category Color
                  </label>
                  <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                    <input
                      id="category-editor-color"
                      type="color"
                      value={editColor}
                      onChange={(e) => setEditColor(e.target.value)}
                      style={{
                        width: '40px',
                        height: '32px',
                        border: 'none',
                        cursor: 'pointer',
                        background: 'none',
                      }}
                    />
                    <input
                      type="text"
                      className="form-input"
                      value={editColor}
                      onChange={(e) => setEditColor(e.target.value)}
                      style={{ width: '120px' }}
                    />
                  </div>
                </div>

                <div className="form-group">
                  <span className="form-label">Category Emoji (Optional)</span>
                  <div className="emoji-input-wrapper" ref={emojiPickerRef}>
                    <div className="emoji-input-container">
                      <button
                        type="button"
                        className={`emoji-picker-button ${editEmoji ? 'has-emoji' : ''}`}
                        onClick={() => setShowEmojiPicker(!showEmojiPicker)}
                        title={editEmoji ? 'Change emoji' : 'Pick an emoji'}
                      >
                        {editEmoji ? (
                          <span className="emoji-picker-button-emoji">{editEmoji}</span>
                        ) : (
                          <span className="emoji-picker-button-placeholder">Pick an emoji</span>
                        )}
                      </button>
                      {editEmoji && (
                        <button
                          type="button"
                          className="emoji-clear-btn"
                          onClick={() => setEditEmoji('')}
                          title="Remove emoji"
                        >
                          ✕
                        </button>
                      )}
                    </div>
                    {showEmojiPicker && (
                      <div className="emoji-picker-container">
                        <EmojiPicker
                          mode="inline"
                          onSelect={(emoji: string) => {
                            setEditEmoji(emoji);
                            setShowEmojiPicker(false);
                          }}
                          onClose={() => setShowEmojiPicker(false)}
                        />
                      </div>
                    )}
                  </div>
                  <span className="channel-form-hint">
                    Shown next to the category name in the friends list and tints member names.
                  </span>
                </div>

                <div className="role-editor-actions">
                  <button
                    type="button"
                    className="server-settings-cancel-btn"
                    onClick={() => setConfirmDelete(true)}
                  >
                    Delete
                  </button>
                  <button type="button" className="server-settings-submit-btn" onClick={handleSave}>
                    Save
                  </button>
                </div>
              </>
            ) : (
              <div className="category-manager-empty">
                Select a category to edit, or create a new one.
              </div>
            )}
          </div>
        </div>
      </div>

      {confirmDelete && selected && (
        <div className="category-delete-confirm-overlay" role="alertdialog" aria-modal="true">
          <div className="category-delete-confirm">
            <h4>Delete &ldquo;{selected.name}&rdquo;?</h4>
            <p>
              Its {selected.memberIds.length}{' '}
              {selected.memberIds.length === 1 ? 'friend' : 'friends'} will move back to
              Online/Offline.
            </p>
            <div className="category-delete-confirm-actions">
              <button
                type="button"
                className="server-settings-cancel-btn"
                onClick={() => setConfirmDelete(false)}
              >
                Cancel
              </button>
              <button
                type="button"
                className="category-delete-confirm-btn"
                onClick={handleConfirmDelete}
              >
                Delete Category
              </button>
            </div>
          </div>
        </div>
      )}
    </dialog>
  );
};

export default CategoryManagerPanel;
