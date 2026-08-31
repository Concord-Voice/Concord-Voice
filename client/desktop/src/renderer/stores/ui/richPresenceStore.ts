import { create, type StoreApi } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import { apiFetch } from '../../services/system/apiClient';
import {
  captureRuntimeServerSelection,
  onRuntimeServerSelectionChange,
  runtimeServerSelectionIsCurrent,
} from '../../services/system/runtimeServerBase';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
  type AuthLifecycleSnapshot,
} from '../../services/system/postLoginHydrationLifecycle';
import { useAuthStore } from '../auth/authStore';
import type { VoiceConnectionState } from '../voice/voiceStore';
import type { CustomTextPresencePayload, RichPresenceEntry } from '../../types/ws-events';
import { z } from 'zod';

export type CustomTextStatus = CustomTextPresencePayload;
export type RichPresenceCategory = RichPresenceEntry['category'];

type StoredCustomTextEntry = Omit<
  Extract<RichPresenceEntry, { category: 'custom_text' }>,
  'updated_at'
> & {
  updated_at?: number;
};
export type StoredRichPresenceEntry =
  Exclude<RichPresenceEntry, { category: 'custom_text' }> | StoredCustomTextEntry;

export type OtherPresenceByUser = Record<
  string,
  Partial<Record<RichPresenceCategory, StoredRichPresenceEntry>>
>;

export interface SelfPresence {
  tier: number;
  customText?: string;
  customTextEmoji?: string;
}

export type PresenceTier = 0 | 1 | 2;

export interface PresenceSettings {
  masterEnabled: boolean;
  serverVoiceTier: PresenceTier;
  serverVoiceShowDetails: boolean;
  privateCallTier: PresenceTier;
  privateCallShowDetails: boolean;
  customTextTier: PresenceTier;
  customText?: string;
  customTextEmoji?: string;
}

export type PresenceSettingsUpdate = Pick<
  PresenceSettings,
  | 'masterEnabled'
  | 'serverVoiceTier'
  | 'serverVoiceShowDetails'
  | 'privateCallTier'
  | 'privateCallShowDetails'
  | 'customTextTier'
>;

const PRESENCE_SETTINGS_ENDPOINT = '/api/v1/users/me/presence-settings';
const INITIAL_PRESENCE_SETTINGS: PresenceSettings = {
  masterEnabled: true,
  serverVoiceTier: 1,
  serverVoiceShowDetails: true,
  privateCallTier: 0,
  privateCallShowDetails: false,
  customTextTier: 0,
};

const presenceSettingsWireSchema = z.object({
  master_enabled: z.boolean(),
  server_voice_tier: z.union([z.literal(0), z.literal(1), z.literal(2)]),
  server_voice_show_details: z.boolean(),
  private_call_tier: z.union([z.literal(0), z.literal(1), z.literal(2)]),
  private_call_show_details: z.boolean(),
  custom_text_tier: z.union([z.literal(0), z.literal(1), z.literal(2)]),
  custom_text: z
    .string()
    .refine((value) => [...value].length <= 140)
    .nullable(),
  custom_text_emoji: z
    .string()
    .refine((value) => [...value].length <= 32)
    .nullable(),
});

const PRESENCE_SETTINGS_WIRE_FIELDS: Record<keyof PresenceSettingsUpdate, string> = {
  masterEnabled: 'master_enabled',
  serverVoiceTier: 'server_voice_tier',
  serverVoiceShowDetails: 'server_voice_show_details',
  privateCallTier: 'private_call_tier',
  privateCallShowDetails: 'private_call_show_details',
  customTextTier: 'custom_text_tier',
};

function preparePresenceSettingsUpdateBody(
  patch: Partial<PresenceSettingsUpdate>,
  current: PresenceSettings,
  presenceSettingsSaving: boolean,
  previousConfirmed: PresenceSettings | null
): { body: Record<string, unknown>; previousConfirmed: PresenceSettings } | null {
  if (presenceSettingsSaving || previousConfirmed === null) return null;
  const body: Record<string, unknown> = {};
  for (const [field, wireField] of Object.entries(PRESENCE_SETTINGS_WIRE_FIELDS)) {
    const typedField = field as keyof PresenceSettingsUpdate;
    const value = patch[typedField];
    if (value !== undefined && value !== current[typedField]) {
      body[wireField] = value;
    }
  }
  return Object.keys(body).length === 0 ? null : { body, previousConfirmed };
}

