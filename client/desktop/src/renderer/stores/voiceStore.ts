import { createStore } from '../utils/createStore';
import type { CallState } from '../services/voiceService/callStateMachine';

// ---------------------------------------------------------------------------
// Persisted device settings (per-machine via localStorage)
// ---------------------------------------------------------------------------
const VOICE_SETTINGS_KEY = 'concord:voice-settings';

interface PersistedVoiceSettings {
  audioInputDeviceId: string | null;
  audioOutputDeviceId: string | null;
  videoDeviceId: string | null;
  qualityTier: AudioQualityTier;
}

function loadPersistedSettings(): Partial<PersistedVoiceSettings> {
  try {
    const raw = localStorage.getItem(VOICE_SETTINGS_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw) as Partial<PersistedVoiceSettings>;
    // Migrate old tier name → new tier name
    if ((parsed.qualityTier as string) === 'voice') parsed.qualityTier = 'low';
    return parsed;
  } catch {
    return {};
  }
}

function savePersistedSettings(settings: PersistedVoiceSettings): void {
  try {
    localStorage.setItem(VOICE_SETTINGS_KEY, JSON.stringify(settings));
  } catch {
    // localStorage full or unavailable — ignore
  }
}

// ---------------------------------------------------------------------------
// Voice participant state
// ---------------------------------------------------------------------------
export interface VoiceParticipant {
  userId: string;
  username: string;
  displayName?: string;
  avatarUrl?: string;
  isMuted: boolean;
  isDeafened: boolean;
  serverMuted: boolean;
  serverDeafened: boolean;
  isTesting?: boolean;
  isVideoOn: boolean;
  isScreenSharing: boolean;
  isSpeaking: boolean;
  audioStream?: MediaStream;
  videoStream?: MediaStream;
  screenStream?: MediaStream;
  screenAudioStream?: MediaStream;
}

// ---------------------------------------------------------------------------
// DM caller role (admin can enforce mute/deafen in group DMs)
// ---------------------------------------------------------------------------
export type DMCallerRole = 'admin' | 'member' | null;

// ---------------------------------------------------------------------------
// Audio quality tiers (must match server-side AUDIO_QUALITY_TIERS)
// ---------------------------------------------------------------------------
export type AudioQualityTier =
  | 'minimum'
  | 'low'
  | 'moderate'
  | 'standard'
  | 'high'
  | 'hifi'
  | 'studio';

export interface AudioQualityTierConfig {
  label: string;
  description: string;
  maxBitrate: number;
  opusDtx: boolean;
  opusFec: boolean;
  opusStereo: boolean;
  preferredFrameSize: 10 | 20 | 40 | 60;
  premium: boolean;
}

export const AUDIO_QUALITY_TIERS: Record<AudioQualityTier, AudioQualityTierConfig> = {
  minimum: {
    label: 'Minimum',
    description: 'Optimized for pure survival over quality',
    maxBitrate: 16_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 60,
    premium: false,
  },
  low: {
    label: 'Low',
    description: 'Prioritizes keeping you in the conversation',
    maxBitrate: 32_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 40,
    premium: false,
  },
  moderate: {
    label: 'Moderate',
    description: 'The industry standard sweet spot',
    maxBitrate: 64_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 20,
    premium: false,
  },
  standard: {
    label: 'Standard',
    description: 'The Concord default, maximum clarity',
    maxBitrate: 96_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 20,
    premium: false,
  },
  high: {
    label: 'High',
    description: 'Virtually transparent clarity',
    maxBitrate: 192_000,
    opusDtx: false,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 10,
    premium: true,
  },
  hifi: {
    label: 'Hi-Fi',
    description: 'Maximum fidelity for power users',
    maxBitrate: 256_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    preferredFrameSize: 10,
    premium: true,
  },
  studio: {
    label: 'Studio',
    description: 'The absolute ceiling, acoustically transparent 48kHz/16-bit',
    maxBitrate: 510_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    preferredFrameSize: 10,
    premium: true,
  },
};

// ---------------------------------------------------------------------------
// Available screen shares (opt-in "Tune In" model)
// ---------------------------------------------------------------------------
export interface AvailableScreenShare {
  producerId: string;
  userId: string;
  username: string;
  displayName?: string;
}

