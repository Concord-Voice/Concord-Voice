import {
  LEGACY_COLLAPSED_WIDTH,
  SIDEBAR_COMPACT_BREAKPOINT,
  SIDEBAR_MIN_WIDTH,
  migrateLegacySidebarState,
  normalizeSidebarProfiles,
  selectSidebarDock,
  type SidebarProfile,
  type SidebarProfiles,
  useLayoutStore,
} from '@/renderer/stores/ui/layoutStore';
import { resetAllStores } from '../../helpers/store-helpers';

const dm: SidebarProfile = {
  left: { width: 240, pinned: true },
  right: { width: 260, pinned: true },
};

const server: SidebarProfile = {
  left: { width: 180, pinned: false },
  right: { width: 120, pinned: false },
};

const defaultProfiles = (): SidebarProfiles => ({
  dm: {
    left: { width: 240, pinned: true },
    right: { width: 260, pinned: true },
  },
  server: {
    left: { width: 240, pinned: true },
    right: { width: 260, pinned: true },
  },
});

describe('layoutStore', () => {
  beforeEach(() => {
    resetAllStores();
    // Reset layout store to defaults
    useLayoutStore.setState({
      sidebarProfiles: defaultProfiles(),
      sidebarLayoutsDecoupled: false,
      serverBarHeight: 48,
      folderBarHeight: 32,
      serverFolders: [],
      serverOrder: [],
      interfaceLocked: false,
    });
  });

  describe('sidebar profiles', () => {
    it('routes coupled Server writes and reads through the DM profile', () => {
      useLayoutStore.setState({
        sidebarProfiles: { dm, server },
        sidebarLayoutsDecoupled: false,
        interfaceLocked: false,
      });

      useLayoutStore.getState().setSidebarWidth('server', 'right', 300);
      useLayoutStore.getState().setSidebarPinned('server', 'left', false);

      expect(useLayoutStore.getState().sidebarProfiles.dm.right.width).toBe(300);
      expect(useLayoutStore.getState().sidebarProfiles.dm.left.pinned).toBe(false);
      expect(useLayoutStore.getState().sidebarProfiles.server.right.width).toBe(120);
      expect(useLayoutStore.getState().sidebarProfiles.server.left.pinned).toBe(false);
      expect(selectSidebarDock(useLayoutStore.getState(), 'server', 'right').width).toBe(300);
    });

    it('routes decoupled Server width and pin writes to the Server profile', () => {
      useLayoutStore.setState({
        sidebarProfiles: { dm, server },
        sidebarLayoutsDecoupled: true,
      });

      useLayoutStore.getState().setSidebarWidth('server', 'right', 300);
      useLayoutStore.getState().setSidebarPinned('server', 'left', true);

      expect(useLayoutStore.getState().sidebarProfiles.dm).toEqual(dm);
      expect(useLayoutStore.getState().sidebarProfiles.server).toEqual({
        left: { width: 180, pinned: true },
        right: { width: 300, pinned: false },
      });
    });

    it('returns retained dock references for valid normalized profiles', () => {
      useLayoutStore.setState({
        sidebarProfiles: { dm, server },
        sidebarLayoutsDecoupled: true,
      });

      expect(selectSidebarDock(useLayoutStore.getState(), 'dm', 'right')).toBe(dm.right);
      expect(selectSidebarDock(useLayoutStore.getState(), 'server', 'right')).toBe(server.right);
    });

    it('preserves dormant Server values across coupling and restores them on re-decoupling', () => {
      useLayoutStore.setState({
        sidebarProfiles: { dm, server },
        sidebarLayoutsDecoupled: true,
      });

      useLayoutStore.getState().setSidebarLayoutsDecoupled(false);
      expect(useLayoutStore.getState().sidebarProfiles.server).toEqual(server);
      expect(selectSidebarDock(useLayoutStore.getState(), 'server', 'left')).toEqual(dm.left);

      useLayoutStore.getState().setSidebarLayoutsDecoupled(true);
      expect(useLayoutStore.getState().sidebarProfiles.server).toEqual(server);
      expect(selectSidebarDock(useLayoutStore.getState(), 'server', 'left')).toEqual(server.left);
    });

    it('provisions only missing or invalid Server fields from normalized DM fields', () => {
      const normalized = normalizeSidebarProfiles({
        dm,
        server: {
          left: { width: 320, pinned: false },
          right: { width: Number.NaN, pinned: false },
        },
      });

      expect(normalized.server).toEqual({
        left: { width: 320, pinned: false },
        right: { width: 260, pinned: false },
      });
      expect(normalizeSidebarProfiles(normalized)).toEqual(normalized);
    });

    it('clones DM into a missing Server profile before enabling decoupling', () => {
      useLayoutStore.setState({
        sidebarProfiles: { dm, server: undefined } as unknown as SidebarProfiles,
        sidebarLayoutsDecoupled: false,
      });

      useLayoutStore.getState().setSidebarLayoutsDecoupled(true);

      expect(useLayoutStore.getState().sidebarProfiles.server).toEqual(dm);
      expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(true);
    });

    it('falls back field-by-field when a malformed Server dock reaches the selector', () => {
      const profiles = {
        dm,
        server: {
          left: server.left,
          right: { width: 300 },
        },
      } as unknown as SidebarProfiles;

      expect(
        selectSidebarDock(
          { sidebarProfiles: profiles, sidebarLayoutsDecoupled: true },
          'server',
          'right'
        )
      ).toEqual({ width: 300, pinned: true });
    });

    it('clamps left and right widths to their side-specific bounds', () => {
      useLayoutStore.getState().setSidebarWidth('dm', 'left', 0);
      expect(useLayoutStore.getState().sidebarProfiles.dm.left.width).toBe(SIDEBAR_MIN_WIDTH);
      useLayoutStore.getState().setSidebarWidth('dm', 'left', 500);
      expect(useLayoutStore.getState().sidebarProfiles.dm.left.width).toBe(400);

      useLayoutStore.getState().setSidebarWidth('dm', 'right', 0);
      expect(useLayoutStore.getState().sidebarProfiles.dm.right.width).toBe(SIDEBAR_MIN_WIDTH);
      useLayoutStore.getState().setSidebarWidth('dm', 'right', 500);
      expect(useLayoutStore.getState().sidebarProfiles.dm.right.width).toBe(340);
    });

    it.each([Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY])(
      'rejects non-finite sidebar width %s without changing retained state',
      (width) => {
        const profiles = useLayoutStore.getState().sidebarProfiles;

        useLayoutStore.getState().setSidebarWidth('dm', 'left', width);

        expect(useLayoutStore.getState().sidebarProfiles).toBe(profiles);
        expect(useLayoutStore.getState().sidebarProfiles.dm.left.width).toBe(240);
      }
    );

    it('normalizes and applies unlocked sidebar preferences with the decoupling flag', () => {
      const profiles = {
        dm: {
          left: { width: 500, pinned: false },
          right: { width: 280, pinned: false },
        },
        server: {
          left: { width: 320, pinned: false },
        },
      } as unknown as SidebarProfiles;

      useLayoutStore.getState().applySidebarPreferences(profiles, true);

      expect(useLayoutStore.getState().sidebarProfiles).toEqual({
        dm: {
          left: { width: 400, pinned: false },
          right: { width: 280, pinned: false },
        },
        server: {
          left: { width: 320, pinned: false },
          right: { width: 280, pinned: false },
        },
      });
      expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(true);
    });

    it('rejects profile and decoupling mutations while Interface Lock is enabled', () => {
      useLayoutStore.setState({
        sidebarProfiles: { dm, server },
        sidebarLayoutsDecoupled: false,
        interfaceLocked: true,
      });

      useLayoutStore.getState().setSidebarWidth('dm', 'left', 300);
      useLayoutStore.getState().setSidebarPinned('dm', 'right', false);
      useLayoutStore.getState().setSidebarLayoutsDecoupled(true);
      useLayoutStore.getState().applySidebarPreferences(defaultProfiles(), true);

      expect(useLayoutStore.getState().sidebarProfiles).toEqual({ dm, server });
      expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(false);
    });

    it('clamps a drag below the floor to SIDEBAR_MIN_WIDTH', () => {
      useLayoutStore.getState().setSidebarWidth('dm', 'left', 10);
      expect(useLayoutStore.getState().sidebarProfiles.dm.left.width).toBe(SIDEBAR_MIN_WIDTH);
    });

    // Value pins for the three exported width constants, kept together as one set.
    //
    // Every other suite reads them symbolically (`SIDEBAR_COMPACT_BREAKPOINT - 1`,
    // `.toBe(SIDEBAR_MIN_WIDTH)`, the migration `it.each`), which locks each
    // boundary's DIRECTION but not its number — without these assertions a
    // find-replace of any of the three literals passes the entire suite.
    //
    // The numbers are not arbitrary. SIDEBAR_COMPACT_BREAKPOINT (135) is derived
    // from the measured text floors (112px at `--sp 1`, 122px at 1.25) against a
    // 93/104px header-chrome floor, and it has a cross-version consequence: widths
    // between the old and new breakpoints now project `expanded` rather than
    // `collapsed` into the synced encrypted-preferences blob. LEGACY_COLLAPSED_WIDTH
    // (56) is the pre-migration rail width, deliberately ABOVE the new resize floor
    // SIDEBAR_MIN_WIDTH (44) so a legacy collapsed panel migrates to the old rail
    // rather than to the floor. Changing any of these is a deliberate act; update
    // this pin in the same commit.
    it('pins the sidebar width constants: floor 44 < legacy rail 56 < compact breakpoint 135', () => {
      expect(SIDEBAR_MIN_WIDTH).toBe(44);
      expect(LEGACY_COLLAPSED_WIDTH).toBe(56);
      expect(SIDEBAR_COMPACT_BREAKPOINT).toBe(135);
    });
  });

  describe('sidebar profile migration', () => {
    it('imports all three rendered legacy widths and maps expanded state', () => {
      localStorage.setItem('concord:channelPanelWidth', '310');
      localStorage.setItem('concord:memberPanelWidth', '330');
      localStorage.setItem('concord:friendsPanelWidth', '300');

      expect(
        migrateLegacySidebarState({
          channelPanelPinned: false,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          friendsPanelMode: 'expanded',
        })
      ).toEqual({
        dm: {
          left: { width: 310, pinned: false },
          right: { width: 300, pinned: true },
        },
        server: {
          left: { width: 310, pinned: false },
          right: { width: 330, pinned: true },
        },
      });
    });

    it.each([
      ['expanded', 290, true],
      ['collapsed', LEGACY_COLLAPSED_WIDTH, true],
      ['hidden', 290, false],
    ] as const)('maps %s legacy right panels to width %i and pinned %s', (mode, width, pinned) => {
      localStorage.setItem('concord:memberPanelWidth', '290');
      localStorage.setItem('concord:friendsPanelWidth', '290');

      const profiles = migrateLegacySidebarState({
        memberPanelMode: mode,
        friendsPanelMode: mode,
      });

      expect(profiles.server.right).toEqual({ width, pinned });
      expect(profiles.dm.right).toEqual({ width, pinned });
    });

    it('ignores invalid local width strings and uses valid persisted/default widths', () => {
      localStorage.setItem('concord:channelPanelWidth', 'not-a-number');
      localStorage.setItem('concord:memberPanelWidth', 'Infinity');
      localStorage.setItem('concord:friendsPanelWidth', '');

      const profiles = migrateLegacySidebarState({
        channelPanelWidth: 250,
        memberPanelWidth: 275,
        memberPanelMode: 'expanded',
        friendsPanelMode: 'expanded',
      });

      expect(profiles.dm.left.width).toBe(250);
      expect(profiles.server.right.width).toBe(275);
      expect(profiles.dm.right.width).toBe(260);
    });

    it.each([
      ['below', 179, 159],
      ['above', 401, 341],
    ] as const)(
      'rejects %s-range legacy widths before applying the new sidebar bounds',
      (_range, channelWidth, rightWidth) => {
        localStorage.setItem('concord:channelPanelWidth', String(channelWidth));
        localStorage.setItem('concord:memberPanelWidth', String(rightWidth));
        localStorage.setItem('concord:friendsPanelWidth', String(rightWidth));

        const profiles = migrateLegacySidebarState({
          channelPanelWidth: channelWidth,
          memberPanelWidth: rightWidth,
          memberPanelMode: 'expanded',
          friendsPanelMode: 'expanded',
        });

        expect(profiles.dm.left.width).toBe(240);
        expect(profiles.server.right.width).toBe(260);
        expect(profiles.dm.right.width).toBe(260);
      }
    );

    it('keeps retained profiles stable on repeated migration', () => {
      const first = migrateLegacySidebarState({ sidebarProfiles: { dm, server } });
      localStorage.setItem('concord:channelPanelWidth', '399');

      expect(migrateLegacySidebarState(first)).toEqual(first);
    });

    it('migrates persisted state to version 2 without losing unrelated preferences', async () => {
      localStorage.setItem('concord:channelPanelWidth', '300');
      localStorage.setItem(
        'concord-layout',
        JSON.stringify({
          version: 1,
          state: {
            channelPanelPinned: false,
            channelPanelWidth: 240,
            memberPanelMode: 'collapsed',
            memberPanelWidth: 280,
            friendsPanelMode: 'hidden',
            serverBarHeight: 60,
            folderBarHeight: 40,
            serverFolders: [{ id: 'folder-1', name: 'Work', serverIds: ['server-1'] }],
            serverOrder: ['server-2'],
            interfaceLocked: true,
          },
        })
      );

      await useLayoutStore.persist.rehydrate();

      const state = useLayoutStore.getState();
      expect(state.sidebarProfiles.dm.left).toEqual({ width: 300, pinned: false });
      expect(state.sidebarProfiles.dm.right).toEqual({ width: 260, pinned: false });
      expect(state.sidebarProfiles.server.right).toEqual({
        width: LEGACY_COLLAPSED_WIDTH,
        pinned: true,
      });
      expect(state.sidebarLayoutsDecoupled).toBe(false);
      expect(state.serverBarHeight).toBe(60);
      expect(state.folderBarHeight).toBe(40);
      expect(state.serverFolders).toEqual([
        { id: 'folder-1', name: 'Work', serverIds: ['server-1'] },
      ]);
      expect(state.serverOrder).toEqual(['server-2']);
      expect(state.interfaceLocked).toBe(true);
      expect(JSON.parse(localStorage.getItem('concord-layout') || '{}').version).toBe(2);
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
    it('persists retained profiles and the decoupling preference', () => {
      useLayoutStore.getState().setSidebarWidth('dm', 'right', 320);
      useLayoutStore.getState().setSidebarLayoutsDecoupled(true);

      const stored = JSON.parse(localStorage.getItem('concord-layout') || '{}');
      expect(stored.state?.sidebarProfiles.dm.right.width).toBe(320);
      expect(stored.state?.sidebarLayoutsDecoupled).toBe(true);
    });

    it('does not retain obsolete panel modes, widths, pins, or transient hover state', () => {
      useLayoutStore.getState().setSidebarWidth('dm', 'left', 350);
      const stored = JSON.parse(localStorage.getItem('concord-layout') || '{}');
      const runtime = useLayoutStore.getState();

      for (const key of [
        'channelPanelPinned',
        'channelPanelWidth',
        'memberPanelMode',
        'memberPanelWidth',
        'friendsPanelMode',
        'channelPanelHoverVisible',
      ]) {
        expect(runtime).not.toHaveProperty(key);
        expect(stored.state).not.toHaveProperty(key);
      }
    });

    it('persists interfaceLocked to localStorage (#188)', () => {
      useLayoutStore.getState().setInterfaceLocked(true);
      const stored = JSON.parse(localStorage.getItem('concord-layout') || '{}');
      expect(stored.state?.interfaceLocked).toBe(true);
    });
  });
});