type PresenceSettingsWire = z.infer<typeof presenceSettingsWireSchema>;

function normalizePresenceSettings(data: PresenceSettingsWire): PresenceSettings {
  return {
    masterEnabled: data.master_enabled,
    serverVoiceTier: data.server_voice_tier,
    serverVoiceShowDetails: data.server_voice_show_details,
    privateCallTier: data.private_call_tier,
    privateCallShowDetails: data.private_call_show_details,
    customTextTier: data.custom_text_tier,
    ...(data.custom_text === null ? {} : { customText: data.custom_text }),
    ...(data.custom_text_emoji === null ? {} : { customTextEmoji: data.custom_text_emoji }),
  };
}

/** Validates every presence-settings field before state is committed. */
export function parsePresenceSettingsResponse(data: unknown): PresenceSettings | null {
  const parsed = presenceSettingsWireSchema.safeParse(data);
  return parsed.success ? normalizePresenceSettings(parsed.data) : null;
}

function projectSelfPresence(settings: PresenceSettings): SelfPresence {
  return {
    tier: settings.customTextTier,
    ...(settings.customText === undefined ? {} : { customText: settings.customText }),
    ...(settings.customTextEmoji === undefined
      ? {}
      : { customTextEmoji: settings.customTextEmoji }),
  };
}

function errorMessage(error: unknown, fallback: string): string {
  if (error instanceof Error && error.message) return error.message;
  return fallback;
}

export async function presenceSettingsResponseError(
  response: Response,
  fallback: string
): Promise<Error> {
  const body: unknown = await response.json().catch(() => undefined);
  if (typeof body === 'object' && body !== null && 'error' in body) {
    const message = (body as { error?: unknown }).error;
    if (typeof message === 'string' && message) return new Error(message);
  }
  return new Error(fallback);
}

export function isDefinitePresenceSettingsPatchRejection(response: Response): boolean {
  return response.status >= 400 && response.status < 500;
}

interface PresenceSettingsRequestTicket {
  identity: number;
  request: number;
  mutation: number;
  customTextRevision: number;
  customStatusSuccessRevision: number;
  auth: AuthLifecycleSnapshot;
  runtime: ReturnType<typeof captureRuntimeServerSelection>;
}

/** Snapshot for a custom-status PATCH; unlike a settings request it does not claim request order. */
export interface CustomStatusSubmissionTicket {
  identity: number;
  operation: number;
  activityRevision: number;
  customTextRevision: number;
  auth: AuthLifecycleSnapshot;
  runtime: ReturnType<typeof captureRuntimeServerSelection>;
}

export interface CustomStatusSubmissionResult {
  contextCurrent: boolean;
  activityCurrent: boolean;
  customCurrent: boolean;
}

export const CUSTOM_STATUS_RECONCILIATION_ERROR =
  'Settings changed while saving your status. Reload settings to continue.';

let presenceSettingsIdentity = 0;
let presenceSettingsRequest = 0;
let presenceSettingsMutation = 0;
let presenceSettingsActivityRevision = 0;
let customTextRevision = 0;
let customStatusSuccessRevision = 0;
let customStatusSubmissionOperation = 0;
let activeCustomStatusSubmissionOperation: number | null = null;

function nextPresenceSettingsTicket(
  customTextRevisionBaseline = customTextRevision
): PresenceSettingsRequestTicket {
  return {
    identity: presenceSettingsIdentity,
    request: ++presenceSettingsRequest,
    mutation: presenceSettingsMutation,
    customTextRevision: customTextRevisionBaseline,
    customStatusSuccessRevision,
    auth: captureAuthLifecycle(),
    runtime: captureRuntimeServerSelection(),
  };
}

function nextCustomStatusSubmissionTicket(): CustomStatusSubmissionTicket | null {
  if (activeCustomStatusSubmissionOperation !== null) return null;
  const operation = ++customStatusSubmissionOperation;
  activeCustomStatusSubmissionOperation = operation;
  return {
    identity: presenceSettingsIdentity,
    operation,
    activityRevision: ++presenceSettingsActivityRevision,
    customTextRevision: ++customTextRevision,
    auth: captureAuthLifecycle(),
    runtime: captureRuntimeServerSelection(),
  };
}