// ---------------------------------------------------------------------------
// Channel voice members (sidebar display — who's in each voice channel)
// ---------------------------------------------------------------------------
export interface ChannelVoiceMember {
  userId: string;
  username: string;
  displayName?: string;
  avatarUrl?: string;
  isMuted: boolean;
  /** Self-deafen (#685). For non-active channels this stays false (the SFU
   *  broadcast is room-scoped), so it is meaningful for the active channel /
   *  local optimistic update; the sidebar's active-channel path reads deafen
   *  from `participants` (VoiceParticipant) which carries the live value. */
  isDeafened: boolean;
  serverMuted: boolean;
  serverDeafened: boolean;
}

/**
 * Wire shape of a voice participant returned by the control-plane REST endpoints
 * (initial fetch + the WS-triggered refetch). snake_case per the API contract.
 */
export interface ApiVoiceParticipant {
  user_id: string;
  username: string;
  display_name?: string;
  avatar_url?: string;
  is_muted?: boolean;
  server_muted?: boolean;
  server_deafened?: boolean;
}

/**
 * Map a REST API participant to a sidebar {@link ChannelVoiceMember}. Shared by
 * the channel-list fetch and the WS refetch so the mapping lives in one place
 * (#685). Self-deafen is SFU-broadcast (room-scoped), NOT carried by these REST
 * endpoints, so it defaults false here; the live value flows through
 * `participants` for the active channel.
 */
export function channelVoiceMemberFromApi(p: ApiVoiceParticipant): ChannelVoiceMember {
  return {
    userId: p.user_id,
    username: p.username,
    displayName: p.display_name,
    avatarUrl: p.avatar_url,
    isMuted: p.is_muted || false,
    isDeafened: false,
    serverMuted: p.server_muted || false,
    serverDeafened: p.server_deafened || false,
  };
}

// ---------------------------------------------------------------------------
// Connection state
// ---------------------------------------------------------------------------
export type VoiceConnectionState =
  | 'disconnected'
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'error';

/** Decoder budget profiling health zone (IGNIS). */
export type DecoderHealth = 'green' | 'yellow' | 'red';

// ---------------------------------------------------------------------------
// Voice store
// ---------------------------------------------------------------------------
interface VoiceState {
  // Connection
  activeChannelId: string | null;
  activeChannelName: string | null;
  activeServerId: string | null;
  connectionState: VoiceConnectionState;

  // Local user state
  isMuted: boolean;
  isDeafened: boolean;
  isVideoOn: boolean;
  isScreenSharing: boolean;
  localIsTesting: boolean;

  // Participants (keyed by userId)
  participants: Record<string, VoiceParticipant>;
  activeSpeakerId: string | null;

  // Device selection
  audioInputDeviceId: string | null;
  audioOutputDeviceId: string | null;
  videoDeviceId: string | null;

  // Quality
  qualityTier: AudioQualityTier;
  effectiveQualityTier: AudioQualityTier; // Resolved: channel override or personal tier

  // Network quality (updated by packet loss monitor)
  packetLossPercent: number;
  packetLossWarning: boolean;

  // Decoder health (IGNIS profiling)
  decoderHealth: DecoderHealth;

  // Available screen shares (opt-in "Tune In" model — not auto-consumed)
  availableScreenShares: AvailableScreenShare[];

  // Channel voice members (sidebar — all voice channels, not just the one we're in)
  channelVoiceMembers: Record<string, ChannelVoiceMember[]>;

  // Voice text chat
  showVoiceTextChat: boolean;
  voiceTextChatHeight: number;
  voiceTextChatLayout: 'horizontal' | 'vertical';
  voiceTextChatWidth: number;

  // Server-wide voice counts (for tooltip display)
  serverVoiceCounts: Record<string, number>;

  // Multi-stream screen share consumption (producerId → consumerId, up to 5)
  tunedInScreenShares: Record<string, string>;
  dominantScreenShareId: string | null;

  // Video slot enforcement: 50 - (5 * tuned-in screen share count)
  maxVideoSlots: number;
  videoSlotError: string | null;

