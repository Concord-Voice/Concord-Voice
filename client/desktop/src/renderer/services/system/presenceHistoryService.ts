import { z } from 'zod';
import { apiFetch } from './apiClient';

const HISTORY_ENDPOINT = '/api/v1/users/me/presence-history';
const SETTINGS_ENDPOINT = `${HISTORY_ENDPOINT}/settings`;
const INVALID_RESPONSE_MESSAGE = 'Invalid Activity History response';
const INVALID_MUTATION_MESSAGE = 'Invalid Activity History settings mutation';
const INVALID_PAGE_OPTIONS_MESSAGE = 'Invalid Activity History page options';
const FALLBACK_REQUEST_CODE = 'activity_history_request_failed';
const MAX_CURSOR_LENGTH = 512;
const RAW_ASCII_CONTROL_PATTERN = /[\u0000-\u001f\u007f]/u;
const HIERARCHICAL_POLICY_SCHEME_PATTERN = /^(https?):\/\//iu;

export const ACTIVITY_HISTORY_RETENTION_DAYS = [7, 30, 90, 365] as const;
export type ActivityHistoryRetentionDays = (typeof ACTIVITY_HISTORY_RETENTION_DAYS)[number];

export type PresenceHistorySettingsMutation =
  | {
      kind: 'enable';
      retentionDays: ActivityHistoryRetentionDays;
      consentVersion: number;
      consentCopyHash: string;
    }
  | { kind: 'disable' }
  | { kind: 'retention'; retentionDays: ActivityHistoryRetentionDays };

const PRESENCE_HISTORY_CATEGORIES = [
  'server_voice',
  'private_call',
  'games',
  'music',
  'streaming',
  'browser',
  'productivity',
  'creator',
  'custom_text',
] as const;

export type PresenceHistoryCategory = (typeof PRESENCE_HISTORY_CATEGORIES)[number];

export interface PresenceHistoryRequiredConsent {
  version: number;
  copyHash: string;
  operatorName: string;
  requiredText: string;
  details: string[];
  privacyPolicyUrl: string;
  acknowledgementLabel: string;
}

export interface PresenceHistorySettings {
  available: boolean;
  enabled: boolean;
  reconsentRequired: boolean;
  retentionDays: ActivityHistoryRetentionDays;
  consentVersion: number | null;
  consentCopyHash: string | null;
  consentedAt: string | null;
  requiredConsent: PresenceHistoryRequiredConsent | null;
}

interface PresenceHistoryItemMetadata {
  id: string;
  category: PresenceHistoryCategory;
  payloadVersion: number;
  startedAt: string;
  endedAt: string | null;
  recordedAt: string;
  expiresAt: string;
}

export interface SupportedCustomTextHistoryItem extends PresenceHistoryItemMetadata {
  status: 'supported';
  category: 'custom_text';
  payloadVersion: 1;
  payload: { text: string; emoji?: string };
}

export interface UnsupportedPresenceHistoryItem extends PresenceHistoryItemMetadata {
  status: 'unsupported';
  payload: null;
}

export type PresenceHistoryItem = SupportedCustomTextHistoryItem | UnsupportedPresenceHistoryItem;

export interface PresenceHistoryPage {
  items: PresenceHistoryItem[];
  nextCursor: string | null;
}

export interface PresenceHistoryPageOptions {
  limit?: number;
  before?: string;
  signal?: AbortSignal;
}

export class PresenceHistoryRequestError extends Error {
  public readonly status: number;
  public readonly code: string;
  public readonly retryAfter: number | null;

  constructor(status: number, code: string, retryAfter: number | null) {
    super(code);
    Object.defineProperty(this, 'name', {
      value: 'PresenceHistoryRequestError',
      configurable: true,
    });
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
  }
}

const RetentionDaysSchema = z.union([z.literal(7), z.literal(30), z.literal(90), z.literal(365)]);
const ConsentHashSchema = z.string().regex(/^[0-9a-f]{64}$/u);
const OffsetTimestampSchema = z.iso.datetime({ offset: true });
const PrivacyPolicyURLSchema = z.string().refine(validPrivacyPolicyURL);
const RequiredConsentWireSchema = z.strictObject({
  version: z.number().int().positive(),
  copy_hash: ConsentHashSchema,
  operator_name: z.string().min(1),
  required_text: z.string().min(1),
  details: z.array(z.string().min(1)).min(1),
  privacy_policy_url: PrivacyPolicyURLSchema,
  acknowledgement_label: z.string().min(1),
});

