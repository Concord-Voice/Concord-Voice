import { describe, expect, it } from 'vitest';
import {
  getPresenceActivityAudience,
  presentRemoteActivities,
  presentSelfActivity,
} from '@/renderer/utils/ui/richPresencePresentation';
import type { PresenceSettings } from '@/renderer/stores/ui/richPresenceStore';

const channelId = '11111111-1111-4111-8111-111111111111';
const serverId = '22222222-2222-4222-8222-222222222222';

const settings: PresenceSettings = {
  masterEnabled: true,
  serverVoiceTier: 1,
  serverVoiceShowDetails: true,
  privateCallTier: 1,
  privateCallShowDetails: true,
  customTextTier: 0,
};

describe('rich presence presentation', () => {
  it('orders remote activities by server voice, private call, then custom status', () => {
    expect(
      presentRemoteActivities({
        private_call: {
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'dm' },
          updated_at: 1,
        },
        server_voice: {
          category: 'server_voice',
          minimized: false,
          payload: {
            channel_id: channelId,
            channel_name: 'Lobby',
            server_id: serverId,
            server_name: 'Concord',
          },
          updated_at: 1,
        },
        custom_text: {
          category: 'custom_text',
          payload: { emoji: '🎮', text: 'Gaming' },
        },
      })
    ).toEqual([
      {
        category: 'server_voice',
        label: 'Server Voice',
        headline: 'In voice',
        detail: 'Lobby · Concord',
        minimized: false,
      },
      {
        category: 'private_call',
        label: 'Private Call',
        headline: 'In a private call',
        minimized: true,
      },
      {
        category: 'custom_text',
        label: 'Custom Status',
        headline: 'Gaming',
        emoji: '🎮',
        minimized: false,
      },
    ]);
  });

  it('omits minimized details and preserves custom status emoji/text', () => {
    expect(
      presentRemoteActivities({
        server_voice: {
          category: 'server_voice',
          minimized: true,
          payload: { channel_id: channelId, server_id: serverId },
          updated_at: 1,
        },
        private_call: {
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'group' },
          updated_at: 1,
        },
      })
    ).toEqual([
      {
        category: 'server_voice',
        label: 'Server Voice',
        headline: 'In voice',
        minimized: true,
      },
      {
        category: 'private_call',
        label: 'Private Call',
        headline: 'In a group call',
        minimized: true,
      },
    ]);

    expect(
      presentRemoteActivities({
        private_call: {
          category: 'private_call',
          minimized: false,
          payload: Object.assign(
            { call_type: 'group' as const, participant_count: 3 },
            { participant_names: ['Alice Example'] }
          ),
          updated_at: 1,
        },
        custom_text: { category: 'custom_text', payload: { emoji: '🎧', text: 'Listening' } },
      })
    ).toEqual([
      {
        category: 'private_call',
        label: 'Private Call',
        headline: 'In a group call',
        detail: 'With 3 people',
        minimized: false,
      },
      {
        category: 'custom_text',
        label: 'Custom Status',
        headline: 'Listening',
        emoji: '🎧',
        minimized: false,
      },
    ]);
  });

  it('never presents identity-shaped private-call fields', () => {
    const presented = presentRemoteActivities({
      private_call: {
        category: 'private_call',
        minimized: false,
        payload: Object.assign(
          { call_type: 'group' as const, participant_count: 2 },
          { participant_names: ['Alice Example', 'Bob Example'] }
        ),
        updated_at: 1,
      },
    });

    expect(presented).toEqual([
      {
        category: 'private_call',
        label: 'Private Call',
        headline: 'In a group call',
        detail: 'With 2 people',
        minimized: false,
      },
    ]);
    expect(JSON.stringify(presented)).not.toContain('Alice Example');
    expect(JSON.stringify(presented)).not.toContain('Bob Example');
  });

  it('uses the settings audience wording for master, server, and private tiers', () => {
    expect(getPresenceActivityAudience('server_voice', { ...settings, masterEnabled: false })).toBe(
      'Nobody'
    );
    expect(getPresenceActivityAudience('server_voice', { ...settings, serverVoiceTier: 0 })).toBe(
      'Nobody'
    );
    expect(getPresenceActivityAudience('server_voice', { ...settings, serverVoiceTier: 1 })).toBe(
      'Friends—and eligible friends-of-friends—who are in this server and can view this voice channel.'
    );
    expect(getPresenceActivityAudience('server_voice', { ...settings, serverVoiceTier: 2 })).toBe(
      'People in this server who can view this voice channel.'
    );
    expect(getPresenceActivityAudience('private_call', { ...settings, privateCallTier: 0 })).toBe(
      'People currently in this private call.'
    );
    expect(getPresenceActivityAudience('private_call', { ...settings, privateCallTier: 1 })).toBe(
      'People currently in this call, plus your friends and eligible friends-of-friends.'
    );
    expect(getPresenceActivityAudience('private_call', { ...settings, privateCallTier: 2 })).toBe(
      'People currently in this call, plus your friends, eligible friends-of-friends, and people who share a server with you.'
    );
  });

  it('presents coarse self activity and confirmed eligibility only', () => {
    expect(
      presentSelfActivity(
        { category: 'server_voice', channelId, channelName: 'Lobby', serverId },
        settings,
        'online'
      )
    ).toEqual({
      headline: 'In voice',
      eligibility:
        'Friends—and eligible friends-of-friends—who are in this server and can view this voice channel.',
    });

    expect(
      presentSelfActivity(
        { category: 'private_call', callType: 'group', participantCount: 3 },
        settings,
        'online'
      )
    ).toMatchObject({ headline: 'In a group call' });
  });

  it('returns null for no local activity and fixed notes for Invisible or Offline', () => {
    expect(presentSelfActivity(null, settings, 'online')).toBeNull();
    expect(
      presentSelfActivity({ category: 'private_call', callType: 'dm' }, null, 'invisible')
    ).toEqual({
      headline: 'In a private call',
      eligibility: 'Audience unavailable',
      deliveryNote: 'Not currently shared while Invisible',
    });
    expect(
      presentSelfActivity({ category: 'private_call', callType: 'dm' }, settings, 'offline')
    ).toMatchObject({
      deliveryNote: 'Not currently shared while Offline',
    });
  });
});