  // Layout section visibility (Mode B — three-section layout)
  showUserFrameBar: boolean;
  showStreamBar: boolean;
  userFrameBarHeight: number;
  streamBarHeight: number;

  // Stage layout: 'equal' = all streams share center stage equally,
  //               'focus' = one dominant stream + others in bottom StreamBar
  stageLayout: 'equal' | 'focus';

  // PiP state
  pipWindows: string[];

  // Stream focus behavior
  keepActiveWhileUnfocused: boolean;
  // When true, the local user's own stream previews are hidden to save GPU/decode resources
  localStreamPaused: boolean;

  // Persistent voice controls (when navigated away from voice channel)
  voiceControlsPinned: boolean;
  voiceControlsPoppedOut: boolean;
  persistentTextChatHeight: number;

  // DM voice call state
  isDMCall: boolean;
  dmConversationId: string | null;
  isGroupDM: boolean;
  callerDMRole: DMCallerRole;
  effectivePermissions: bigint;

  // DM voice call ring state machine (#1209 plan task D2/D3). Mirror of
  // the call-state machinery in voiceService/callStateMachine.ts —
  // exposed on the store so UI components (IncomingCallBanner,
  // OutgoingCallModal, sidebar "in-call" indicator) can subscribe to
  // state transitions via selective Zustand subscriptions. See spec §7.4.
  callState: CallState;

  // Active DM-call rosters keyed by conversation ID (#1219 R4). Tracks who
  // is currently in a DM voice call (for calls the local user may NOT be in)
  // so the "Join voice call" header affordance (R5), the "N of M in call"
  // list indicator (R6), and the ringtone-suppression gate (R9) can render
  // multi-participant state. Fed by live `dm_voice_state_update` deltas and
  // hydrated on conversation-open via GET /voice/participants. `total` is the
  // group's member count (denominator for "N of M"); `participantIds` is the
  // live roster (numerator).
  activeDMCalls: Record<string, { participantIds: string[]; total: number }>;

  // Solo bandwidth saving (applies to both DM calls and server voice)
  isSoloBandwidthSaving: boolean;
  soloBandwidthNotification: boolean;

  // Room codec floor — universally-supported video codecs (lowercase mimeTypes)
  // null = no restriction (<2 participants with capabilities)
  codecFloor: string[] | null;

  // Currently active video codecs (lowercase mimeTypes, null when not producing)
  activeCameraCodec: string | null;
  activeScreenCodec: string | null;

  // Actions
  setConnectionState: (state: VoiceConnectionState) => void;
  setActiveChannel: (channelId: string | null, channelName?: string, serverId?: string) => void;
  setMuted: (muted: boolean) => void;
  setDeafened: (deafened: boolean) => void;
  setVideoOn: (on: boolean) => void;
  setScreenSharing: (sharing: boolean) => void;
  setLocalIsTesting: (testing: boolean) => void;
  setActiveSpeaker: (userId: string | null) => void;
  setAudioInputDevice: (deviceId: string) => void;
  setAudioOutputDevice: (deviceId: string) => void;
  setVideoDevice: (deviceId: string) => void;
  setQualityTier: (tier: AudioQualityTier) => void;
  setEffectiveQualityTier: (tier: AudioQualityTier) => void;
  setPacketLoss: (percent: number, warningThreshold?: number) => void;
  setDecoderHealth: (health: 'green' | 'yellow' | 'red') => void;

  // Screen share opt-in management
  addAvailableScreenShare: (share: AvailableScreenShare) => void;
  removeAvailableScreenShare: (producerId: string) => void;
  clearAvailableScreenShares: () => void;

  // Participant management
  addParticipant: (participant: VoiceParticipant) => void;
  removeParticipant: (userId: string) => void;
  updateParticipant: (userId: string, update: Partial<VoiceParticipant>) => void;
  setParticipants: (participants: VoiceParticipant[]) => void;
  clearParticipants: () => void;

  // Channel voice members management (sidebar)
  setChannelVoiceMembers: (channelId: string, members: ChannelVoiceMember[]) => void;
  addChannelVoiceMember: (channelId: string, member: ChannelVoiceMember) => void;
  removeChannelVoiceMember: (channelId: string, userId: string) => void;
  updateChannelVoiceMember: (
    channelId: string,
    userId: string,
    update: Partial<ChannelVoiceMember>
  ) => void;
  clearAllChannelVoiceMembers: () => void;

