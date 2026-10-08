import { z } from 'zod';
import { createStore } from '../../utils/runtime/createStore';

// Mirrors the backend ClientConfigResponse from services/control-plane/internal/clientconfig/handlers.go

interface FeatureFlags {
  // Server-side flag: true when the control-plane has a KLIPY app key configured
  // and the GIF proxy routes are mounted. The key itself is NEVER sent to the
  // client — all KLIPY traffic goes through /api/v1/klipy/* on the control-plane.
  gifsEnabled?: boolean;
}

// Backend uses omitempty on host/realm (may send `turn: {}`), but the
// clientConfigService always defaults missing fields to '' before calling
// setConfig, so these are guaranteed strings in the store.
interface TURNConfig {
  host: string;
  realm: string;
}

/**
 * The parser IS the type. Not a hand-written interface alongside a zod schema:
 * that pair silently diverged and made this feature unreachable in production.
 * `chunkedAttachmentUpload` was declared here and omitted from the schema, and
 * because zod strips unknown keys, the parsed value was ALWAYS undefined --
 * so the capability read `confirmed-unsupported` no matter what the server
 * said, and every upload took the legacy path this PR exists to replace.
 * tsc could not see it (the interface promised the field) and no test caught it
 * (the tests set the store directly, never going through the parser).
 *
 * Adding a field here now adds it to both at once. Declared in the store rather
 * than in clientConfigService because the service already imports the store,
 * and the reverse edge would be a cycle.
 */
export const ServerCapabilitiesSchema = z.object({
  auth: z.object({
    oauthProviders: z.array(z.string()),
  }),
  features: z.object({
    activityHistorySupported: z.boolean().optional(),
    /** Whether this control plane exposes the chunked attachment upload
     *  session (#2157 PR 2). Absent on any server predating it, and absence
     *  must read as false — the client then keeps the legacy single-shot path
     *  and its renderer-memory ceiling. */
    chunkedAttachmentUpload: z.boolean().optional(),
    /** Versions whose init-session geometry this control plane understands.
     *  Missing on older servers; callers must select v2 in that case. */
    attachmentEnvelopeVersions: z.array(z.union([z.literal(2), z.literal(3)])).optional(),
    /** Whether this control plane gates dangerous actions on MFA for a server
     *  that enforces it (#3454). The server always sends it; absent means a
     *  server that predates it, and zod STRIPS a key it does not declare, so
     *  this line is what makes the capability readable at all (#3456 X16). */
    mfaEnforcedDangerousActions: z.boolean().optional(),
    /** Whether purges keep pinned messages by default and honour
     *  `include_pinned` (#3458). Absent on older servers, which delete pins;
     *  the purge dialogs then hide the option and say nothing about pins. */
    purgeKeepsPinned: z.boolean().optional(),
  }),
});

export type ServerCapabilities = z.infer<typeof ServerCapabilitiesSchema>;

/** Whether the chunked upload session is available, as THREE states.
 *
 *  Reading `serverCapabilities?.features?.chunkedAttachmentUpload === true`
 *  collapsed three distinct facts into one `false`: the server said no, the
 *  server predates the field, and WE COULD NOT ASK. A transient blip on
 *  /server/capabilities nulls the whole object, and the consequences cascaded
 *  silently -- the premium 256 MiB limit clamped to 128 MiB, the upload reverted
 *  to the whole-file legacy path this feature exists to replace, and a queued
 *  200 MiB file was refused with "exceeds the 128 MB limit": a number that is
 *  not the user's limit, blamed on nothing, for a reason unrelated to limits.
 *
 *  Failing closed on `error` is right and unchanged. What was wrong is that the
 *  failure was unobservable and the copy misattributed it. Mirrors
 *  ActivityHistoryCapabilityState below, which got this right first. */
export type ChunkedUploadCapabilityState =
  | { status: 'loading' }
  | { status: 'supported' }
  | { status: 'confirmed-unsupported' }
  | { status: 'error' };

export type ActivityHistoryCapabilityState =
  | { status: 'loading' }
  | { status: 'supported' }
  | { status: 'confirmed-unsupported' }
  | { status: 'error'; lastConfirmedSupported: boolean };

