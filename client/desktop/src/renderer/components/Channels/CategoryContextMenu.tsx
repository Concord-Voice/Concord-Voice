import React from 'react';
import { Pencil, Trash2, FolderOpen } from 'lucide-react';
import { ChannelGroup } from '../../types/chat';
import ContextMenu from '../ui/ContextMenu';

interface CategoryContextMenuProps {
  group: ChannelGroup;
  position: { x: number; y: number };
  onClose: () => void;
  onEditCategory: (group: ChannelGroup) => void;
  onDeleteCategory: (group: ChannelGroup) => void;
  onCategoryPermissions?: (group: ChannelGroup) => void;
}

const CategoryContextMenu: React.FC<CategoryContextMenuProps> = ({
  group,
  position,
  onClose,
  onEditCategory,
  onDeleteCategory,
  onCategoryPermissions,
}) => {
  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Header>
        <FolderOpen size={14} />
        <span style={{ marginLeft: 6, fontWeight: 600 }}>{group.name}</span>
      </ContextMenu.Header>

      <ContextMenu.Separator />

      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none">
            <path
              d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        }
        label="Category Permissions"
        onClick={() => {
          onClose();
          onCategoryPermissions?.(group);
        }}
      />

      <ContextMenu.Item
        icon={<Pencil size={16} />}
        label="Edit Category"
        onClick={() => {
          onEditCategory(group);
          onClose();
        }}
      />

      <ContextMenu.Item
        icon={<Trash2 size={16} />}
        label="Delete Category"
        danger
        onClick={() => {
          onDeleteCategory(group);
          onClose();
        }}
      />
    </ContextMenu>
  );
};

export default CategoryContextMenu;