function customStatusSubmissionContextIsCurrent(ticket: CustomStatusSubmissionTicket): boolean {
  return (
    ticket.identity === presenceSettingsIdentity &&
    ticket.operation === activeCustomStatusSubmissionOperation &&
    isSameAuthLifecycle(ticket.auth) &&
    runtimeServerSelectionIsCurrent(ticket.runtime)
  );
}

function customStatusSubmissionResult(
  ticket: CustomStatusSubmissionTicket,
  presenceSettingsSaving: boolean
): CustomStatusSubmissionResult {
  const contextCurrent = customStatusSubmissionContextIsCurrent(ticket);
  return {
    contextCurrent,
    activityCurrent:
      contextCurrent &&
      ticket.activityRevision === presenceSettingsActivityRevision &&
      !presenceSettingsSaving,
    customCurrent: contextCurrent && ticket.customTextRevision === customTextRevision,
  };
}

function revalidateCustomStatusSubmissionAfterApply(
  ticket: CustomStatusSubmissionTicket,
  result: CustomStatusSubmissionResult,
  presenceSettingsSaving: boolean
): CustomStatusSubmissionResult {
  const current = customStatusSubmissionResult(ticket, presenceSettingsSaving);
  return {
    contextCurrent: result.contextCurrent && current.contextCurrent,
    activityCurrent: result.activityCurrent && current.activityCurrent,
    customCurrent: result.customCurrent && current.customCurrent,
  };
}

function withCustomText(
  settings: PresenceSettings,
  source: Pick<SelfPresence, 'customText' | 'customTextEmoji'>
): PresenceSettings {
  const { customText: _customText, customTextEmoji: _customTextEmoji, ...rest } = settings;
  return {
    ...rest,
    ...(source.customText === undefined ? {} : { customText: source.customText }),
    ...(source.customTextEmoji === undefined ? {} : { customTextEmoji: source.customTextEmoji }),
  };
}

function mergeRequestSettings(
  settings: PresenceSettings,
  ticket: PresenceSettingsRequestTicket,
  current: PresenceSettings
): PresenceSettings {
  return ticket.customTextRevision === customTextRevision
    ? settings
    : withCustomText(settings, current);
}

function mergeCustomStatusSettings(
  settings: PresenceSettings,
  result: CustomStatusSubmissionResult,
  current: PresenceSettings
): PresenceSettings {
  const activity = result.activityCurrent ? settings : withCustomText(current, settings);
  return result.customCurrent ? activity : withCustomText(activity, current);
}

function isCurrentPresenceSettingsTicket(ticket: PresenceSettingsRequestTicket): boolean {
  return (
    ticket.identity === presenceSettingsIdentity &&
    ticket.request === presenceSettingsRequest &&
    ticket.mutation === presenceSettingsMutation &&
    isSameAuthLifecycle(ticket.auth) &&
    runtimeServerSelectionIsCurrent(ticket.runtime)
  );
}

function resetPresenceSettingsState(preserveSelf: boolean, preserveRemote = false): void {
  presenceSettingsIdentity += 1;
  presenceSettingsRequest += 1;
  presenceSettingsMutation += 1;
  activeCustomStatusSubmissionOperation = null;
  useRichPresenceStore.setState({
    presenceSettings: { ...INITIAL_PRESENCE_SETTINGS },
    confirmedPresenceSettings: null,
    presenceSettingsLoading: false,
    presenceSettingsSaving: false,
    customStatusSaving: false,
    presenceSettingsError: null,
    ...(preserveSelf ? {} : { self: { ...INITIAL_SELF } }),
    ...(preserveRemote ? {} : { otherByUser: {} }),
  });
}

