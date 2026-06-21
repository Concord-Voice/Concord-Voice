import React from 'react';
import { Plus, FolderPlus } from 'lucide-react';
import ContextMenu from '../ui/ContextMenu';

interface ChannelListContextMenuProps {
  position: { x: number; y: number };
  onClose: () => void;
  onCreateChannel: () => void;
  onCreateCategory: () => void;
}

const ChannelListContextMenu: React.FC<ChannelListContextMenuProps> = ({
  position,
  onClose,
  onCreateChannel,
  onCreateCategory,
}) => {
  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Item
        icon={<Plus size={16} />}
        label="Create Channel"
        onClick={() => {
          onCreateChannel();
          onClose();
        }}
      />
      <ContextMenu.Item
        icon={<FolderPlus size={16} />}
        label="Create Category"
        onClick={() => {
          onCreateCategory();
          onClose();
        }}
      />
    </ContextMenu>
  );
};

export default ChannelListContextMenu;