  // Voice text chat
  setShowVoiceTextChat: (show: boolean) => void;
  toggleVoiceTextChat: () => void;
  setVoiceTextChatHeight: (height: number) => void;
  setVoiceTextChatLayout: (layout: 'horizontal' | 'vertical') => void;
  toggleVoiceTextChatLayout: () => void;
  setVoiceTextChatWidth: (width: number) => void;

  // Server voice counts (tooltip)
  setServerVoiceCounts: (counts: Record<string, number>) => void;
  setServerVoiceCount: (serverId: string, count: number) => void;
  incrementServerVoiceCount: (serverId: string) => void;
  decrementServerVoiceCount: (serverId: string) => void;

  // Multi-stream screen share
  tuneIn: (producerId: string, consumerId: string) => void;
  tuneOut: (producerId: string) => void;
  setDominantScreenShare: (producerId: string | null) => void;
  recalculateMaxVideoSlots: () => void;
  setVideoSlotError: (msg: string | null) => void;

  // Layout sections (Mode B)
  toggleUserFrameBar: () => void;
  toggleStreamBar: () => void;
  setUserFrameBarHeight: (height: number) => void;
  setStreamBarHeight: (height: number) => void;
  setStageLayout: (layout: 'equal' | 'focus') => void;
  toggleStageLayout: () => void;

  // PiP
  addPipWindow: (id: string) => void;
  removePipWindow: (id: string) => void;

  // Stream focus behavior
  setKeepActiveWhileUnfocused: (keep: boolean) => void;
  setLocalStreamPaused: (paused: boolean) => void;

  // Persistent voice controls
  setVoiceControlsPinned: (pinned: boolean) => void;
  toggleVoiceControlsPinned: () => void;
  setVoiceControlsPoppedOut: (poppedOut: boolean) => void;
  setPersistentTextChatHeight: (height: number) => void;

  // DM voice call
  setDMCall: (isDM: boolean, conversationId?: string | null) => void;
  setGroupDMInfo: (isGroupDM: boolean, callerRole: DMCallerRole) => void;
  setEffectivePermissions: (permissions: bigint) => void;

  // DM voice call ring state machine setter (#1209)
  setCallState: (state: CallState) => void;

  // Active DM-call roster actions (#1219 R4)
  applyDMVoiceState: (
    conversationId: string,
    action: string,
    userId: string | undefined,
    total: number
  ) => void;
  seedActiveDMCall: (conversationId: string, participantIds: string[], total: number) => void;
  clearActiveDMCall: (conversationId: string) => void;

  // Solo bandwidth saving
  setSoloBandwidthSaving: (saving: boolean) => void;
  setSoloBandwidthNotification: (show: boolean) => void;

  // Codec floor & active codecs
  setCodecFloor: (floor: string[] | null) => void;
  setActiveCameraCodec: (codec: string | null) => void;
  setActiveScreenCodec: (codec: string | null) => void;

  // Full reset (on disconnect)
  reset: () => void;
}

// Load persisted device settings from localStorage
const persisted = loadPersistedSettings();