const SettingsWireSchema = z
  .strictObject({
    available: z.boolean(),
    enabled: z.boolean(),
    reconsent_required: z.boolean(),
    retention_days: RetentionDaysSchema,
    consent_version: z.number().int().positive().nullable(),
    consent_copy_hash: ConsentHashSchema.nullable(),
    consented_at: OffsetTimestampSchema.nullable(),
    required_consent: RequiredConsentWireSchema.optional(),
  })
  .superRefine((settings, context) => {
    const hasDisclosure = settings.required_consent !== undefined;
    if (settings.available !== hasDisclosure) {
      context.addIssue({
        code: 'custom',
        path: ['required_consent'],
        message: 'Disclosure availability does not match required consent',
      });
    }

    if (settings.enabled) {
      if (!settings.available || settings.reconsent_required) {
        context.addIssue({
          code: 'custom',
          path: ['enabled'],
          message: 'Enabled history must be available and currently consented',
        });
      }
      if (
        settings.consent_version === null ||
        settings.consent_copy_hash === null ||
        settings.consented_at === null
      ) {
        context.addIssue({
          code: 'custom',
          path: ['consent_version'],
          message: 'Enabled history requires complete consent metadata',
        });
      }
      if (
        settings.required_consent !== undefined &&
        (settings.consent_version !== settings.required_consent.version ||
          settings.consent_copy_hash !== settings.required_consent.copy_hash)
      ) {
        context.addIssue({
          code: 'custom',
          path: ['consent_version'],
          message: 'Enabled history consent must match the current disclosure',
        });
      }
      return;
    }

    if (
      settings.consent_version !== null ||
      settings.consent_copy_hash !== null ||
      settings.consented_at !== null
    ) {
      context.addIssue({
        code: 'custom',
        path: ['consent_version'],
        message: 'Disabled history cannot retain accepted consent metadata',
      });
    }
  });

const MutationSchema = z.discriminatedUnion('kind', [
  z.strictObject({
    kind: z.literal('enable'),
    retentionDays: RetentionDaysSchema,
    consentVersion: z.number().int().positive(),
    consentCopyHash: ConsentHashSchema,
  }),
  z.strictObject({ kind: z.literal('disable') }),
  z.strictObject({
    kind: z.literal('retention'),
    retentionDays: RetentionDaysSchema,
  }),
]);

const CustomTextSchema = z.strictObject({
  text: z.string().refine((value) => codePointLength(value) >= 1 && codePointLength(value) <= 140),
  emoji: z
    .string()
    .refine((value) => codePointLength(value) <= 32)
    .optional(),
});
const HistoryCategorySchema = z.enum(PRESENCE_HISTORY_CATEGORIES);
const HistoryMetadataWireShape = {
  id: z.uuid(),
  started_at: OffsetTimestampSchema,
  ended_at: OffsetTimestampSchema.nullable(),
  recorded_at: OffsetTimestampSchema,
  expires_at: OffsetTimestampSchema,
};
const SupportedItemWireSchema = z.strictObject({
  status: z.literal('supported'),
  ...HistoryMetadataWireShape,
  category: z.literal('custom_text'),
  payload_version: z.literal(1),
  payload: CustomTextSchema,
});
const UnsupportedItemWireSchema = z.strictObject({
  status: z.literal('unsupported'),
  ...HistoryMetadataWireShape,
  category: HistoryCategorySchema,
  payload_version: z.number().int().positive(),
  payload: z.null(),
});
const HistoryItemWireSchema = z.discriminatedUnion('status', [
  SupportedItemWireSchema,
  UnsupportedItemWireSchema,
]);
const HistoryPageWireSchema = z.strictObject({
  items: z.array(HistoryItemWireSchema),
  next_cursor: z.string().min(1).max(MAX_CURSOR_LENGTH).nullable(),
});
const RequestErrorWireSchema = z.strictObject({
  code: z.string().regex(/^[a-z][a-z0-9_]{0,127}$/u),
  required_consent: z.unknown().optional(),
});

type SettingsWire = z.infer<typeof SettingsWireSchema>;
type HistoryItemWire = z.infer<typeof HistoryItemWireSchema>;