interface RichPresenceState {
  otherByUser: OtherPresenceByUser;
  self: SelfPresence;
  setOtherPresence: (userId: string, entry: StoredRichPresenceEntry) => void;
  clearOtherPresence: (userId: string, category: RichPresenceCategory) => void;
  replaceOtherPresence: (next: OtherPresenceByUser) => void;
  clearAllOtherPresence: () => void;
  getPresence: (
    userId: string,
    category: RichPresenceCategory
  ) => StoredRichPresenceEntry | undefined;
  setCustomText: (userId: string, status: CustomTextStatus) => void;
  clearCustomText: (userId: string) => void;
  getCustomText: (userId: string) => CustomTextStatus | undefined;
  setSelfPresence: (updates: Partial<SelfPresence>) => void;
  captureCustomStatusSubmission: () => CustomStatusSubmissionTicket | null;
  releaseCustomStatusSubmission: (ticket: CustomStatusSubmissionTicket) => void;
  getCustomStatusSubmissionResult: (
    ticket: CustomStatusSubmissionTicket
  ) => CustomStatusSubmissionResult;
  applyCustomStatusSettings: (
    settings: PresenceSettings,
    ticket: CustomStatusSubmissionTicket
  ) => CustomStatusSubmissionResult;
  reconcileCustomStatusAmbiguousOutcome: (
    ticket: CustomStatusSubmissionTicket
  ) => Promise<CustomStatusSubmissionResult>;
  presenceSettings: PresenceSettings;
  confirmedPresenceSettings: PresenceSettings | null;
  presenceSettingsLoading: boolean;
  presenceSettingsSaving: boolean;
  customStatusSaving: boolean;
  presenceSettingsError: string | null;
  hydratePresenceSettings: (customTextRevisionBaseline?: number) => Promise<void>;
  updatePresenceSettings: (patch: Partial<PresenceSettingsUpdate>) => Promise<void>;
  reset: () => void;
}

const INITIAL_SELF: SelfPresence = { tier: 0 };

function rollbackPresenceSettingsUpdate(
  set: StoreApi<RichPresenceState>['setState'],
  get: StoreApi<RichPresenceState>['getState'],
  previousConfirmed: PresenceSettings,
  ticket: PresenceSettingsRequestTicket
): boolean {
  const preserveUnconfirmed = get().confirmedPresenceSettings === null;
  const settings = mergeRequestSettings(previousConfirmed, ticket, get().presenceSettings);
  const preserveCustomStatusReconciliation =
    ticket.customStatusSuccessRevision !== customStatusSuccessRevision;
  presenceSettingsActivityRevision += 1;
  set({
    presenceSettings: settings,
    confirmedPresenceSettings: preserveUnconfirmed ? null : settings,
    presenceSettingsLoading: false,
    presenceSettingsSaving: false,
    presenceSettingsError: preserveCustomStatusReconciliation
      ? CUSTOM_STATUS_RECONCILIATION_ERROR
      : null,
    self: projectSelfPresence(settings),
  });
  return preserveCustomStatusReconciliation;
}

async function reconcileAmbiguousPresenceSettingsUpdate(
  set: StoreApi<RichPresenceState>['setState'],
  get: StoreApi<RichPresenceState>['getState'],
  previousConfirmed: PresenceSettings,
  ticket: PresenceSettingsRequestTicket,
  error: unknown
): Promise<void> {
  if (!isCurrentPresenceSettingsTicket(ticket)) return;
  rollbackPresenceSettingsUpdate(set, get, previousConfirmed, ticket);
  if (!isCurrentPresenceSettingsTicket(ticket)) return;
  set({
    confirmedPresenceSettings: null,
    presenceSettingsError: errorMessage(error, 'Failed to update Rich Presence settings'),
  });
  if (!isCurrentPresenceSettingsTicket(ticket)) return;
  await get().hydratePresenceSettings(ticket.customTextRevision);
}

async function handlePresenceSettingsPatchRejection(
  response: Response,
  set: StoreApi<RichPresenceState>['setState'],
  get: StoreApi<RichPresenceState>['getState'],
  previousConfirmed: PresenceSettings,
  ticket: PresenceSettingsRequestTicket
): Promise<void> {
  const error = await presenceSettingsResponseError(
    response,
    'Failed to update Rich Presence settings'
  );
  if (!isCurrentPresenceSettingsTicket(ticket)) return;
  if (!isDefinitePresenceSettingsPatchRejection(response)) {
    await reconcileAmbiguousPresenceSettingsUpdate(set, get, previousConfirmed, ticket, error);
    return;
  }
  const preserveCustomStatusReconciliation = rollbackPresenceSettingsUpdate(
    set,
    get,
    previousConfirmed,
    ticket
  );
  if (!isCurrentPresenceSettingsTicket(ticket)) return;
  set({
    presenceSettingsError: preserveCustomStatusReconciliation
      ? CUSTOM_STATUS_RECONCILIATION_ERROR
      : error.message,
  });
}

export interface LocalRichPresenceVoiceState {
  activeChannelId: string | null;
  activeChannelName: string | null;
  activeServerId: string | null;
  connectionState: VoiceConnectionState;
  isDMCall: boolean;
  isGroupDM: boolean;
  callState: { kind: string };
  participants: Record<string, unknown>;
}

