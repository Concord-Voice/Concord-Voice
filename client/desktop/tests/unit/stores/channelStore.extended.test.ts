import { useChannelStore } from '@/renderer/stores/channelStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockEncryptedChannel } from '../../mocks/fixtures';
import type { Channel, ChannelGroup } from '@/renderer/types/chat';

const mockGroup: ChannelGroup = {
  id: 'group-1',
  server_id: 'server-1',
  name: 'Text Channels',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockGroup2: ChannelGroup = {
  id: 'group-2',
  server_id: 'server-1',
  name: 'Voice Channels',
  position: 1,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

describe('channelStore — extended coverage', () => {
  beforeEach(() => {
    resetAllStores();
    localStorage.clear();
  });

  // --- addChannel deduplication ---

  describe('addChannel deduplication', () => {
    it('does not duplicate when adding same channel twice', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockChannel);
      expect(useChannelStore.getState().channels).toHaveLength(1);
    });
  });

  // --- Channel Group operations ---

  describe('addChannelGroup', () => {
    it('adds a channel group', () => {
      useChannelStore.getState().addChannelGroup(mockGroup);
      expect(useChannelStore.getState().channelGroups).toHaveLength(1);
      expect(useChannelStore.getState().channelGroups[0].name).toBe('Text Channels');
    });

    it('deduplicates groups', () => {
      useChannelStore.getState().addChannelGroup(mockGroup);
      useChannelStore.getState().addChannelGroup(mockGroup);
      expect(useChannelStore.getState().channelGroups).toHaveLength(1);
    });

    it('sorts groups by position', () => {
      useChannelStore.getState().addChannelGroup(mockGroup2);
      useChannelStore.getState().addChannelGroup(mockGroup);
      const groups = useChannelStore.getState().channelGroups;
      expect(groups[0].id).toBe('group-1');
      expect(groups[1].id).toBe('group-2');
    });
  });

  describe('updateChannelGroup', () => {
    it('updates group properties', () => {
      useChannelStore.getState().addChannelGroup(mockGroup);
      useChannelStore.getState().updateChannelGroup('group-1', { name: 'Renamed' });
      expect(useChannelStore.getState().channelGroups[0].name).toBe('Renamed');
    });

    it('re-sorts groups after position update', () => {
      useChannelStore.getState().addChannelGroup(mockGroup);
      useChannelStore.getState().addChannelGroup(mockGroup2);
      // Move group-2 to position 0 (before group-1)
      useChannelStore.getState().updateChannelGroup('group-2', { position: -1 });
      const groups = useChannelStore.getState().channelGroups;
      expect(groups[0].id).toBe('group-2');
    });
  });

  describe('removeChannelGroup', () => {
    it('removes a channel group', () => {
      useChannelStore.getState().addChannelGroup(mockGroup);
      useChannelStore.getState().removeChannelGroup('group-1');
      expect(useChannelStore.getState().channelGroups).toHaveLength(0);
    });

    it('ungroups channels in the removed group', () => {
      useChannelStore.getState().addChannelGroup(mockGroup);
      const channelInGroup: Channel = { ...mockChannel, group_id: 'group-1' };
      useChannelStore.getState().addChannel(channelInGroup);
      useChannelStore.getState().removeChannelGroup('group-1');
      expect(useChannelStore.getState().channels[0].group_id).toBeNull();
    });

    it('removes group from collapsedGroups', () => {
      useChannelStore.setState({ collapsedGroups: ['group-1', 'group-2'] });
      useChannelStore.getState().addChannelGroup(mockGroup);
      useChannelStore.getState().removeChannelGroup('group-1');
      expect(useChannelStore.getState().collapsedGroups).toEqual(['group-2']);
    });
  });

  // --- toggleGroupCollapsed ---

  describe('toggleGroupCollapsed', () => {
    it('collapses an expanded group', () => {
      useChannelStore.setState({ collapsedGroups: [] });
      useChannelStore.getState().toggleGroupCollapsed('group-1');
      expect(useChannelStore.getState().collapsedGroups).toContain('group-1');
    });

    it('expands a collapsed group', () => {
      useChannelStore.setState({ collapsedGroups: ['group-1'] });
      useChannelStore.getState().toggleGroupCollapsed('group-1');
      expect(useChannelStore.getState().collapsedGroups).not.toContain('group-1');
    });
  });

  // --- reorderChannels ---

  describe('reorderChannels', () => {
    it('updates channel positions and group assignments', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      useChannelStore.getState().reorderChannels([
        { channel_id: 'channel-1', group_id: 'group-1', position: 1 },
        { channel_id: 'channel-2', group_id: 'group-1', position: 0 },
      ]);

      const channels = useChannelStore.getState().channels;
      const ch1 = channels.find((c) => c.id === 'channel-1');
      const ch2 = channels.find((c) => c.id === 'channel-2');
      expect(ch1?.group_id).toBe('group-1');
      expect(ch1?.position).toBe(1);
      expect(ch2?.group_id).toBe('group-1');
      expect(ch2?.position).toBe(0);
    });

    it('ignores updates for non-existent channels', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore
        .getState()
        .reorderChannels([{ channel_id: 'nonexistent', group_id: null, position: 0 }]);
      // Should not crash; channel-1 still present
      expect(useChannelStore.getState().channels).toHaveLength(1);
    });
  });

  // --- getLinkedTextChannel ---

  describe('getLinkedTextChannel', () => {
    it('returns linked text channel for a voice channel', () => {
      const linkedText: Channel = {
        ...mockChannel,
        id: 'linked-text-1',
        name: 'voice-text',
        linked_voice_channel_id: 'voice-1',
      };
      useChannelStore.getState().addChannel(linkedText);
      const result = useChannelStore.getState().getLinkedTextChannel('voice-1');
      expect(result?.id).toBe('linked-text-1');
    });

    it('returns undefined when no linked text channel exists', () => {
      useChannelStore.getState().addChannel(mockChannel);
      const result = useChannelStore.getState().getLinkedTextChannel('voice-1');
      expect(result).toBeUndefined();
    });
  });

  // --- setActiveChannel with null channelId ---

  describe('setActiveChannel edge cases', () => {
    it('does not update lastChannelByServer when channelId is null', () => {
      useChannelStore.setState({
        currentServerId: 'server-1',
        lastChannelByServer: { 'server-1': 'old-channel' },
      });
      useChannelStore.getState().setActiveChannel(null);
      expect(useChannelStore.getState().lastChannelByServer['server-1']).toBe('old-channel');
    });
  });
});