export async function getPresenceHistorySettings(
  signal?: AbortSignal
): Promise<PresenceHistorySettings> {
  const raw = await requestJson(SETTINGS_ENDPOINT, undefined, signal);
  const parsed = SettingsWireSchema.safeParse(raw);
  signal?.throwIfAborted();
  if (!parsed.success) throw invalidResponseError();
  return mapSettings(parsed.data);
}

export async function patchPresenceHistorySettings(
  mutation: PresenceHistorySettingsMutation,
  signal?: AbortSignal
): Promise<PresenceHistorySettings> {
  signal?.throwIfAborted();
  const parsedMutation = MutationSchema.safeParse(mutation);
  if (!parsedMutation.success) throw new Error(INVALID_MUTATION_MESSAGE);

  const body = mutationBody(parsedMutation.data);
  const raw = await requestJson(
    SETTINGS_ENDPOINT,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
    signal
  );
  const parsed = SettingsWireSchema.safeParse(raw);
  signal?.throwIfAborted();
  if (!parsed.success) throw invalidResponseError();
  return mapSettings(parsed.data);
}

export async function getPresenceHistoryPage(
  options: PresenceHistoryPageOptions = {}
): Promise<PresenceHistoryPage> {
  const { limit, before, signal } = options;
  signal?.throwIfAborted();
  if (limit !== undefined && (!Number.isInteger(limit) || limit < 1 || limit > 100)) {
    throw new Error(INVALID_PAGE_OPTIONS_MESSAGE);
  }
  if (before !== undefined && (typeof before !== 'string' || before.length === 0)) {
    throw new Error(INVALID_PAGE_OPTIONS_MESSAGE);
  }

  const query = new URLSearchParams();
  if (limit !== undefined) query.set('limit', String(limit));
  if (before !== undefined) query.set('before', before);
  const suffix = query.size === 0 ? '' : `?${query.toString()}`;
  const raw = await requestJson(`${HISTORY_ENDPOINT}${suffix}`, undefined, signal);
  const parsed = HistoryPageWireSchema.safeParse(raw);
  signal?.throwIfAborted();
  if (!parsed.success) throw invalidResponseError();
  return {
    items: parsed.data.items.map(mapHistoryItem),
    nextCursor: parsed.data.next_cursor,
  };
}

export async function deletePresenceHistory(signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted();
  const response = await apiFetch(HISTORY_ENDPOINT, withSignal({ method: 'DELETE' }, signal));
  signal?.throwIfAborted();
  if (!response.ok) throw await requestError(response, signal);
  if (response.status !== 204) throw invalidResponseError();
  signal?.throwIfAborted();
}

function codePointLength(value: string): number {
  return [...value].length;
}

function validPrivacyPolicyURL(value: string): boolean {
  if (value.length === 0 || RAW_ASCII_CONTROL_PATTERN.test(value)) return false;

  const schemeMatch = HIERARCHICAL_POLICY_SCHEME_PATTERN.exec(value);
  if (schemeMatch === null) return false;
  const scheme: 'https:' | 'http:' = schemeMatch[1].toLowerCase() === 'https' ? 'https:' : 'http:';

  const authority = urlAuthority(value);
  if (authority.length === 0 || authority.includes('@') || authority.includes('\\')) return false;

  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    return false;
  }
  if (
    parsed.protocol !== scheme ||
    parsed.hostname.length === 0 ||
    parsed.username.length > 0 ||
    parsed.password.length > 0
  ) {
    return false;
  }
  if (scheme === 'https:') return true;
  return isLoopbackAuthority(authority, parsed.hostname);
}

