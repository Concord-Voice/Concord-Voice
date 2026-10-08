import React, { useEffect } from 'react';
import Modal from '../ui/Modal';
import OverridePanel, { type OverrideUpsertArgs } from '../Permissions/OverridePanel';
import {
  usePermissionStore,
  ChannelOverride,
  NO_PERMISSION_WRITES,
} from '../../stores/chat/permissionStore';
import { useMemberStore } from '../../stores/chat/memberStore';
import { ChannelGroup } from '../../types/chat';
import './CategorySettingsModal.css';

interface CategorySettingsModalProps {
  isOpen: boolean;
  category: ChannelGroup;
  serverId: string;
  onClose: () => void;
}

const CategorySettingsModal: React.FC<CategorySettingsModalProps> = ({
  isOpen,
  category,
  serverId,
  onClose,
}) => {
  const fetchCategoryOverrides = usePermissionStore((s) => s.fetchCategoryOverrides);
  const upsertCategoryOverride = usePermissionStore((s) => s.upsertCategoryOverride);
  const deleteCategoryOverride = usePermissionStore((s) => s.deleteCategoryOverride);
  const fetchRoles = usePermissionStore((s) => s.fetchRoles);
  const serverRoles = usePermissionStore((s) => s.serverRoles);
  const channelOverrides = usePermissionStore((s) => s.channelOverrides);
  const members = useMemberStore((s) => s.members);

  const storeKey = `category:${category.id}`;
  const overrides: ChannelOverride[] = channelOverrides[storeKey] ?? [];
  // Survives a close and reopen, unlike the panel's own write lock (#3406
  // review, round 6).
  const writesInFlight = usePermissionStore(
    (s) => s.permissionWritesInFlight[storeKey] ?? NO_PERMISSION_WRITES
  );
  const roles = serverRoles[serverId] ?? [];

  useEffect(() => {
    if (isOpen) {
      fetchCategoryOverrides(category.id);
      fetchRoles(serverId);
    }
  }, [isOpen, category.id, serverId, fetchCategoryOverrides, fetchRoles]);

  const handleUpsert = (...args: OverrideUpsertArgs) =>
    upsertCategoryOverride(category.id, ...args);

  const handleDelete = (overrideId: string) => deleteCategoryOverride(category.id, overrideId);

  return (
    <Modal
      isOpen={isOpen}
      onClose={onClose}
      title={`Category Permissions — ${category.name}`}
      width="large"
    >
      <OverridePanel
        overrides={overrides}
        roles={roles}
        members={members}
        onUpsert={handleUpsert}
        onDelete={handleDelete}
        writesInFlight={writesInFlight}
        stepUpPurpose="overrides.category_upsert"
        emptyMessage="No permission overrides configured for this category."
      />
    </Modal>
  );
};

export default CategorySettingsModal;