export type LocalRichPresenceActivity =
  | {
      category: 'server_voice';
      channelId: string;
      channelName: string;
      serverId: string;
    }
  | {
      category: 'private_call';
      callType: 'dm' | 'group';
      participantCount?: number;
    };

export function selectLocalRichPresenceActivity(
  voiceState: LocalRichPresenceVoiceState
): LocalRichPresenceActivity | null {
  if (voiceState.connectionState !== 'connected') return null;

  if (voiceState.isDMCall) {
    if (voiceState.callState.kind !== 'in-call') return null;
    const activity: LocalRichPresenceActivity = {
      category: 'private_call',
      callType: voiceState.isGroupDM ? 'group' : 'dm',
    };
    const participantCount = Object.keys(voiceState.participants).length;
    return voiceState.isGroupDM && participantCount > 0
      ? { ...activity, participantCount }
      : activity;
  }

  if (voiceState.callState.kind !== 'idle' && voiceState.callState.kind !== 'in-call') return null;
  if (!voiceState.activeChannelId || !voiceState.activeChannelName || !voiceState.activeServerId) {
    return null;
  }
  return {
    category: 'server_voice',
    channelId: voiceState.activeChannelId,
    channelName: voiceState.activeChannelName,
    serverId: voiceState.activeServerId,
  };
}

export const selectCustomText =
  (userId: string) =>
  (state: RichPresenceState): CustomTextStatus | undefined => {
    const entry = state.otherByUser[userId]?.custom_text;
    return entry?.category === 'custom_text' ? entry.payload : undefined;
  };