function urlAuthority(value: string): string {
  const remainder = value.slice(value.indexOf('://') + 3);
  const delimiterIndex = remainder.search(/[/?#]/u);
  return delimiterIndex === -1 ? remainder : remainder.slice(0, delimiterIndex);
}

function isLoopbackAuthority(authority: string, parsedHostname: string): boolean {
  if (authority.startsWith('[')) return isIPv6Loopback(parsedHostname);
  const portSeparator = authority.lastIndexOf(':');
  const hostname = portSeparator === -1 ? authority : authority.slice(0, portSeparator);
  if (hostname.toLowerCase() === 'localhost') return true;
  return isIPv4Loopback(hostname);
}

function isIPv4Loopback(hostname: string): boolean {
  const octets = hostname.split('.');
  if (octets.length !== 4 || octets[0] !== '127') return false;
  return octets.every((octet) => /^(0|[1-9]\d{0,2})$/u.test(octet) && Number(octet) <= 255);
}

function isIPv6Loopback(parsedHostname: string): boolean {
  const normalized = parsedHostname.toLowerCase();
  if (normalized === '[::1]') return true;
  return /^\[::ffff:7f[0-9a-f]{2}:[0-9a-f]{1,4}\]$/u.test(normalized);
}

function mutationBody(mutation: z.infer<typeof MutationSchema>): Record<string, unknown> {
  switch (mutation.kind) {
    case 'enable':
      return {
        enabled: true,
        retention_days: mutation.retentionDays,
        acknowledged: true,
        consent_version: mutation.consentVersion,
        consent_copy_hash: mutation.consentCopyHash,
      };
    case 'disable':
      return { enabled: false };
    case 'retention':
      return { retention_days: mutation.retentionDays };
  }
}

function mapSettings(settings: SettingsWire): PresenceHistorySettings {
  return {
    available: settings.available,
    enabled: settings.enabled,
    reconsentRequired: settings.reconsent_required,
    retentionDays: settings.retention_days,
    consentVersion: settings.consent_version,
    consentCopyHash: settings.consent_copy_hash,
    consentedAt: settings.consented_at,
    requiredConsent:
      settings.required_consent === undefined
        ? null
        : {
            version: settings.required_consent.version,
            copyHash: settings.required_consent.copy_hash,
            operatorName: settings.required_consent.operator_name,
            requiredText: settings.required_consent.required_text,
            details: [...settings.required_consent.details],
            privacyPolicyUrl: settings.required_consent.privacy_policy_url,
            acknowledgementLabel: settings.required_consent.acknowledgement_label,
          },
  };
}

function mapHistoryItem(item: HistoryItemWire): PresenceHistoryItem {
  const metadata = {
    id: item.id,
    category: item.category,
    payloadVersion: item.payload_version,
    startedAt: item.started_at,
    endedAt: item.ended_at,
    recordedAt: item.recorded_at,
    expiresAt: item.expires_at,
  };
  if (item.status === 'unsupported') {
    return { ...metadata, status: 'unsupported', payload: null };
  }
  return {
    ...metadata,
    status: 'supported',
    category: 'custom_text',
    payloadVersion: 1,
    payload: {
      text: item.payload.text,
      ...(item.payload.emoji === undefined ? {} : { emoji: item.payload.emoji }),
    },
  };
}

async function requestJson(
  path: string,
  init: RequestInit | undefined,
  signal: AbortSignal | undefined
): Promise<unknown> {
  signal?.throwIfAborted();
  const response = await apiFetch(path, withSignal(init, signal));
  signal?.throwIfAborted();
  if (!response.ok) throw await requestError(response, signal);
  try {
    const raw: unknown = await response.json();
    signal?.throwIfAborted();
    return raw;
  } catch {
    signal?.throwIfAborted();
    throw invalidResponseError();
  }
}

async function requestError(
  response: Response,
  signal: AbortSignal | undefined
): Promise<PresenceHistoryRequestError> {
  let code = FALLBACK_REQUEST_CODE;
  try {
    const raw: unknown = await response.json();
    signal?.throwIfAborted();
    const parsed = RequestErrorWireSchema.safeParse(raw);
    if (parsed.success) code = parsed.data.code;
  } catch {
    signal?.throwIfAborted();
  }
  return new PresenceHistoryRequestError(
    response.status,
    code,
    parseRetryAfter(response.headers.get('Retry-After'))
  );
}

function parseRetryAfter(value: string | null): number | null {
  if (value === null || !/^(0|[1-9]\d*)$/u.test(value)) return null;
  const seconds = Number(value);
  return Number.isSafeInteger(seconds) ? seconds : null;
}

function withSignal(
  init: RequestInit | undefined,
  signal: AbortSignal | undefined
): RequestInit | undefined {
  if (signal === undefined) return init;
  return { ...init, signal };
}

function invalidResponseError(): Error {
  return new Error(INVALID_RESPONSE_MESSAGE);
}