const initialState = {
  activeChannelId: null as string | null,
  activeChannelName: null as string | null,
  activeServerId: null as string | null,
  connectionState: 'disconnected' as VoiceConnectionState,
  isMuted: false,
  isDeafened: false,
  isVideoOn: false,
  isScreenSharing: false,
  localIsTesting: false,
  participants: {} as Record<string, VoiceParticipant>,
  activeSpeakerId: null as string | null,
  audioInputDeviceId: persisted.audioInputDeviceId ?? null,
  audioOutputDeviceId: persisted.audioOutputDeviceId ?? null,
  videoDeviceId: persisted.videoDeviceId ?? null,
  qualityTier: persisted.qualityTier ?? 'standard',
  effectiveQualityTier: persisted.qualityTier ?? 'standard',
  packetLossPercent: 0,
  packetLossWarning: false,
  decoderHealth: 'green' as DecoderHealth,
  availableScreenShares: [] as AvailableScreenShare[],
  channelVoiceMembers: {} as Record<string, ChannelVoiceMember[]>,
  showVoiceTextChat: false,
  voiceTextChatHeight: (() => {
    try {
      const s = localStorage.getItem('concord:voice-text-chat-height');
      if (s) {
        const n = Number.parseInt(s, 10);
        if (!Number.isNaN(n) && n >= 150) return n;
      }
    } catch {
      /* ignore */
    }
    return 300;
  })(),
  voiceTextChatLayout: (() => {
    try {
      const s = localStorage.getItem('concord:voice-text-chat-layout');
      if (s === 'horizontal' || s === 'vertical') return s;
    } catch {
      /* ignore */
    }
    return 'horizontal';
    // S4325 false-positive: the assertion narrows the IIFE's inferred return
    // type from `string` (widened literal) to the `'horizontal' | 'vertical'`
    // union required by VoiceState — removing it breaks `tsc --noEmit`.
  })() as 'horizontal' | 'vertical',
  voiceTextChatWidth: (() => {
    try {
      const s = localStorage.getItem('concord:voice-text-chat-width');
      if (s) {
        const n = Number.parseInt(s, 10);
        if (!Number.isNaN(n) && n >= 250) return n;
      }
    } catch {
      /* ignore */
    }
    return 350;
  })(),
  serverVoiceCounts: {} as Record<string, number>,
  tunedInScreenShares: {} as Record<string, string>,
  dominantScreenShareId: null as string | null,
  maxVideoSlots: 50,
  videoSlotError: null as string | null,
  showUserFrameBar: true,
  showStreamBar: true,
  userFrameBarHeight: 120,
  streamBarHeight: 120,
  stageLayout: 'focus' as 'equal' | 'focus',
  pipWindows: [] as string[],
  keepActiveWhileUnfocused: (() => {
    try {
      return localStorage.getItem('concord:keep-active-unfocused') === 'true';
    } catch {
      return false;
    }
  })(),
  localStreamPaused: false,
  voiceControlsPinned: true,
  voiceControlsPoppedOut: false,
  persistentTextChatHeight: 250,
  isDMCall: false,
  dmConversationId: null as string | null,
  isGroupDM: false,
  callerDMRole: null as DMCallerRole,
  effectivePermissions: 0n,
  callState: { kind: 'idle' } as CallState,
  activeDMCalls: {} as Record<string, { participantIds: string[]; total: number }>,
  isSoloBandwidthSaving: false,
  soloBandwidthNotification: false,
  codecFloor: null as string[] | null,
  activeCameraCodec: null as string | null,
  activeScreenCodec: null as string | null,
};

