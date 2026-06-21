import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('layoutStore', () => {
  beforeEach(() => {
    resetAllStores();
    // Reset layout store to defaults
    useLayoutStore.setState({
      channelPanelPinned: true,
      channelPanelWidth: 240,
      memberPanelMode: 'expanded',
      memberPanelWidth: 240,
      serverBarHeight: 48,
      folderBarHeight: 32,
      serverFolders: [],
      serverOrder: [],
      interfaceLocked: false,
      channelPanelHoverVisible: false,
    });
  });

  describe('channel panel', () => {
    it('toggles channel pin', () => {
      const initial = useLayoutStore.getState().channelPanelPinned;
      useLayoutStore.getState().toggleChannelPin();
      expect(useLayoutStore.getState().channelPanelPinned).toBe(!initial);
    });

    it('toggleChannelPin also clears hover visible', () => {
      useLayoutStore.setState({ channelPanelHoverVisible: true });
      useLayoutStore.getState().toggleChannelPin();
      expect(useLayoutStore.getState().channelPanelHoverVisible).toBe(false);
    });

    it('clamps channel panel width to valid range', () => {
      useLayoutStore.getState().setChannelPanelWidth(100);
      expect(useLayoutStore.getState().channelPanelWidth).toBe(180);
      useLayoutStore.getState().setChannelPanelWidth(500);
      expect(useLayoutStore.getState().channelPanelWidth).toBe(400);
    });

    it('accepts valid width', () => {
      useLayoutStore.getState().setChannelPanelWidth(300);
      expect(useLayoutStore.getState().channelPanelWidth).toBe(300);
    });

    it('shows and hides channel panel hover', () => {
      useLayoutStore.getState().showChannelPanelHover();
      expect(useLayoutStore.getState().channelPanelHoverVisible).toBe(true);
      useLayoutStore.getState().hideChannelPanelHover();
      expect(useLayoutStore.getState().channelPanelHoverVisible).toBe(false);
    });
  });

  describe('member panel', () => {
    it('sets member panel mode', () => {
      useLayoutStore.getState().setMemberPanelMode('collapsed');
      expect(useLayoutStore.getState().memberPanelMode).toBe('collapsed');
    });

    it('cycles member panel mode', () => {
      useLayoutStore.getState().setMemberPanelMode('expanded');
      useLayoutStore.getState().cycleMemberPanelMode();
      expect(useLayoutStore.getState().memberPanelMode).toBe('collapsed');
      useLayoutStore.getState().cycleMemberPanelMode();
      expect(useLayoutStore.getState().memberPanelMode).toBe('hidden');
      useLayoutStore.getState().cycleMemberPanelMode();
      expect(useLayoutStore.getState().memberPanelMode).toBe('expanded');
    });

    it('clamps member panel width', () => {
      useLayoutStore.getState().setMemberPanelWidth(100);
      expect(useLayoutStore.getState().memberPanelWidth).toBe(160);
      useLayoutStore.getState().setMemberPanelWidth(500);
      expect(useLayoutStore.getState().memberPanelWidth).toBe(340);
    });
  });

  describe('server bar', () => {
    it('clamps server bar height', () => {
      useLayoutStore.getState().setServerBarHeight(20);
      expect(useLayoutStore.getState().serverBarHeight).toBe(36);
      useLayoutStore.getState().setServerBarHeight(100);
      expect(useLayoutStore.getState().serverBarHeight).toBe(64);
    });

    it('accepts valid server bar height', () => {
      useLayoutStore.getState().setServerBarHeight(50);
      expect(useLayoutStore.getState().serverBarHeight).toBe(50);
    });

    it('clamps folder bar height', () => {
      useLayoutStore.getState().setFolderBarHeight(10);
      expect(useLayoutStore.getState().folderBarHeight).toBe(24);
      useLayoutStore.getState().setFolderBarHeight(100);
      expect(useLayoutStore.getState().folderBarHeight).toBe(48);
    });

    it('accepts valid folder bar height', () => {
      useLayoutStore.getState().setFolderBarHeight(36);
      expect(useLayoutStore.getState().folderBarHeight).toBe(36);
    });
  });

  describe('server folders', () => {
    it('creates a folder', () => {
      useLayoutStore.getState().createFolder('Gaming');
      const folders = useLayoutStore.getState().serverFolders;
      expect(folders).toHaveLength(1);
      expect(folders[0].name).toBe('Gaming');
    });

    it('creates a folder with initial servers', () => {
      useLayoutStore.setState({ serverOrder: ['server-1', 'server-2', 'server-3'] });
      useLayoutStore.getState().createFolder('Work', ['server-1', 'server-2']);
      expect(useLayoutStore.getState().serverFolders[0].serverIds).toEqual([
        'server-1',
        'server-2',
      ]);
      // Foldered servers are removed from top-level order
      expect(useLayoutStore.getState().serverOrder).toEqual(['server-3']);
    });

    it('renames a folder', () => {
      useLayoutStore.getState().createFolder('Gaming');
      const folderId = useLayoutStore.getState().serverFolders[0].id;
      useLayoutStore.getState().renameFolder(folderId, 'Work');
      expect(useLayoutStore.getState().serverFolders[0].name).toBe('Work');
    });

    it('deletes a folder and returns servers to order', () => {
      useLayoutStore.getState().createFolder('Gaming', ['server-1']);
      const folderId = useLayoutStore.getState().serverFolders[0].id;
      useLayoutStore.getState().deleteFolder(folderId);
      expect(useLayoutStore.getState().serverFolders).toHaveLength(0);
      expect(useLayoutStore.getState().serverOrder).toContain('server-1');
    });

    it('moves server to folder', () => {
      useLayoutStore.getState().createFolder('Gaming');
      const folderId = useLayoutStore.getState().serverFolders[0].id;
      useLayoutStore.setState({ serverOrder: ['server-1', 'server-2'] });
      useLayoutStore.getState().moveServerToFolder('server-1', folderId);
      expect(useLayoutStore.getState().serverFolders[0].serverIds).toContain('server-1');
      expect(useLayoutStore.getState().serverOrder).not.toContain('server-1');
    });

    it('moves server to folder at specific index', () => {
      useLayoutStore.getState().createFolder('Gaming', ['server-a', 'server-b']);
      const folderId = useLayoutStore.getState().serverFolders[0].id;
      useLayoutStore.setState({ serverOrder: ['server-1'] });
      useLayoutStore.getState().moveServerToFolder('server-1', folderId, 1);
      expect(useLayoutStore.getState().serverFolders[0].serverIds).toEqual([
        'server-a',
        'server-1',
        'server-b',
      ]);
    });

    it('removes server from folder', () => {
      useLayoutStore.getState().createFolder('Gaming', ['server-1']);
      useLayoutStore.getState().removeServerFromFolder('server-1');
      expect(useLayoutStore.getState().serverFolders[0].serverIds).not.toContain('server-1');
      expect(useLayoutStore.getState().serverOrder).toContain('server-1');
    });

    it('reorders servers within a folder', () => {
      useLayoutStore.getState().createFolder('Gaming', ['server-1', 'server-2', 'server-3']);
      const folderId = useLayoutStore.getState().serverFolders[0].id;
      useLayoutStore
        .getState()
        .reorderFolderServers(folderId, ['server-3', 'server-1', 'server-2']);
      expect(useLayoutStore.getState().serverFolders[0].serverIds).toEqual([
        'server-3',
        'server-1',
        'server-2',
      ]);
    });
  });

  describe('reorderServers', () => {
    it('reorders server list', () => {
      useLayoutStore.getState().reorderServers(['server-2', 'server-1']);
      expect(useLayoutStore.getState().serverOrder).toEqual(['server-2', 'server-1']);
    });
  });

  describe('interface lock (#188)', () => {
    it('defaults to unlocked', () => {
      expect(useLayoutStore.getState().interfaceLocked).toBe(false);
    });

    it('setInterfaceLocked sets the flag in both directions', () => {
      useLayoutStore.getState().setInterfaceLocked(true);
      expect(useLayoutStore.getState().interfaceLocked).toBe(true);
      useLayoutStore.getState().setInterfaceLocked(false);
      expect(useLayoutStore.getState().interfaceLocked).toBe(false);
    });
  });

  describe('persistence', () => {
    it('persists layout settings to localStorage', () => {
      useLayoutStore.getState().setChannelPanelWidth(350);
      const stored = JSON.parse(localStorage.getItem('concord-layout') || '{}');
      expect(stored.state?.channelPanelWidth).toBe(350);
    });

    it('does not persist channelPanelHoverVisible', () => {
      useLayoutStore.getState().showChannelPanelHover();
      const stored = JSON.parse(localStorage.getItem('concord-layout') || '{}');
      expect(stored.state?.channelPanelHoverVisible).toBeUndefined();
    });

    it('persists interfaceLocked to localStorage (#188)', () => {
      useLayoutStore.getState().setInterfaceLocked(true);
      const stored = JSON.parse(localStorage.getItem('concord-layout') || '{}');
      expect(stored.state?.interfaceLocked).toBe(true);
    });
  });
});
