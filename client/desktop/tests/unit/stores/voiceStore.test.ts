import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voiceStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  useVoiceStore.getState().reset();
  // Clear fields that reset() preserves (device IDs, layout prefs)
  useVoiceStore.setState({
    audioInputDeviceId: null,
    audioOutputDeviceId: null,
    videoDeviceId: null,
    qualityTier: 'standard',
    effectiveQualityTier: 'standard',
    voiceTextChatHeight: 300,
    voiceTextChatWidth: 350,
    voiceTextChatLayout: 'horizontal',
    keepActiveWhileUnfocused: false,
    channelVoiceMembers: {},
    serverVoiceCounts: {},
    voiceControlsPinned: false,
    voiceControlsPoppedOut: false,
  });
  localStorage.clear();
});

const mockParticipant: VoiceParticipant = {
  userId: 'user-1',
  username: 'alice',
  isMuted: false,
  isDeafened: false,
  isVideoOn: false,
  isScreenSharing: false,
  isSpeaking: false,
  serverMuted: false,
  serverDeafened: false,
};

describe('voiceStore', () => {
  describe('channel management', () => {
    it('sets active channel', () => {
      useVoiceStore.getState().setActiveChannel('voice-1', 'General', 'server-1');
      expect(useVoiceStore.getState().activeChannelId).toBe('voice-1');
    });

    it('reset clears voice state', () => {
      useVoiceStore.getState().setActiveChannel('voice-1', 'General', 'server-1');
      useVoiceStore.getState().addParticipant(mockParticipant);

      useVoiceStore.getState().reset();

      const state = useVoiceStore.getState();
      expect(state.activeChannelId).toBeNull();
      expect(Object.keys(state.participants)).toHaveLength(0);
      expect(state.connectionState).toBe('disconnected');
    });
  });

  describe('self state toggles', () => {
    it('toggles muted', () => {
      useVoiceStore.getState().setMuted(true);
      expect(useVoiceStore.getState().isMuted).toBe(true);

      useVoiceStore.getState().setMuted(false);
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });

    it('toggles deafened', () => {
      useVoiceStore.getState().setDeafened(true);
      expect(useVoiceStore.getState().isDeafened).toBe(true);
    });

    it('toggles video', () => {
      useVoiceStore.getState().setVideoOn(true);
      expect(useVoiceStore.getState().isVideoOn).toBe(true);
    });

    it('toggles screen sharing', () => {
      useVoiceStore.getState().setScreenSharing(true);
      expect(useVoiceStore.getState().isScreenSharing).toBe(true);
    });
  });

  describe('participant management', () => {
    it('adds participant', () => {
      useVoiceStore.getState().addParticipant(mockParticipant);
      expect(Object.keys(useVoiceStore.getState().participants)).toHaveLength(1);
      expect(useVoiceStore.getState().participants['user-1']?.username).toBe('alice');
    });

    it('updates participant', () => {
      useVoiceStore.getState().addParticipant(mockParticipant);
      useVoiceStore.getState().updateParticipant('user-1', { isMuted: true, isSpeaking: true });

      const p = useVoiceStore.getState().participants['user-1'];
      expect(p?.isMuted).toBe(true);
      expect(p?.isSpeaking).toBe(true);
    });

    it('removes participant', () => {
      useVoiceStore.getState().addParticipant(mockParticipant);
      useVoiceStore.getState().removeParticipant('user-1');
      expect(Object.keys(useVoiceStore.getState().participants)).toHaveLength(0);
    });
  });

  describe('connection state', () => {
    it('transitions connection state', () => {
      useVoiceStore.getState().setConnectionState('connecting');
      expect(useVoiceStore.getState().connectionState).toBe('connecting');

      useVoiceStore.getState().setConnectionState('connected');
      expect(useVoiceStore.getState().connectionState).toBe('connected');

      useVoiceStore.getState().setConnectionState('failed');
      expect(useVoiceStore.getState().connectionState).toBe('failed');
    });
  });

  describe('audio quality tiers', () => {
    it('sets quality tier', () => {
      useVoiceStore.getState().setQualityTier('hifi');
      expect(useVoiceStore.getState().qualityTier).toBe('hifi');
    });
  });

  describe('device selection', () => {
    it('sets audio input device', () => {
      useVoiceStore.getState().setAudioInputDevice('mic-1');
      expect(useVoiceStore.getState().audioInputDeviceId).toBe('mic-1');
    });

    it('sets audio output device', () => {
      useVoiceStore.getState().setAudioOutputDevice('speaker-1');
      expect(useVoiceStore.getState().audioOutputDeviceId).toBe('speaker-1');
    });

    it('sets video device', () => {
      useVoiceStore.getState().setVideoDevice('cam-1');
      expect(useVoiceStore.getState().videoDeviceId).toBe('cam-1');
    });
  });

  // ===== Screen share management =====

  describe('screen share management', () => {
    it('adds available screen share', () => {
      useVoiceStore.getState().addAvailableScreenShare({
        producerId: 'p1',
        userId: 'u1',
        username: 'alice',
      });
      expect(useVoiceStore.getState().availableScreenShares).toHaveLength(1);
    });

    it('prevents duplicate screen shares', () => {
      const share = { producerId: 'p1', userId: 'u1', username: 'alice' };
      useVoiceStore.getState().addAvailableScreenShare(share);
      useVoiceStore.getState().addAvailableScreenShare(share);
      expect(useVoiceStore.getState().availableScreenShares).toHaveLength(1);
    });

    it('removes screen share by producerId', () => {
      useVoiceStore.getState().addAvailableScreenShare({
        producerId: 'p1',
        userId: 'u1',
        username: 'alice',
      });
      useVoiceStore.getState().removeAvailableScreenShare('p1');
      expect(useVoiceStore.getState().availableScreenShares).toHaveLength(0);
    });

    it('clears all screen shares', () => {
      useVoiceStore
        .getState()
        .addAvailableScreenShare({ producerId: 'p1', userId: 'u1', username: 'alice' });
      useVoiceStore
        .getState()
        .addAvailableScreenShare({ producerId: 'p2', userId: 'u2', username: 'bob' });
      useVoiceStore.getState().clearAvailableScreenShares();
      expect(useVoiceStore.getState().availableScreenShares).toHaveLength(0);
    });
  });

  // ===== Channel voice members =====

  describe('channel voice members', () => {
    it('sets members for a channel', () => {
      useVoiceStore.getState().setChannelVoiceMembers('ch1', [
        { userId: 'u1', username: 'alice' },
        { userId: 'u2', username: 'bob' },
      ]);
      expect(useVoiceStore.getState().channelVoiceMembers['ch1']).toHaveLength(2);
    });

    it('adds member and prevents duplicates', () => {
      useVoiceStore.getState().addChannelVoiceMember('ch1', { userId: 'u1', username: 'alice' });
      useVoiceStore.getState().addChannelVoiceMember('ch1', { userId: 'u1', username: 'alice' });
      expect(useVoiceStore.getState().channelVoiceMembers['ch1']).toHaveLength(1);
    });

    it('removes member and cleans up empty channels', () => {
      useVoiceStore.getState().addChannelVoiceMember('ch1', { userId: 'u1', username: 'alice' });
      useVoiceStore.getState().removeChannelVoiceMember('ch1', 'u1');
      expect(useVoiceStore.getState().channelVoiceMembers['ch1']).toBeUndefined();
    });

    it('clears all channel voice members', () => {
      useVoiceStore.getState().addChannelVoiceMember('ch1', { userId: 'u1', username: 'alice' });
      useVoiceStore.getState().addChannelVoiceMember('ch2', { userId: 'u2', username: 'bob' });
      useVoiceStore.getState().clearAllChannelVoiceMembers();
      expect(Object.keys(useVoiceStore.getState().channelVoiceMembers)).toHaveLength(0);
    });
  });

  // ===== Voice text chat =====

  describe('voice text chat', () => {
    it('toggles voice text chat', () => {
      expect(useVoiceStore.getState().showVoiceTextChat).toBe(false);
      useVoiceStore.getState().toggleVoiceTextChat();
      expect(useVoiceStore.getState().showVoiceTextChat).toBe(true);
      useVoiceStore.getState().toggleVoiceTextChat();
      expect(useVoiceStore.getState().showVoiceTextChat).toBe(false);
    });

    it('sets voice text chat height and persists', () => {
      useVoiceStore.getState().setVoiceTextChatHeight(500);
      expect(useVoiceStore.getState().voiceTextChatHeight).toBe(500);
      expect(localStorage.getItem('concord:voice-text-chat-height')).toBe('500');
    });

    it('sets voice text chat layout and persists', () => {
      useVoiceStore.getState().setVoiceTextChatLayout('vertical');
      expect(useVoiceStore.getState().voiceTextChatLayout).toBe('vertical');
      expect(localStorage.getItem('concord:voice-text-chat-layout')).toBe('vertical');
    });

    it('toggles voice text chat layout', () => {
      expect(useVoiceStore.getState().voiceTextChatLayout).toBe('horizontal');
      useVoiceStore.getState().toggleVoiceTextChatLayout();
      expect(useVoiceStore.getState().voiceTextChatLayout).toBe('vertical');
      useVoiceStore.getState().toggleVoiceTextChatLayout();
      expect(useVoiceStore.getState().voiceTextChatLayout).toBe('horizontal');
    });

    it('sets voice text chat width and persists', () => {
      useVoiceStore.getState().setVoiceTextChatWidth(400);
      expect(useVoiceStore.getState().voiceTextChatWidth).toBe(400);
      expect(localStorage.getItem('concord:voice-text-chat-width')).toBe('400');
    });
  });

  // ===== Server voice counts =====

  describe('server voice counts', () => {
    it('sets server voice count', () => {
      useVoiceStore.getState().setServerVoiceCount('s1', 5);
      expect(useVoiceStore.getState().serverVoiceCounts['s1']).toBe(5);
    });

    it('increments server voice count', () => {
      useVoiceStore.getState().setServerVoiceCount('s1', 3);
      useVoiceStore.getState().incrementServerVoiceCount('s1');
      expect(useVoiceStore.getState().serverVoiceCounts['s1']).toBe(4);
    });

    it('increments from zero when no previous count', () => {
      useVoiceStore.getState().incrementServerVoiceCount('s2');
      expect(useVoiceStore.getState().serverVoiceCounts['s2']).toBe(1);
    });

    it('decrements server voice count', () => {
      useVoiceStore.getState().setServerVoiceCount('s1', 3);
      useVoiceStore.getState().decrementServerVoiceCount('s1');
      expect(useVoiceStore.getState().serverVoiceCounts['s1']).toBe(2);
    });

    it('does not go below zero', () => {
      useVoiceStore.getState().setServerVoiceCount('s1', 0);
      useVoiceStore.getState().decrementServerVoiceCount('s1');
      expect(useVoiceStore.getState().serverVoiceCounts['s1']).toBe(0);
    });
  });

  // ===== Multi-stream screen share =====

  describe('multi-stream screen share', () => {
    it('tuneIn adds mapping and recalculates slots', () => {
      useVoiceStore.getState().tuneIn('prod-1', 'cons-1');
      expect(useVoiceStore.getState().tunedInScreenShares['prod-1']).toBe('cons-1');
      expect(useVoiceStore.getState().maxVideoSlots).toBe(45); // 50 - 5
    });

    it('tuneOut removes mapping', () => {
      useVoiceStore.getState().tuneIn('prod-1', 'cons-1');
      useVoiceStore.getState().tuneOut('prod-1');
      expect(useVoiceStore.getState().tunedInScreenShares['prod-1']).toBeUndefined();
      expect(useVoiceStore.getState().maxVideoSlots).toBe(50);
    });

    it('tuneOut updates dominant when removing dominant share', () => {
      useVoiceStore.getState().tuneIn('prod-1', 'cons-1');
      useVoiceStore.getState().tuneIn('prod-2', 'cons-2');
      useVoiceStore.getState().setDominantScreenShare('prod-1');

      useVoiceStore.getState().tuneOut('prod-1');
      // Dominant should switch to remaining share
      expect(useVoiceStore.getState().dominantScreenShareId).toBe('prod-2');
    });

    it('setDominantScreenShare', () => {
      useVoiceStore.getState().setDominantScreenShare('prod-1');
      expect(useVoiceStore.getState().dominantScreenShareId).toBe('prod-1');
    });

    it('recalculateMaxVideoSlots', () => {
      useVoiceStore.getState().tuneIn('p1', 'c1');
      useVoiceStore.getState().tuneIn('p2', 'c2');
      useVoiceStore.getState().recalculateMaxVideoSlots();
      expect(useVoiceStore.getState().maxVideoSlots).toBe(40); // 50 - (2 * 5)
    });
  });

  // ===== Layout sections =====

  describe('layout sections', () => {
    it('toggles user frame bar', () => {
      const initial = useVoiceStore.getState().showUserFrameBar;
      useVoiceStore.getState().toggleUserFrameBar();
      expect(useVoiceStore.getState().showUserFrameBar).toBe(!initial);
    });

    it('toggles stream bar', () => {
      const initial = useVoiceStore.getState().showStreamBar;
      useVoiceStore.getState().toggleStreamBar();
      expect(useVoiceStore.getState().showStreamBar).toBe(!initial);
    });

    it('sets user frame bar height', () => {
      useVoiceStore.getState().setUserFrameBarHeight(200);
      expect(useVoiceStore.getState().userFrameBarHeight).toBe(200);
    });

    it('sets stream bar height', () => {
      useVoiceStore.getState().setStreamBarHeight(150);
      expect(useVoiceStore.getState().streamBarHeight).toBe(150);
    });

    it('sets and toggles stage layout', () => {
      useVoiceStore.getState().setStageLayout('focus');
      expect(useVoiceStore.getState().stageLayout).toBe('focus');

      useVoiceStore.getState().toggleStageLayout();
      expect(useVoiceStore.getState().stageLayout).toBe('equal');
    });
  });

  // ===== PiP windows =====

  describe('PiP windows', () => {
    it('adds PiP window', () => {
      useVoiceStore.getState().addPipWindow('pip-1');
      expect(useVoiceStore.getState().pipWindows).toContain('pip-1');
    });

    it('prevents duplicate PiP windows', () => {
      useVoiceStore.getState().addPipWindow('pip-1');
      useVoiceStore.getState().addPipWindow('pip-1');
      expect(useVoiceStore.getState().pipWindows).toHaveLength(1);
    });

    it('removes PiP window', () => {
      useVoiceStore.getState().addPipWindow('pip-1');
      useVoiceStore.getState().removePipWindow('pip-1');
      expect(useVoiceStore.getState().pipWindows).toHaveLength(0);
    });
  });

  // ===== Stream focus =====

  describe('stream focus', () => {
    it('sets keepActiveWhileUnfocused and persists', () => {
      useVoiceStore.getState().setKeepActiveWhileUnfocused(true);
      expect(useVoiceStore.getState().keepActiveWhileUnfocused).toBe(true);
      expect(localStorage.getItem('concord:keep-active-unfocused')).toBe('true');
    });

    it('sets local stream paused', () => {
      useVoiceStore.getState().setLocalStreamPaused(true);
      expect(useVoiceStore.getState().localStreamPaused).toBe(true);
    });
  });

  // ===== Persistent voice controls =====

  describe('persistent voice controls', () => {
    it('toggles voice controls pinned', () => {
      expect(useVoiceStore.getState().voiceControlsPinned).toBe(false);
      useVoiceStore.getState().toggleVoiceControlsPinned();
      expect(useVoiceStore.getState().voiceControlsPinned).toBe(true);
    });

    it('sets voice controls popped out', () => {
      useVoiceStore.getState().setVoiceControlsPoppedOut(true);
      expect(useVoiceStore.getState().voiceControlsPoppedOut).toBe(true);
    });

    it('sets persistent text chat height', () => {
      useVoiceStore.getState().setPersistentTextChatHeight(250);
      expect(useVoiceStore.getState().persistentTextChatHeight).toBe(250);
    });
  });

  // ===== DM voice calls =====

  describe('DM voice calls', () => {
    it('sets DM call state', () => {
      useVoiceStore.getState().setDMCall(true, 'conv-1');
      expect(useVoiceStore.getState().isDMCall).toBe(true);
      expect(useVoiceStore.getState().dmConversationId).toBe('conv-1');
    });

    it('clears DM call state', () => {
      useVoiceStore.getState().setDMCall(true, 'conv-1');
      useVoiceStore.getState().setDMCall(false);
      expect(useVoiceStore.getState().isDMCall).toBe(false);
      expect(useVoiceStore.getState().dmConversationId).toBeNull();
    });
  });

  // ===== Bandwidth saving =====

  describe('bandwidth saving', () => {
    it('sets solo bandwidth saving', () => {
      useVoiceStore.getState().setSoloBandwidthSaving(true);
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(true);
    });

    it('sets solo bandwidth notification', () => {
      useVoiceStore.getState().setSoloBandwidthNotification(true);
      expect(useVoiceStore.getState().soloBandwidthNotification).toBe(true);
    });
  });

  // ===== Codec management =====

  describe('codec management', () => {
    it('sets codec floor', () => {
      useVoiceStore.getState().setCodecFloor(['video/VP8', 'video/VP9']);
      expect(useVoiceStore.getState().codecFloor).toEqual(['video/VP8', 'video/VP9']);
    });

    it('sets active camera codec', () => {
      useVoiceStore.getState().setActiveCameraCodec('video/h264');
      expect(useVoiceStore.getState().activeCameraCodec).toBe('video/h264');
    });

    it('sets active screen codec', () => {
      useVoiceStore.getState().setActiveScreenCodec('video/av1');
      expect(useVoiceStore.getState().activeScreenCodec).toBe('video/av1');
    });
  });

  // ===== Packet loss & health =====

  describe('packet loss and health', () => {
    it('sets packet loss without warning', () => {
      useVoiceStore.getState().setPacketLoss(2.5);
      expect(useVoiceStore.getState().packetLossPercent).toBe(2.5);
      expect(useVoiceStore.getState().packetLossWarning).toBe(false);
    });

    it('sets packet loss with warning threshold', () => {
      useVoiceStore.getState().setPacketLoss(10, 5);
      expect(useVoiceStore.getState().packetLossPercent).toBe(10);
      expect(useVoiceStore.getState().packetLossWarning).toBe(true);
    });

    it('clears warning when below threshold', () => {
      useVoiceStore.getState().setPacketLoss(10, 5);
      useVoiceStore.getState().setPacketLoss(3, 5);
      expect(useVoiceStore.getState().packetLossWarning).toBe(false);
    });

    it('sets decoder health', () => {
      useVoiceStore.getState().setDecoderHealth('red');
      expect(useVoiceStore.getState().decoderHealth).toBe('red');
    });
  });

  // ===== Video slot error =====

  describe('video slot error', () => {
    it('sets error message', () => {
      useVoiceStore.getState().setVideoSlotError('Max slots reached');
      expect(useVoiceStore.getState().videoSlotError).toBe('Max slots reached');
    });

    it('clears error', () => {
      useVoiceStore.getState().setVideoSlotError('error');
      useVoiceStore.getState().setVideoSlotError(null);
      expect(useVoiceStore.getState().videoSlotError).toBeNull();
    });
  });

  // ===== Active speaker =====

  describe('active speaker', () => {
    it('sets active speaker', () => {
      useVoiceStore.getState().setActiveSpeaker('user-1');
      expect(useVoiceStore.getState().activeSpeakerId).toBe('user-1');
    });

    it('clears active speaker', () => {
      useVoiceStore.getState().setActiveSpeaker('user-1');
      useVoiceStore.getState().setActiveSpeaker(null);
      expect(useVoiceStore.getState().activeSpeakerId).toBeNull();
    });
  });

  // ===== setParticipants + clearParticipants =====

  describe('bulk participant operations', () => {
    it('sets multiple participants at once', () => {
      useVoiceStore.getState().setParticipants([
        { ...mockParticipant, userId: 'u1', username: 'alice' },
        { ...mockParticipant, userId: 'u2', username: 'bob' },
      ]);
      expect(Object.keys(useVoiceStore.getState().participants)).toHaveLength(2);
    });

    it('clears all participants', () => {
      useVoiceStore.getState().addParticipant(mockParticipant);
      useVoiceStore.getState().clearParticipants();
      expect(Object.keys(useVoiceStore.getState().participants)).toHaveLength(0);
    });
  });

  // ===== Reset preserves preferences =====

  describe('reset preserves preferences', () => {
    it('preserves device settings on reset', () => {
      useVoiceStore.getState().setAudioInputDevice('my-mic');
      useVoiceStore.getState().setAudioOutputDevice('my-speaker');
      useVoiceStore.getState().setVideoDevice('my-cam');
      useVoiceStore.getState().setQualityTier('hifi');

      useVoiceStore.getState().reset();

      const state = useVoiceStore.getState();
      expect(state.audioInputDeviceId).toBe('my-mic');
      expect(state.audioOutputDeviceId).toBe('my-speaker');
      expect(state.videoDeviceId).toBe('my-cam');
      expect(state.qualityTier).toBe('hifi');
    });

    it('preserves layout settings on reset', () => {
      useVoiceStore.getState().setVoiceTextChatHeight(500);
      useVoiceStore.getState().setVoiceTextChatWidth(400);
      useVoiceStore.getState().setKeepActiveWhileUnfocused(true);

      useVoiceStore.getState().reset();

      const state = useVoiceStore.getState();
      expect(state.voiceTextChatHeight).toBe(500);
      expect(state.voiceTextChatWidth).toBe(400);
      expect(state.keepActiveWhileUnfocused).toBe(true);
    });

    it('clears transient state on reset', () => {
      useVoiceStore.getState().setConnectionState('connected');
      useVoiceStore.getState().setActiveChannel('ch1', 'General', 's1');
      useVoiceStore.getState().setMuted(true);
      useVoiceStore.getState().addParticipant(mockParticipant);
      useVoiceStore.getState().setSoloBandwidthSaving(true);
      useVoiceStore.getState().setCodecFloor(['video/VP8']);

      useVoiceStore.getState().reset();

      const state = useVoiceStore.getState();
      expect(state.connectionState).toBe('disconnected');
      expect(state.activeChannelId).toBeNull();
      expect(state.isMuted).toBe(false);
      expect(Object.keys(state.participants)).toHaveLength(0);
      expect(state.isSoloBandwidthSaving).toBe(false);
      expect(state.codecFloor).toBeNull();
    });
  });

  // ===== Effective quality tier =====

  describe('effective quality tier', () => {
    it('sets effective quality tier independently', () => {
      useVoiceStore.getState().setQualityTier('low');
      useVoiceStore.getState().setEffectiveQualityTier('high');
      expect(useVoiceStore.getState().qualityTier).toBe('low');
      expect(useVoiceStore.getState().effectiveQualityTier).toBe('high');
    });
  });

  // ===== Server voice counts batch =====

  describe('server voice counts batch', () => {
    it('sets all server voice counts at once', () => {
      useVoiceStore.getState().setServerVoiceCounts({ s1: 3, s2: 7 });
      expect(useVoiceStore.getState().serverVoiceCounts).toEqual({ s1: 3, s2: 7 });
    });
  });

  // ===== Server enforcement =====

  describe('server enforcement', () => {
    it('should update participant serverMuted via updateParticipant', () => {
      useVoiceStore.getState().addParticipant({
        ...mockParticipant,
        userId: 'user-1',
        username: 'alice',
      });
      useVoiceStore.getState().updateParticipant('user-1', { serverMuted: true });
      expect(useVoiceStore.getState().participants['user-1']?.serverMuted).toBe(true);
    });

    it('should update participant serverDeafened via updateParticipant', () => {
      useVoiceStore.getState().addParticipant({
        ...mockParticipant,
        userId: 'user-1',
        username: 'alice',
      });
      useVoiceStore.getState().updateParticipant('user-1', { serverDeafened: true });
      expect(useVoiceStore.getState().participants['user-1']?.serverDeafened).toBe(true);
    });

    it('should update channel voice member enforcement state', () => {
      useVoiceStore.getState().setChannelVoiceMembers('ch-1', [
        {
          userId: 'user-1',
          username: 'alice',
          isMuted: false,
          serverMuted: false,
          serverDeafened: false,
        },
      ]);
      useVoiceStore.getState().updateChannelVoiceMember('ch-1', 'user-1', { serverMuted: true });
      const members = useVoiceStore.getState().channelVoiceMembers['ch-1'];
      expect(members?.[0]?.serverMuted).toBe(true);
    });

    it('should not modify other members when updating one', () => {
      useVoiceStore.getState().setChannelVoiceMembers('ch-1', [
        {
          userId: 'user-1',
          username: 'alice',
          isMuted: false,
          serverMuted: false,
          serverDeafened: false,
        },
        {
          userId: 'user-2',
          username: 'bob',
          isMuted: false,
          serverMuted: false,
          serverDeafened: false,
        },
      ]);
      useVoiceStore.getState().updateChannelVoiceMember('ch-1', 'user-1', { serverMuted: true });
      const members = useVoiceStore.getState().channelVoiceMembers['ch-1'];
      expect(members?.[1]?.serverMuted).toBe(false);
    });

    it('should handle updateChannelVoiceMember for non-existent channel', () => {
      // Should not throw
      useVoiceStore
        .getState()
        .updateChannelVoiceMember('nonexistent', 'user-1', { serverMuted: true });
      expect(useVoiceStore.getState().channelVoiceMembers['nonexistent']).toBeUndefined();
    });

    it('should set and clear group DM info', () => {
      useVoiceStore.getState().setGroupDMInfo(true, 'admin');
      expect(useVoiceStore.getState().isGroupDM).toBe(true);
      expect(useVoiceStore.getState().callerDMRole).toBe('admin');

      useVoiceStore.getState().setGroupDMInfo(false, null);
      expect(useVoiceStore.getState().isGroupDM).toBe(false);
      expect(useVoiceStore.getState().callerDMRole).toBeNull();
    });

    it('should set effective permissions', () => {
      useVoiceStore.getState().setEffectivePermissions(0x40000n);
      expect(useVoiceStore.getState().effectivePermissions).toBe(0x40000n);
    });
  });

  // ===== Active DM calls roster (#1219 R4) =====

  describe('activeDMCalls roster', () => {
    it('applyDMVoiceState reduces joined/left/room_empty', () => {
      const s = useVoiceStore.getState();
      s.applyDMVoiceState('CONV', 'joined', 'u1', 5);
      s.applyDMVoiceState('CONV', 'joined', 'u2', 5);
      expect(useVoiceStore.getState().activeDMCalls['CONV']).toEqual({
        participantIds: ['u1', 'u2'],
        total: 5,
      });
      s.applyDMVoiceState('CONV', 'left', 'u1', 5);
      expect(useVoiceStore.getState().activeDMCalls['CONV']?.participantIds).toEqual(['u2']);
      s.applyDMVoiceState('CONV', 'room_empty', undefined, 5);
      expect(useVoiceStore.getState().activeDMCalls['CONV']).toBeUndefined();
    });

    it('applyDMVoiceState does not duplicate a re-joining user', () => {
      const s = useVoiceStore.getState();
      s.applyDMVoiceState('CONV', 'joined', 'u1', 3);
      s.applyDMVoiceState('CONV', 'joined', 'u1', 3);
      expect(useVoiceStore.getState().activeDMCalls['CONV']?.participantIds).toEqual(['u1']);
    });

    it('applyDMVoiceState deletes the entry when the last participant leaves', () => {
      const s = useVoiceStore.getState();
      s.applyDMVoiceState('CONV', 'joined', 'u1', 3);
      s.applyDMVoiceState('CONV', 'left', 'u1', 3);
      expect(useVoiceStore.getState().activeDMCalls['CONV']).toBeUndefined();
    });

    it('applyDMVoiceState ignores joined/left with no userId', () => {
      const s = useVoiceStore.getState();
      s.applyDMVoiceState('CONV', 'joined', undefined, 3);
      expect(useVoiceStore.getState().activeDMCalls['CONV']).toBeUndefined();
    });

    it('seedActiveDMCall hydrates the roster', () => {
      useVoiceStore.getState().seedActiveDMCall('CONV', ['u1', 'u2'], 5);
      expect(useVoiceStore.getState().activeDMCalls['CONV']).toEqual({
        participantIds: ['u1', 'u2'],
        total: 5,
      });
    });

    it('clearActiveDMCall removes the roster entry', () => {
      useVoiceStore.getState().seedActiveDMCall('CONV', ['u1'], 5);
      useVoiceStore.getState().clearActiveDMCall('CONV');
      expect(useVoiceStore.getState().activeDMCalls['CONV']).toBeUndefined();
    });

    it('reset clears activeDMCalls', () => {
      useVoiceStore.getState().seedActiveDMCall('CONV', ['u1'], 5);
      useVoiceStore.getState().reset();
      expect(useVoiceStore.getState().activeDMCalls).toEqual({});
    });
  });
});
