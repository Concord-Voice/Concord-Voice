import ServerActionModal from '../Servers/ServerActionModal';
import CreateServerModal from '../Servers/CreateServerModal';
import JoinServerModal from '../Servers/JoinServerModal';
import CreateChannelModal from '../Channels/CreateChannelModal';
import CreateCategoryModal from '../Channels/CreateCategoryModal';
import DeleteServerModal from '../Servers/DeleteServerModal';
import LeaveServerModal from '../Servers/LeaveServerModal';
import EditChannelModal from '../Channels/EditChannelModal';
import DeleteChannelModal from '../Channels/DeleteChannelModal';
import InviteToServerModal from '../Servers/InviteToServerModal';
import EditCategoryModal from '../Channels/EditCategoryModal';
import DeleteCategoryModal from '../Channels/DeleteCategoryModal';
import ChannelSettingsModal from '../Channels/ChannelSettingsModal';
import CategorySettingsModal from '../Channels/CategorySettingsModal';
import { ServerWithRole } from '../../types/server';
import { Channel, ChannelGroup } from '../../types/chat';

export interface MainViewModalsProps {
  isServerActionModalOpen: boolean;
  setIsServerActionModalOpen: (v: boolean) => void;
  isCreateServerModalOpen: boolean;
  setIsCreateServerModalOpen: (v: boolean) => void;
  isJoinServerModalOpen: boolean;
  setIsJoinServerModalOpen: (v: boolean) => void;
  isCreateChannelModalOpen: boolean;
  setIsCreateChannelModalOpen: (v: boolean) => void;
  isCreateCategoryModalOpen: boolean;
  setIsCreateCategoryModalOpen: (v: boolean) => void;
  deletingServer: ServerWithRole | null;
  setDeletingServer: (v: ServerWithRole | null) => void;
  leavingServer: ServerWithRole | null;
  setLeavingServer: (v: ServerWithRole | null) => void;
  editingChannel: Channel | null;
  setEditingChannel: (v: Channel | null) => void;
  deletingChannel: Channel | null;
  setDeletingChannel: (v: Channel | null) => void;
  invitingServer: ServerWithRole | null;
  setInvitingServer: (v: ServerWithRole | null) => void;
  editingCategory: ChannelGroup | null;
  setEditingCategory: (v: ChannelGroup | null) => void;
  deletingCategory: ChannelGroup | null;
  setDeletingCategory: (v: ChannelGroup | null) => void;
  channelPermissions: Channel | null;
  setChannelPermissions: (v: Channel | null) => void;
  categoryPermissions: ChannelGroup | null;
  setCategoryPermissions: (v: ChannelGroup | null) => void;
  activeServer: ServerWithRole | null;
  onCreateServerSuccess: (server: ServerWithRole) => void;
  onCreateChannelSuccess: (channel: Channel) => void;
}

const MainViewModals = (props: MainViewModalsProps) => {
  return (
    <>
      <ServerActionModal
        isOpen={props.isServerActionModalOpen}
        onClose={() => props.setIsServerActionModalOpen(false)}
        onCreateServer={() => props.setIsCreateServerModalOpen(true)}
        onJoinServer={() => props.setIsJoinServerModalOpen(true)}
      />

      <CreateServerModal
        isOpen={props.isCreateServerModalOpen}
        onClose={() => props.setIsCreateServerModalOpen(false)}
        onSuccess={props.onCreateServerSuccess}
      />

      <JoinServerModal
        isOpen={props.isJoinServerModalOpen}
        onClose={() => props.setIsJoinServerModalOpen(false)}
        onSuccess={props.onCreateServerSuccess}
      />

      <CreateChannelModal
        isOpen={props.isCreateChannelModalOpen}
        onClose={() => props.setIsCreateChannelModalOpen(false)}
        onSuccess={props.onCreateChannelSuccess}
      />

      <CreateCategoryModal
        isOpen={props.isCreateCategoryModalOpen}
        onClose={() => props.setIsCreateCategoryModalOpen(false)}
      />

      {props.deletingServer && (
        <DeleteServerModal
          isOpen={!!props.deletingServer}
          server={props.deletingServer}
          onClose={() => props.setDeletingServer(null)}
        />
      )}

      {props.leavingServer && (
        <LeaveServerModal
          isOpen={!!props.leavingServer}
          server={props.leavingServer}
          onClose={() => props.setLeavingServer(null)}
        />
      )}

      {props.invitingServer && (
        <InviteToServerModal
          isOpen={!!props.invitingServer}
          server={props.invitingServer}
          onClose={() => props.setInvitingServer(null)}
        />
      )}

      {props.editingChannel && (
        <EditChannelModal
          isOpen={!!props.editingChannel}
          channel={props.editingChannel}
          onClose={() => props.setEditingChannel(null)}
        />
      )}

      {props.deletingChannel && (
        <DeleteChannelModal
          isOpen={!!props.deletingChannel}
          channel={props.deletingChannel}
          onClose={() => props.setDeletingChannel(null)}
        />
      )}

      {props.editingCategory && (
        <EditCategoryModal
          isOpen={!!props.editingCategory}
          group={props.editingCategory}
          onClose={() => props.setEditingCategory(null)}
        />
      )}

      {props.deletingCategory && (
        <DeleteCategoryModal
          isOpen={!!props.deletingCategory}
          group={props.deletingCategory}
          onClose={() => props.setDeletingCategory(null)}
        />
      )}

      {props.channelPermissions && props.activeServer && (
        <ChannelSettingsModal
          isOpen={!!props.channelPermissions}
          channel={props.channelPermissions}
          serverId={props.activeServer.id}
          onClose={() => props.setChannelPermissions(null)}
        />
      )}

      {props.categoryPermissions && props.activeServer && (
        <CategorySettingsModal
          isOpen={!!props.categoryPermissions}
          category={props.categoryPermissions}
          serverId={props.activeServer.id}
          onClose={() => props.setCategoryPermissions(null)}
        />
      )}
    </>
  );
};

export default MainViewModals;
