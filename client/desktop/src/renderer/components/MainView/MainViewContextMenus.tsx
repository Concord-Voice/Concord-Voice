import { useLocation } from 'react-router-dom';
import ServerContextMenu from '../Servers/ServerContextMenu';
import ChannelContextMenu from '../Channels/ChannelContextMenu';
import ChannelListContextMenu from '../Channels/ChannelListContextMenu';
import CategoryContextMenu from '../Channels/CategoryContextMenu';
import { ServerWithRole } from '../../types/server';
import { Channel, ChannelGroup } from '../../types/chat';

export interface MainViewContextMenusProps {
  serverContextMenu: { server: ServerWithRole; position: { x: number; y: number } } | null;
  setServerContextMenu: (v: null) => void;
  channelContextMenu: { channel: Channel; position: { x: number; y: number } } | null;
  setChannelContextMenu: (v: null) => void;
  categoryContextMenu: { group: ChannelGroup; position: { x: number; y: number } } | null;
  setCategoryContextMenu: (v: null) => void;
  emptyContextMenu: { position: { x: number; y: number } } | null;
  setEmptyContextMenu: (v: null) => void;
  activeServer: ServerWithRole | null;
  canManageChannels: boolean;
  onEditServer: (server: ServerWithRole) => void;
  onDeleteServer: (server: ServerWithRole) => void;
  onLeaveServer: (server: ServerWithRole) => void;
  onInviteServer: (server: ServerWithRole) => void;
  onEditChannel: (channel: Channel) => void;
  onDeleteChannel: (channel: Channel) => void;
  onChannelPermissions: (channel: Channel) => void;
  onEditCategory: (group: ChannelGroup) => void;
  onDeleteCategory: (group: ChannelGroup) => void;
  onCategoryPermissions: (group: ChannelGroup) => void;
  onOpenCreateChannelModal: () => void;
  onOpenCreateCategoryModal: () => void;
}

const MainViewContextMenus = (props: MainViewContextMenusProps) => {
  // activeServerId is preserved when the user navigates to /app/dms (so back-nav
  // returns them to the same server), but that means a right-click in DM empty
  // space would otherwise pass the activeServer && canManageChannels guard and
  // render the server-page Create Channel / Create Category menu — the bug from
  // issue #984. Read the route to suppress the empty-area menu on DM routes.
  const location = useLocation();
  const isDmRoute = location.pathname.startsWith('/app/dms');

  return (
    <>
      {props.serverContextMenu && (
        <ServerContextMenu
          server={props.serverContextMenu.server}
          position={props.serverContextMenu.position}
          onClose={() => props.setServerContextMenu(null)}
          onEditServer={props.onEditServer}
          onDeleteServer={props.onDeleteServer}
          onLeaveServer={props.onLeaveServer}
          onInvite={props.onInviteServer}
        />
      )}

      {props.channelContextMenu && props.activeServer && (
        <ChannelContextMenu
          channel={props.channelContextMenu.channel}
          position={props.channelContextMenu.position}
          serverId={props.activeServer.id}
          onClose={() => props.setChannelContextMenu(null)}
          onEditChannel={props.onEditChannel}
          onDeleteChannel={(channel) => {
            props.setChannelContextMenu(null);
            props.onDeleteChannel(channel);
          }}
          onChannelPermissions={(channel) => {
            props.setChannelContextMenu(null);
            props.onChannelPermissions(channel);
          }}
        />
      )}

      {props.emptyContextMenu && !isDmRoute && props.activeServer && props.canManageChannels && (
        <ChannelListContextMenu
          position={props.emptyContextMenu.position}
          onClose={() => props.setEmptyContextMenu(null)}
          onCreateChannel={() => {
            props.setEmptyContextMenu(null);
            props.onOpenCreateChannelModal();
          }}
          onCreateCategory={() => {
            props.setEmptyContextMenu(null);
            props.onOpenCreateCategoryModal();
          }}
        />
      )}

      {props.categoryContextMenu && props.activeServer && props.canManageChannels && (
        <CategoryContextMenu
          group={props.categoryContextMenu.group}
          position={props.categoryContextMenu.position}
          onClose={() => props.setCategoryContextMenu(null)}
          onEditCategory={(group) => {
            props.setCategoryContextMenu(null);
            props.onEditCategory(group);
          }}
          onDeleteCategory={(group) => {
            props.setCategoryContextMenu(null);
            props.onDeleteCategory(group);
          }}
          onCategoryPermissions={(group) => {
            props.setCategoryContextMenu(null);
            props.onCategoryPermissions(group);
          }}
        />
      )}
    </>
  );
};

export default MainViewContextMenus;