/** Whether dangerous actions are gated on MFA, as the same FOUR states
 *  (#3456 X16). Only `confirmed-unsupported` is a fact about the server: a
 *  payload that parsed and does not carry the key. `loading` and `error` say we
 *  have not been told, so a caller must not claim the server is too old on
 *  either; the server stays the authority on every request. */
export type MfaEnforcementCapabilityState =
  | { status: 'loading' }
  | { status: 'supported' }
  | { status: 'confirmed-unsupported' }
  | { status: 'error' };

/** The capability a parsed capabilities payload states. A failed fetch is
 *  `error`, set by `clientConfigService.setCapabilityError`, never this. */
export function mfaEnforcementCapabilityOf(
  capabilities: ServerCapabilities
): MfaEnforcementCapabilityState {
  return capabilities.features.mfaEnforcedDangerousActions === true
    ? { status: 'supported' }
    : { status: 'confirmed-unsupported' };
}

interface ClientConfig {
  minVersion: string;
  featureFlags: FeatureFlags;
  mediaPlaneUrl: string;
  turn: TURNConfig;
  spaUrl: string;
  spaIpcContract: number;
}

interface ClientConfigState extends ClientConfig {
  serverCapabilities: ServerCapabilities | null;
  activityHistoryCapability: ActivityHistoryCapabilityState;
  chunkedUploadCapability: ChunkedUploadCapabilityState;
  /** Set by the capabilities fetch on both paths, like the two above. */
  mfaEnforcementCapability: MfaEnforcementCapabilityState;
  lastFetchedAt: number | null;
  configRequestRevision: number;
  acceptedConfigRevision: number;
  beginConfigRequest: () => number;
  setConfig: (config: ClientConfig, requestRevision: number) => void;
  setServerCapabilities: (capabilities: ServerCapabilities | null) => void;
  setActivityHistoryCapability: (capability: ActivityHistoryCapabilityState) => void;
  setChunkedUploadCapability: (capability: ChunkedUploadCapabilityState) => void;
  setMfaEnforcementCapability: (capability: MfaEnforcementCapabilityState) => void;
  resetForRuntimeServer: () => void;
}

export const useClientConfigStore = createStore<ClientConfigState>()((set, get) => ({
  minVersion: '',
  featureFlags: {},
  mediaPlaneUrl: '',
  turn: { host: '', realm: '' },
  spaUrl: '',
  spaIpcContract: 0,
  serverCapabilities: null,
  activityHistoryCapability: { status: 'loading' },
  chunkedUploadCapability: { status: 'loading' },
  mfaEnforcementCapability: { status: 'loading' },
  lastFetchedAt: null,
  configRequestRevision: 0,
  acceptedConfigRevision: 0,
  beginConfigRequest: () => {
    const revision = get().configRequestRevision + 1;
    set({ configRequestRevision: revision });
    return revision;
  },
  setConfig: (config, requestRevision) =>
    set({ ...config, lastFetchedAt: Date.now(), acceptedConfigRevision: requestRevision }),
  setServerCapabilities: (serverCapabilities) => set({ serverCapabilities }),
  setActivityHistoryCapability: (activityHistoryCapability) => set({ activityHistoryCapability }),
  setChunkedUploadCapability: (chunkedUploadCapability) => set({ chunkedUploadCapability }),
  setMfaEnforcementCapability: (mfaEnforcementCapability) => set({ mfaEnforcementCapability }),
  resetForRuntimeServer: () =>
    set({
      minVersion: '',
      featureFlags: {},
      mediaPlaneUrl: '',
      turn: { host: '', realm: '' },
      spaUrl: '',
      spaIpcContract: 0,
      serverCapabilities: null,
      activityHistoryCapability: { status: 'loading' },
      chunkedUploadCapability: { status: 'loading' },
      mfaEnforcementCapability: { status: 'loading' },
      lastFetchedAt: null,
      acceptedConfigRevision: 0,
    }),
}));

/** Whether the connected server keeps pinned messages through a purge
 *  (#3458). Subscribed, so a dialog sees the value the moment it lands; each
 *  dialog samples it once on open. Anything but an explicit true (an older
 *  server, a failed fetch, not yet loaded) reads false. */
export function usePurgeKeepsPinned(): boolean {
  return useClientConfigStore((s) => s.serverCapabilities?.features.purgeKeepsPinned === true);
}