export const useVoiceStore = createStore<VoiceState>()((set) => ({
  ...initialState,

  setConnectionState: (connectionState) => set({ connectionState }),
  setActiveChannel: (channelId, channelName, serverId) =>
    set({
      activeChannelId: channelId,
      activeChannelName: channelName ?? null,
      activeServerId: serverId ?? null,
    }),
  setMuted: (isMuted) => set({ isMuted }),
  setDeafened: (isDeafened) => set({ isDeafened }),
  setVideoOn: (isVideoOn) => set({ isVideoOn }),
  setScreenSharing: (isScreenSharing) => set({ isScreenSharing }),
  setLocalIsTesting: (localIsTesting) => set({ localIsTesting }),
  setActiveSpeaker: (activeSpeakerId) => set({ activeSpeakerId }),
  setAudioInputDevice: (audioInputDeviceId) => {
    set({ audioInputDeviceId });
    const s = useVoiceStore.getState();
    savePersistedSettings({
      audioInputDeviceId,
      audioOutputDeviceId: s.audioOutputDeviceId,
      videoDeviceId: s.videoDeviceId,
      qualityTier: s.qualityTier,
    });
  },
  setAudioOutputDevice: (audioOutputDeviceId) => {
    set({ audioOutputDeviceId });
    const s = useVoiceStore.getState();
    savePersistedSettings({
      audioInputDeviceId: s.audioInputDeviceId,
      audioOutputDeviceId,
      videoDeviceId: s.videoDeviceId,
      qualityTier: s.qualityTier,
    });
  },
  setVideoDevice: (videoDeviceId) => {
    set({ videoDeviceId });
    const s = useVoiceStore.getState();
    savePersistedSettings({
      audioInputDeviceId: s.audioInputDeviceId,
      audioOutputDeviceId: s.audioOutputDeviceId,
      videoDeviceId,
      qualityTier: s.qualityTier,
    });
  },
  setQualityTier: (qualityTier) => {
    set({ qualityTier });
    const s = useVoiceStore.getState();
    savePersistedSettings({
      audioInputDeviceId: s.audioInputDeviceId,
      audioOutputDeviceId: s.audioOutputDeviceId,
      videoDeviceId: s.videoDeviceId,
      qualityTier,
    });
  },
  setEffectiveQualityTier: (effectiveQualityTier) => set({ effectiveQualityTier }),
  setDecoderHealth: (decoderHealth) => set({ decoderHealth }),
  setPacketLoss: (percent, warningThreshold = 3) => {
    set({ packetLossPercent: percent, packetLossWarning: percent >= warningThreshold });
  },

  // Screen share opt-in
  addAvailableScreenShare: (share) =>
    set((state) => {
      // Avoid duplicates
      if (state.availableScreenShares.some((s) => s.producerId === share.producerId)) return state;
      return { availableScreenShares: [...state.availableScreenShares, share] };
    }),
  removeAvailableScreenShare: (producerId) =>
    set((state) => ({
      availableScreenShares: state.availableScreenShares.filter((s) => s.producerId !== producerId),
    })),
  clearAvailableScreenShares: () => set({ availableScreenShares: [] }),

  addParticipant: (participant) =>
    set((state) => ({
      participants: { ...state.participants, [participant.userId]: participant },
    })),
  removeParticipant: (userId) =>
    set((state) => {
      const { [userId]: _, ...rest } = state.participants;
      return { participants: rest };
    }),
  updateParticipant: (userId, update) =>
    set((state) => {
      const existing = state.participants[userId];
      if (!existing) return state;
      return {
        participants: {
          ...state.participants,
          [userId]: { ...existing, ...update },
        },
      };
    }),
  setParticipants: (participants) =>
    set({
      participants: Object.fromEntries(participants.map((p) => [p.userId, p])),
    }),
  clearParticipants: () => set({ participants: {} }),

  // Channel voice members (sidebar)
  setChannelVoiceMembers: (channelId, members) =>
    set((state) => ({
      channelVoiceMembers: { ...state.channelVoiceMembers, [channelId]: members },
    })),
  addChannelVoiceMember: (channelId, member) =>
    set((state) => {
      const existing = state.channelVoiceMembers[channelId] || [];
      // Avoid duplicates
      if (existing.some((m) => m.userId === member.userId)) return state;
      return {
        channelVoiceMembers: {
          ...state.channelVoiceMembers,
          [channelId]: [...existing, member],
        },
      };
    }),
  removeChannelVoiceMember: (channelId, userId) =>
    set((state) => {
      const existing = state.channelVoiceMembers[channelId];
      if (!existing) return state;
      const filtered = existing.filter((m) => m.userId !== userId);
      if (filtered.length === 0) {
        const { [channelId]: _, ...rest } = state.channelVoiceMembers;
        return { channelVoiceMembers: rest };
      }
      return {
        channelVoiceMembers: { ...state.channelVoiceMembers, [channelId]: filtered },
      };
    }),
  updateChannelVoiceMember: (channelId, userId, update) =>
    set((state) => {
      const existing = state.channelVoiceMembers[channelId];
      if (!existing) return state;
      return {
        channelVoiceMembers: {
          ...state.channelVoiceMembers,
          [channelId]: existing.map((m) => (m.userId === userId ? { ...m, ...update } : m)),
        },
      };
    }),
  clearAllChannelVoiceMembers: () => set({ channelVoiceMembers: {} }),

  // Voice text chat
  setShowVoiceTextChat: (show) => set({ showVoiceTextChat: show }),
  toggleVoiceTextChat: () => set((state) => ({ showVoiceTextChat: !state.showVoiceTextChat })),
  setVoiceTextChatHeight: (height) => {
    set({ voiceTextChatHeight: height });
    try {
      localStorage.setItem('concord:voice-text-chat-height', String(height));
    } catch {
      /* ignore */
    }
  },
  setVoiceTextChatLayout: (layout) => {
    set({ voiceTextChatLayout: layout });
    try {
      localStorage.setItem('concord:voice-text-chat-layout', layout);
    } catch {
      /* ignore */
    }
  },
  toggleVoiceTextChatLayout: () =>
    set((state) => {
      const next = state.voiceTextChatLayout === 'horizontal' ? 'vertical' : 'horizontal';
      try {
        localStorage.setItem('concord:voice-text-chat-layout', next);
      } catch {
        /* ignore */
      }
      return { voiceTextChatLayout: next };
    }),
  setVoiceTextChatWidth: (width) => {
    set({ voiceTextChatWidth: width });
    try {
      localStorage.setItem('concord:voice-text-chat-width', String(width));
    } catch {
      /* ignore */
    }
  },

  // Server voice counts
  setServerVoiceCounts: (counts) => set({ serverVoiceCounts: counts }),
  setServerVoiceCount: (serverId, count) =>
    set((state) => ({
      serverVoiceCounts: { ...state.serverVoiceCounts, [serverId]: Math.max(0, count) },
    })),
  incrementServerVoiceCount: (serverId) =>
    set((state) => ({
      serverVoiceCounts: {
        ...state.serverVoiceCounts,
        [serverId]: (state.serverVoiceCounts[serverId] ?? 0) + 1,
      },
    })),
  decrementServerVoiceCount: (serverId) =>
    set((state) => ({
      serverVoiceCounts: {
        ...state.serverVoiceCounts,
        [serverId]: Math.max(0, (state.serverVoiceCounts[serverId] ?? 0) - 1),
      },
    })),

  // Multi-stream screen share
  tuneIn: (producerId, consumerId) =>
    set((state) => {
      const next = { ...state.tunedInScreenShares, [producerId]: consumerId };
      const count = Object.keys(next).length;
      return {
        tunedInScreenShares: next,
        maxVideoSlots: 50 - 5 * count,
      };
    }),
  tuneOut: (producerId) =>
    set((state) => {
      const { [producerId]: _, ...rest } = state.tunedInScreenShares;
      const count = Object.keys(rest).length;
      const dominant =
        state.dominantScreenShareId === producerId
          ? (Object.keys(rest)[0] ?? null)
          : state.dominantScreenShareId;
      return {
        tunedInScreenShares: rest,
        dominantScreenShareId: dominant,
        maxVideoSlots: 50 - 5 * count,
      };
    }),
  setDominantScreenShare: (dominantScreenShareId) => set({ dominantScreenShareId }),
  recalculateMaxVideoSlots: () =>
    set((state) => ({
      maxVideoSlots: 50 - 5 * Object.keys(state.tunedInScreenShares).length,
    })),
  setVideoSlotError: (videoSlotError) => set({ videoSlotError }),

  // Layout sections (Mode B)
  toggleUserFrameBar: () => set((state) => ({ showUserFrameBar: !state.showUserFrameBar })),
  toggleStreamBar: () => set((state) => ({ showStreamBar: !state.showStreamBar })),
  setUserFrameBarHeight: (userFrameBarHeight) => set({ userFrameBarHeight }),
  setStreamBarHeight: (streamBarHeight) => set({ streamBarHeight }),
  setStageLayout: (stageLayout) => set({ stageLayout }),
  toggleStageLayout: () =>
    set((state) => ({ stageLayout: state.stageLayout === 'equal' ? 'focus' : 'equal' })),

  // PiP
  addPipWindow: (id) =>
    set((state) => ({
      pipWindows: state.pipWindows.includes(id) ? state.pipWindows : [...state.pipWindows, id],
    })),
  removePipWindow: (id) =>
    set((state) => ({
      pipWindows: state.pipWindows.filter((w) => w !== id),
    })),

  // Stream focus behavior
  setKeepActiveWhileUnfocused: (keep) => {
    set({ keepActiveWhileUnfocused: keep });
    try {
      localStorage.setItem('concord:keep-active-unfocused', String(keep));
    } catch {
      /* ignore */
    }
  },
  setLocalStreamPaused: (localStreamPaused) => set({ localStreamPaused }),

  setVoiceControlsPinned: (voiceControlsPinned) => set({ voiceControlsPinned }),
  toggleVoiceControlsPinned: () =>
    set((state) => ({ voiceControlsPinned: !state.voiceControlsPinned })),
  setVoiceControlsPoppedOut: (voiceControlsPoppedOut) => set({ voiceControlsPoppedOut }),
  setPersistentTextChatHeight: (persistentTextChatHeight) => set({ persistentTextChatHeight }),

  setDMCall: (isDMCall, conversationId) =>
    set({
      isDMCall,
      dmConversationId: isDMCall ? (conversationId ?? null) : null,
    }),

  setGroupDMInfo: (isGroupDM, callerDMRole) => set({ isGroupDM, callerDMRole }),
  setEffectivePermissions: (effectivePermissions) => set({ effectivePermissions }),

  setCallState: (callState) => set({ callState }),

  // Active DM-call roster reducer (#1219 R4). Reduces live
  // `dm_voice_state_update` deltas into the per-conversation roster.
  applyDMVoiceState: (conversationId, action, userId, total) =>
    set((state) => {
      const next = { ...state.activeDMCalls };
      if (action === 'room_empty') {
        delete next[conversationId];
        return { activeDMCalls: next };
      }
      // joined/left both require a userId — guard the optional field before
      // mutating (the schema marks user_id optional for some voice actions).
      if (!userId) return {};
      const cur = next[conversationId]?.participantIds ?? [];
      if (action === 'joined') {
        next[conversationId] = {
          participantIds: cur.includes(userId) ? cur : [...cur, userId],
          total,
        };
      } else if (action === 'left') {
        const remaining = cur.filter((id) => id !== userId);
        if (remaining.length === 0) delete next[conversationId];
        else next[conversationId] = { participantIds: remaining, total };
      } else {
        // Non-roster actions (mute/video/etc.) — no roster change.
        return {};
      }
      return { activeDMCalls: next };
    }),
  seedActiveDMCall: (conversationId, participantIds, total) =>
    set((state) => ({
      activeDMCalls: {
        ...state.activeDMCalls,
        [conversationId]: { participantIds, total },
      },
    })),
  clearActiveDMCall: (conversationId) =>
    set((state) => {
      const next = { ...state.activeDMCalls };
      delete next[conversationId];
      return { activeDMCalls: next };
    }),

  setSoloBandwidthSaving: (isSoloBandwidthSaving) => set({ isSoloBandwidthSaving }),
  setSoloBandwidthNotification: (soloBandwidthNotification) => set({ soloBandwidthNotification }),

  setCodecFloor: (codecFloor) => set({ codecFloor }),
  setActiveCameraCodec: (activeCameraCodec) => set({ activeCameraCodec }),
  setActiveScreenCodec: (activeScreenCodec) => set({ activeScreenCodec }),

  reset: () =>
    set((state) => ({
      ...initialState,
      // Preserve server-wide voice member data (sidebar display) across join/leave
      channelVoiceMembers: state.channelVoiceMembers,
      // Preserve device settings across join/leave (they're persisted to localStorage)
      audioInputDeviceId: state.audioInputDeviceId,
      audioOutputDeviceId: state.audioOutputDeviceId,
      videoDeviceId: state.videoDeviceId,
      qualityTier: state.qualityTier,
      // Preserve text chat preferences
      voiceTextChatHeight: state.voiceTextChatHeight,
      voiceTextChatLayout: state.voiceTextChatLayout,
      voiceTextChatWidth: state.voiceTextChatWidth,
      // Preserve server voice counts (cross-server state)
      serverVoiceCounts: state.serverVoiceCounts,
      // Preserve section height & layout preferences
      userFrameBarHeight: state.userFrameBarHeight,
      streamBarHeight: state.streamBarHeight,
      stageLayout: state.stageLayout,
      // Preserve stream focus preference
      keepActiveWhileUnfocused: state.keepActiveWhileUnfocused,
      // Preserve persistent bar text chat height
      persistentTextChatHeight: state.persistentTextChatHeight,
    })),
}));