export const useRichPresenceStore = wrapStore(
  create<RichPresenceState>()(
    devtools(
      (set, get) => ({
        otherByUser: {},
        self: { ...INITIAL_SELF },
        presenceSettings: { ...INITIAL_PRESENCE_SETTINGS },
        confirmedPresenceSettings: null,
        presenceSettingsLoading: false,
        presenceSettingsSaving: false,
        customStatusSaving: false,
        presenceSettingsError: null,

        setOtherPresence: (userId, entry) => {
          set((state) => ({
            otherByUser: {
              ...state.otherByUser,
              [userId]: { ...state.otherByUser[userId], [entry.category]: entry },
            },
          }));
        },

        clearOtherPresence: (userId, category) => {
          set((state) => {
            const current = state.otherByUser[userId];
            if (!current || !(category in current)) return state;
            const nextUser = { ...current };
            delete nextUser[category];
            const next = { ...state.otherByUser };
            if (Object.keys(nextUser).length === 0) delete next[userId];
            else next[userId] = nextUser;
            return { otherByUser: next };
          });
        },

        replaceOtherPresence: (next) => set({ otherByUser: next }),
        clearAllOtherPresence: () => set({ otherByUser: {} }),

        getPresence: (userId, category) => get().otherByUser[userId]?.[category],

        setCustomText: (userId, status) =>
          get().setOtherPresence(userId, { category: 'custom_text', payload: status }),

        clearCustomText: (userId) => get().clearOtherPresence(userId, 'custom_text'),

        getCustomText: (userId) => selectCustomText(userId)(get()),

        setSelfPresence: (updates) => {
          const hasCustomTextUpdate =
            Object.hasOwn(updates, 'customText') || Object.hasOwn(updates, 'customTextEmoji');
          if (hasCustomTextUpdate) customTextRevision += 1;
          set((state) => {
            const self = { ...state.self, ...updates };
            return {
              self,
              ...(hasCustomTextUpdate
                ? {
                    presenceSettings: withCustomText(state.presenceSettings, self),
                    ...(state.confirmedPresenceSettings === null
                      ? {}
                      : {
                          confirmedPresenceSettings: withCustomText(
                            state.confirmedPresenceSettings,
                            self
                          ),
                        }),
                  }
                : {}),
            };
          });
        },

        captureCustomStatusSubmission: () => {
          const { confirmedPresenceSettings, presenceSettingsError, presenceSettingsLoading } =
            get();
          if (
            presenceSettingsLoading ||
            (confirmedPresenceSettings === null && presenceSettingsError !== null)
          ) {
            return null;
          }
          const ticket = nextCustomStatusSubmissionTicket();
          if (ticket === null) return null;
          set({ customStatusSaving: true });
          const current = customStatusSubmissionResult(ticket, get().presenceSettingsSaving);
          if (!current.contextCurrent || !current.customCurrent) {
            get().releaseCustomStatusSubmission(ticket);
            return null;
          }
          return ticket;
        },

        releaseCustomStatusSubmission: (ticket) => {
          if (
            ticket.identity !== presenceSettingsIdentity ||
            ticket.operation !== activeCustomStatusSubmissionOperation
          ) {
            return;
          }
          activeCustomStatusSubmissionOperation = null;
          set({ customStatusSaving: false });
        },

        getCustomStatusSubmissionResult: (ticket) =>
          customStatusSubmissionResult(ticket, get().presenceSettingsSaving),

        applyCustomStatusSettings: (settings, ticket) => {
          const result = customStatusSubmissionResult(ticket, get().presenceSettingsSaving);
          if (!result.contextCurrent) return result;
          if (!result.activityCurrent && !result.customCurrent) {
            set({
              confirmedPresenceSettings: null,
              presenceSettingsError: CUSTOM_STATUS_RECONCILIATION_ERROR,
            });
            return result;
          }
          const committed = mergeCustomStatusSettings(settings, result, get().presenceSettings);
          const confirmed = result.activityCurrent && result.customCurrent;
          if (result.activityCurrent) ticket.activityRevision = ++presenceSettingsActivityRevision;
          if (result.customCurrent) {
            ticket.customTextRevision = ++customTextRevision;
            customStatusSuccessRevision += 1;
          }
          set({
            presenceSettings: committed,
            confirmedPresenceSettings: confirmed ? committed : null,
            presenceSettingsError: confirmed ? null : CUSTOM_STATUS_RECONCILIATION_ERROR,
            self: projectSelfPresence(committed),
          });
          const postApplyResult = revalidateCustomStatusSubmissionAfterApply(
            ticket,
            result,
            get().presenceSettingsSaving
          );
          if (
            postApplyResult.contextCurrent &&
            (!postApplyResult.activityCurrent || !postApplyResult.customCurrent)
          ) {
            set({
              confirmedPresenceSettings: null,
              presenceSettingsError: CUSTOM_STATUS_RECONCILIATION_ERROR,
            });
          }
          return postApplyResult;
        },

        reconcileCustomStatusAmbiguousOutcome: async (ticket) => {
          const result = customStatusSubmissionResult(ticket, get().presenceSettingsSaving);
          if (!result.contextCurrent) return result;
          if (!result.activityCurrent && !result.customCurrent) {
            set({
              confirmedPresenceSettings: null,
              presenceSettingsError: CUSTOM_STATUS_RECONCILIATION_ERROR,
            });
            return result;
          }
          set({ confirmedPresenceSettings: null });
          try {
            const response = await apiFetch(PRESENCE_SETTINGS_ENDPOINT);
            if (!response.ok) {
              throw await presenceSettingsResponseError(
                response,
                'Failed to load Rich Presence settings'
              );
            }
            const raw: unknown = await response.json();
            const settings = parsePresenceSettingsResponse(raw);
            if (settings === null) throw new Error('Received invalid Rich Presence settings');
            return get().applyCustomStatusSettings(settings, ticket);
          } catch (error) {
            const current = customStatusSubmissionResult(ticket, get().presenceSettingsSaving);
            if (!current.contextCurrent) return current;
            if (!current.activityCurrent && !current.customCurrent) {
              set({
                confirmedPresenceSettings: null,
                presenceSettingsError: CUSTOM_STATUS_RECONCILIATION_ERROR,
              });
              return current;
            }
            set({
              confirmedPresenceSettings: null,
              presenceSettingsError: errorMessage(error, 'Failed to load Rich Presence settings'),
            });
            return current;
          }
        },

        hydratePresenceSettings: async (customTextRevisionBaseline) => {
          if (get().presenceSettingsSaving) return;
          const ticket = nextPresenceSettingsTicket(customTextRevisionBaseline);
          set({ presenceSettingsLoading: true, presenceSettingsError: null });
          try {
            const response = await apiFetch(PRESENCE_SETTINGS_ENDPOINT);
            if (!response.ok) {
              throw await presenceSettingsResponseError(
                response,
                'Failed to load Rich Presence settings'
              );
            }
            const raw: unknown = await response.json();
            const parsed = parsePresenceSettingsResponse(raw);
            if (parsed === null) {
              throw new Error('Received invalid Rich Presence settings');
            }
            if (!isCurrentPresenceSettingsTicket(ticket)) return;
            if (get().customStatusSaving) {
              const settings = withCustomText(parsed, get().presenceSettings);
              presenceSettingsActivityRevision += 1;
              set({
                presenceSettings: settings,
                confirmedPresenceSettings: null,
                presenceSettingsLoading: false,
                presenceSettingsError: CUSTOM_STATUS_RECONCILIATION_ERROR,
                self: projectSelfPresence(settings),
              });
              return;
            }
            const settings = mergeRequestSettings(parsed, ticket, get().presenceSettings);
            const adoptsCustom = ticket.customTextRevision === customTextRevision;
            presenceSettingsActivityRevision += 1;
            if (adoptsCustom) customTextRevision += 1;
            set({
              presenceSettings: settings,
              confirmedPresenceSettings: settings,
              presenceSettingsLoading: false,
              presenceSettingsError: null,
              self: projectSelfPresence(settings),
            });
          } catch (error) {
            if (!isCurrentPresenceSettingsTicket(ticket)) return;
            set({
              confirmedPresenceSettings: null,
              presenceSettingsLoading: false,
              presenceSettingsError: errorMessage(error, 'Failed to load Rich Presence settings'),
            });
          }
        },

        updatePresenceSettings: async (patch) => {
          const {
            presenceSettingsSaving,
            confirmedPresenceSettings: previousConfirmedSnapshot,
            presenceSettings: current,
          } = get();
          const prepared = preparePresenceSettingsUpdateBody(
            patch,
            current,
            presenceSettingsSaving,
            previousConfirmedSnapshot
          );
          if (prepared === null) return;
          const { body, previousConfirmed } = prepared;

          presenceSettingsMutation += 1;
          presenceSettingsActivityRevision += 1;
          const ticket = nextPresenceSettingsTicket();
          const optimistic = { ...current, ...patch };
          set({
            presenceSettings: optimistic,
            presenceSettingsLoading: false,
            presenceSettingsSaving: true,
            presenceSettingsError: null,
          });

          let response: Response;
          try {
            response = await apiFetch(PRESENCE_SETTINGS_ENDPOINT, {
              method: 'PATCH',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(body),
            });
          } catch (error) {
            await reconcileAmbiguousPresenceSettingsUpdate(
              set,
              get,
              previousConfirmed,
              ticket,
              error
            );
            return;
          }
          if (!isCurrentPresenceSettingsTicket(ticket)) return;
          if (!response.ok) {
            await handlePresenceSettingsPatchRejection(
              response,
              set,
              get,
              previousConfirmed,
              ticket
            );
            return;
          }

          let raw: unknown;
          try {
            raw = await response.json();
          } catch (error) {
            await reconcileAmbiguousPresenceSettingsUpdate(
              set,
              get,
              previousConfirmed,
              ticket,
              error
            );
            return;
          }
          const settings = parsePresenceSettingsResponse(raw);
          if (settings === null) {
            await reconcileAmbiguousPresenceSettingsUpdate(
              set,
              get,
              previousConfirmed,
              ticket,
              new Error('Received invalid Rich Presence settings')
            );
            return;
          }
          if (!isCurrentPresenceSettingsTicket(ticket)) return;
          const confirmedSettings = mergeRequestSettings(settings, ticket, get().presenceSettings);
          const keepUnconfirmed =
            get().customStatusSaving || get().confirmedPresenceSettings === null;
          presenceSettingsActivityRevision += 1;
          set({
            presenceSettings: confirmedSettings,
            confirmedPresenceSettings: keepUnconfirmed ? null : confirmedSettings,
            presenceSettingsLoading: false,
            presenceSettingsSaving: false,
            presenceSettingsError: keepUnconfirmed ? CUSTOM_STATUS_RECONCILIATION_ERROR : null,
            self: projectSelfPresence(confirmedSettings),
          });
        },

        reset: () => resetPresenceSettingsState(false),
      }),
      { name: 'RichPresenceStore' }
    )
  )
);

onRuntimeServerSelectionChange(() => {
  resetPresenceSettingsState(true);
});

useAuthStore.subscribe((state, previousState) => {
  if (state.authGeneration !== previousState.authGeneration) {
    resetPresenceSettingsState(true, true);
  }
});
