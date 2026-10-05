import type {
  DeviceRecoveryContext,
  DeviceRecoveryOffer,
  DeviceRecoveryRequest,
  DeviceRecoveryCreateBody,
  DeviceRecoveryRespondBody,
  DeviceRecoveryCompleteBody,
} from './deviceRecoveryContract';
import {
  captureRecoveryTokenContext,
  deriveRecoveryAccountBinding,
  validateRecoveryContext,
  validateRecoveryPublicKey,
  decodeCanonicalBase64,
  deriveDeviceRecoveryKeys,
  decryptDeviceRecoveryPayload,
  encryptDeviceRecoveryPayload,
  validateRecoveryPayload,
  RECOVERY_UPDATE_GUIDANCE,
} from '../../utils/crypto/trustedDeviceRecovery';
import {
  generateDeviceRecoveryKeyPair,
  exportECDHPublicKey,
  randomRecoveryNonce,
  exportRecoveryAccountKey,
  validateRecoveryAccountKey,
  type DeviceRecoveryKeyMaterial,
} from '../../utils/crypto/crypto';
import { e2eeService } from '../e2ee/e2eeService';
import { useAuthStore } from '../../stores/auth/authStore';
import { useUserStore } from '../../stores/auth/userStore';
import { captureAuthLifecycle, isSameAuthLifecycle } from './postLoginHydrationLifecycle';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
  onRuntimeServerSelectionChange,
  type RuntimeServerSelection,
} from './runtimeServerBase';
import { apiFetch } from './apiClient';

const RECOVERY_LIFETIME_MS = 15 * 60_000;
const RECOVERY_CLOCK_SKEW_MS = 2 * 60_000;

const CONTEXT_FIELDS = [
  'request_id',
  'protocol_version',
  'server_origin',
  'account_binding',
  'expires_at',
  'requester_nonce',
  'requester_public_key',
  'recovery_token_jti_hash',
] as const;
const OFFER_FIELDS = ['responder_public_key', 'responder_nonce', 'transcript_hash'] as const;
export type ReviewableDeviceRecoveryRequest = Extract<
  DeviceRecoveryRequest,
  { status: 'pending' | 'offered' }
>;
export interface DeviceRecoveryView {
  status:
    | 'creating'
    | 'pending'
    | 'offered'
    | 'approved-locked'
    | 'completing'
    | 'complete'
    | 'submitted'
    | 'rejected'
    | 'error';
  fingerprint: string;
  confirmed: boolean;
  error: string;
  retryAt: number;
}

export class DeviceRecoveryError extends Error {
  constructor(
    message: string,
    readonly retryable = false,
    readonly retryAfterMs = 3000,
    readonly status = 0
  ) {
    super(message);
  }
}

function exactObject(value: unknown, fields: readonly string[]): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
  const object = value as Record<string, unknown>;
  if (
    Object.keys(object).length !== fields.length ||
    fields.some((key) => !Object.hasOwn(object, key))
  ) {
    throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
  }
  return object;
}

export async function validateDeviceRecoveryRequest(
  value: unknown
): Promise<DeviceRecoveryRequest> {
  const basic = value as Record<string, unknown> | null;
  const status = basic?.status;
  if (status === 'rejected' || status === 'expired' || status === 'complete') {
    const object = exactObject(value, ['request_id', 'protocol_version', 'status', 'expires_at']);
    // Validate terminal metadata using the same canonical ID/expiry predicates.
    if (
      object.protocol_version !== 2 ||
      typeof object.request_id !== 'string' ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(object.request_id) ||
      typeof object.expires_at !== 'number' ||
      !Number.isSafeInteger(object.expires_at) ||
      object.expires_at <= 0
    ) {
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    }
    return Object.freeze(object) as Extract<
      DeviceRecoveryRequest,
      { status: 'rejected' | 'expired' | 'complete' }
    >;
  }
  if (status !== 'pending' && status !== 'offered' && status !== 'approved')
    throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
  const fields = [
    ...CONTEXT_FIELDS,
    'status',
    ...(status === 'pending' ? [] : OFFER_FIELDS),
    ...(status === 'approved' ? ['encrypted_payload'] : []),
  ];
  const object = exactObject(value, fields);
  for (const key of [
    ...CONTEXT_FIELDS.filter((field) => field !== 'protocol_version' && field !== 'expires_at'),
    ...(status === 'pending' ? [] : OFFER_FIELDS),
  ]) {
    if (typeof object[key] !== 'string') throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
  }
  const context = object as unknown as DeviceRecoveryContext;
  validateRecoveryContext(context);
  await validateRecoveryPublicKey(context.requester_public_key);
  if (status !== 'pending') {
    const offer = object as unknown as DeviceRecoveryOffer;
    decodeCanonicalBase64(offer.responder_nonce, 32);
    decodeCanonicalBase64(offer.transcript_hash, 32);
    await validateRecoveryPublicKey(offer.responder_public_key);
  }
  if (status === 'approved') {
    if (typeof object.encrypted_payload !== 'string')
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    validateRecoveryPayload(object.encrypted_payload);
  }
  return Object.freeze(object) as unknown as DeviceRecoveryRequest;
}

function retryDelay(response: Response): number {
  const header = response.headers.get('Retry-After');
  if (!header) return 3000;
  const seconds = Number(header);
  const delay = Number.isFinite(seconds) ? seconds * 1000 : Date.parse(header) - Date.now();
  return Number.isFinite(delay) ? Math.max(3000, delay) : 3000;
}

function recoveryResponseErrorMessage(status: number): string {
  if (status === 429) return 'Recovery is rate limited. Waiting before retrying.';
  if (status >= 500) return 'Recovery connection failed. Retry when the connection is available.';
  if (status === 401) return 'Recovery authorization expired. Start a new request.';
  if (status === 409)
    return 'Recovery state changed. Check the recovering device or restart recovery.';
  return RECOVERY_UPDATE_GUIDANCE;
}

async function readResponse(
  response: Response,
  assertCurrent: () => void = () => {}
): Promise<unknown> {
  assertCurrent();
  if (!response.ok) {
    const retryable = response.status === 429 || response.status >= 500;
    throw new DeviceRecoveryError(
      recoveryResponseErrorMessage(response.status),
      retryable,
      retryDelay(response),
      response.status
    );
  }
  const body: unknown = await response.json().catch(() => {
    throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
  });
  assertCurrent();
  return body;
}

async function recoveryFetch(
  server: RuntimeServerSelection,
  path: string,
  token: string,
  body?: DeviceRecoveryCreateBody | DeviceRecoveryCompleteBody,
  assertCurrent: () => void = () => {}
): Promise<unknown> {
  assertCurrent();
  let response: Response;
  try {
    response = await fetch(`${server.apiBase}${path}`, {
      ...(body ? { method: 'POST', body: JSON.stringify(body) } : {}),
      headers: {
        Authorization: `Bearer ${token}`,
        ...(body ? { 'Content-Type': 'application/json' } : {}),
      },
    });
  } catch {
    throw new DeviceRecoveryError('Recovery connection failed. Waiting before retrying.', true);
  }
  assertCurrent();
  return readResponse(response, assertCurrent);
}

async function respond(
  requestId: string,
  body: DeviceRecoveryRespondBody,
  assertCurrent: () => void
): Promise<unknown> {
  assertCurrent();
  let response: Response;
  try {
    response = await apiFetch(
      `/api/v1/mfa/recovery-requests/${encodeURIComponent(requestId)}/respond`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
      { assertBeforeDispatch: assertCurrent }
    );
  } catch {
    assertCurrent();
    throw new DeviceRecoveryError(
      'Recovery connection failed. Retry when the connection is available.',
      true
    );
  }
  assertCurrent();
  return readResponse(response, assertCurrent);
}

function validateAcknowledgement(value: unknown, requestId: string, status: string): void {
  const fields = exactObject(value, ['request_id', 'protocol_version', 'status']);
  if (
    fields.request_id !== requestId ||
    fields.protocol_version !== 2 ||
    fields.status !== status
  ) {
    throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
  }
}

export function sameRecoveryContext(
  left: DeviceRecoveryContext,
  right: DeviceRecoveryContext
): boolean {
  return CONTEXT_FIELDS.every((field) => left[field] === right[field]);
}
function copyContext(source: DeviceRecoveryContext): DeviceRecoveryContext {
  return Object.freeze(
    Object.fromEntries(CONTEXT_FIELDS.map((field) => [field, source[field]]))
  ) as unknown as DeviceRecoveryContext;
}
function copyOffer(source: DeviceRecoveryOffer): DeviceRecoveryOffer {
  return Object.freeze({
    responder_public_key: source.responder_public_key,
    responder_nonce: source.responder_nonce,
    transcript_hash: source.transcript_hash,
  });
}
function sameOffer(left: DeviceRecoveryOffer, right: DeviceRecoveryOffer): boolean {
  return OFFER_FIELDS.every((field) => left[field] === right[field]);
}

/** Only validated, owned unexpired rows reach the settings UI. */
export async function listDeviceRecoveryRequests(
  userId: string,
  assertCurrent: () => void
): Promise<ReviewableDeviceRecoveryRequest[]> {
  assertCurrent();
  const binding = await deriveRecoveryAccountBinding(userId);
  assertCurrent();
  let response: Response;
  try {
    response = await apiFetch('/api/v1/mfa/recovery-requests');
  } catch {
    throw new DeviceRecoveryError('Recovery requests could not be refreshed.', true);
  }
  assertCurrent();
  const body = exactObject(await readResponse(response, assertCurrent), ['requests']);
  assertCurrent();
  const receiveExpiryBound = Date.now() + RECOVERY_LIFETIME_MS + RECOVERY_CLOCK_SKEW_MS;
  if (!Array.isArray(body.requests)) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
  const requests: ReviewableDeviceRecoveryRequest[] = [];
  const ids = new Set<string>();
  const origin = new URL(captureRuntimeServerSelection().apiBase).origin;
  for (const value of body.requests) {
    const request = await validateDeviceRecoveryRequest(value);
    assertCurrent();
    if (
      (request.status !== 'pending' && request.status !== 'offered') ||
      request.expires_at <= Date.now() ||
      request.expires_at > receiveExpiryBound ||
      request.account_binding !== binding ||
      request.server_origin !== origin ||
      ids.has(request.request_id)
    )
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    ids.add(request.request_id);
    requests.push(request);
  }
  return requests;
}

abstract class RecoveryAttempt {
  readonly server = captureRuntimeServerSelection();
  private readonly auth = captureAuthLifecycle();
  private readonly keyEpoch = e2eeService.captureTeardownEpoch();
  private readonly userId = useUserStore.getState().user?.id;
  private alive = true;
  private readonly unsubscribes: Array<() => void>;
  private readonly expiryWatcher: ReturnType<typeof setInterval>;
  protected expiry = Date.now() + RECOVERY_LIFETIME_MS;
  protected keyPair: CryptoKeyPair | null = null;
  protected material: DeviceRecoveryKeyMaterial | null = null;
  protected offer: DeviceRecoveryOffer | null = null;
  protected context: DeviceRecoveryContext | null = null;
  protected confirmedDigest = '';
  protected view: DeviceRecoveryView = {
    status: 'creating',
    fingerprint: '',
    confirmed: false,
    error: '',
    retryAt: 0,
  };

  constructor(
    private readonly onView: (view: DeviceRecoveryView) => void,
    private readonly responder: boolean
  ) {
    const inspect = () => {
      try {
        this.assertCurrent();
      } catch (error) {
        this.fail(error);
      }
    };
    this.unsubscribes = [
      useAuthStore.subscribe(inspect),
      useUserStore.subscribe(inspect),
      onRuntimeServerSelectionChange(inspect),
    ];
    this.expiryWatcher = setInterval(inspect, 1000);
  }
  readonly assertCurrent = (): void => {
    if (
      !this.alive ||
      !isSameAuthLifecycle(this.auth) ||
      !runtimeServerSelectionIsCurrent(this.server) ||
      e2eeService.wasTornDownSince(this.keyEpoch) ||
      (this.responder && !this.userId) ||
      useUserStore.getState().user?.id !== this.userId
    ) {
      throw new DeviceRecoveryError(
        'Account, server or keys changed. Restart recovery on both devices.'
      );
    }
    if (Date.now() >= this.expiry)
      throw new DeviceRecoveryError('Recovery request expired. Start a new request.');
  };
  protected publish(patch: Partial<DeviceRecoveryView>): void {
    this.assertCurrent();
    this.view = { ...this.view, ...patch };
    this.onView(this.view);
  }
  protected fail(error: unknown): void {
    if (!this.alive) return;
    if (error instanceof DeviceRecoveryError && error.retryable) {
      try {
        this.publish({ error: error.message, retryAt: Date.now() + error.retryAfterMs });
        return;
      } catch {
        /* A lifecycle fence always wins over transport retry. */
      }
    }
    this.view = {
      status: 'error',
      fingerprint: '',
      confirmed: false,
      error: error instanceof DeviceRecoveryError ? error.message : RECOVERY_UPDATE_GUIDANCE,
      retryAt: 0,
    };
    this.dispose();
    this.onView(this.view);
  }
  protected wipe(): void {
    this.keyPair = null;
    this.material = null;
    this.offer = null;
    this.confirmedDigest = '';
  }
  dispose(): void {
    this.alive = false;
    this.wipe();
    clearInterval(this.expiryWatcher);
    this.unsubscribes.forEach((unsubscribe) => unsubscribe());
  }
  protected requireContext(): DeviceRecoveryContext {
    this.assertCurrent();
    if (!this.context) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    return this.context;
  }
  protected confirmDigest(): void {
    this.assertCurrent();
    if (
      !this.material ||
      !this.offer ||
      this.material.transcriptHash !== this.offer.transcript_hash
    ) {
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    }
    this.confirmedDigest = this.material.transcriptHash;
    this.publish({ confirmed: true, error: '', retryAt: 0 });
  }
}

export class RequesterDeviceRecoveryAttempt extends RecoveryAttempt {
  private pollTimer: ReturnType<typeof setTimeout> | null = null;
  private polling = false;
  private processing = false;
  private ciphertext = '';
  private pkcs8: ArrayBuffer | null = null;
  private imported = false;
  constructor(
    private readonly token: string,
    onView: (view: DeviceRecoveryView) => void
  ) {
    super(onView, false);
  }
  async start(): Promise<void> {
    try {
      this.assertCurrent();
      const tokenContext = await captureRecoveryTokenContext(this.token);
      this.assertCurrent();
      this.expiry = Math.min(this.expiry, tokenContext.expiresAt);
      const pair = await generateDeviceRecoveryKeyPair();
      this.assertCurrent();
      this.keyPair = pair;
      const publicKey = await exportECDHPublicKey(pair.publicKey);
      this.assertCurrent();
      const body: DeviceRecoveryCreateBody = {
        protocol_version: 2,
        recovery_token: this.token,
        server_origin: new URL(this.server.apiBase).origin,
        account_binding: tokenContext.accountBinding,
        requester_nonce: randomRecoveryNonce(),
        requester_public_key: publicKey,
      };
      const response = await recoveryFetch(
        this.server,
        '/api/v1/auth/recovery/device-request',
        this.token,
        body,
        this.assertCurrent
      );
      // Creation starts the server's lifetime after key generation and transport.
      // The pre-create bound still fences those awaits; pin the echoed deadline once.
      const receivedAt = Date.now();
      const receiveExpiryBound = Math.min(
        tokenContext.expiresAt,
        receivedAt + RECOVERY_LIFETIME_MS + RECOVERY_CLOCK_SKEW_MS
      );
      const created = await validateDeviceRecoveryRequest(response);
      this.assertCurrent();
      if (
        created.status !== 'pending' ||
        created.server_origin !== body.server_origin ||
        created.account_binding !== body.account_binding ||
        created.requester_nonce !== body.requester_nonce ||
        created.requester_public_key !== publicKey ||
        created.recovery_token_jti_hash !== tokenContext.jtiHash ||
        created.expires_at > receiveExpiryBound ||
        created.expires_at <= Date.now()
      )
        throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      this.context = copyContext(created);
      // Clock tolerance admits the immutable wire deadline, never extends local custody.
      this.expiry = Math.min(created.expires_at, receivedAt + RECOVERY_LIFETIME_MS);
      this.publish({ status: 'pending' });
      this.schedulePoll(3000);
    } catch (error) {
      // Creation has no idempotency key; an ambiguous create must restart with fresh material.
      this.fail(
        error instanceof DeviceRecoveryError && error.retryable
          ? new DeviceRecoveryError(
              'Recovery request could not be created. Start a new request after retrying the connection.'
            )
          : error
      );
    }
  }
  private schedulePoll(delay: number): void {
    this.assertCurrent();
    if (this.pollTimer) clearTimeout(this.pollTimer);
    this.pollTimer = setTimeout(() => {
      void this.poll();
    }, delay);
  }
  async poll(): Promise<void> {
    if (
      this.polling ||
      this.processing ||
      this.view.status === 'complete' ||
      Date.now() < this.view.retryAt
    )
      return;
    this.polling = true;
    let delay = 3000;
    try {
      const context = this.requireContext();
      const response = await recoveryFetch(
        this.server,
        `/api/v1/auth/recovery/device-request/${context.request_id}`,
        this.token,
        undefined,
        this.assertCurrent
      );
      this.assertCurrent();
      const request = await validateDeviceRecoveryRequest(response);
      this.assertCurrent();
      await this.acceptPolledRequest(context, request);
    } catch (error) {
      if (error instanceof DeviceRecoveryError && error.retryable) delay = error.retryAfterMs;
      this.fail(error);
    } finally {
      this.polling = false;
      if (!this.pollHasStopped()) {
        try {
          this.schedulePoll(Math.max(delay, this.view.retryAt - Date.now()));
        } catch (error) {
          this.fail(error);
        }
      }
    }
  }
  private validatePolledMetadata(
    context: DeviceRecoveryContext,
    request: DeviceRecoveryRequest
  ): void {
    if (request.request_id !== context.request_id || request.expires_at !== context.expires_at)
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    if (request.status === 'rejected' || request.status === 'expired')
      throw new DeviceRecoveryError(
        request.status === 'rejected'
          ? 'Recovery request was rejected by the trusted device.'
          : 'Recovery request expired. Start a new request.'
      );
  }
  private async acceptPolledRequest(
    context: DeviceRecoveryContext,
    request: DeviceRecoveryRequest
  ): Promise<void> {
    this.validatePolledMetadata(context, request);
    if (request.status === 'complete') {
      if (!this.imported || !this.pkcs8) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      this.publish({ status: 'complete', error: '', retryAt: 0 });
      return;
    }
    if (
      request.status !== 'pending' &&
      request.status !== 'offered' &&
      request.status !== 'approved'
    )
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    if (!sameRecoveryContext(context, request))
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    if (request.status === 'pending') {
      if (this.offer) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      this.publish({ error: '', retryAt: 0 });
      return;
    }
    await this.retainPolledOffer(context, request);
  }
  private async retainPolledOffer(
    context: DeviceRecoveryContext,
    request: Extract<DeviceRecoveryRequest, { status: 'offered' | 'approved' }>
  ): Promise<void> {
    if (this.offer && !sameOffer(this.offer, request))
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    if (!this.offer) {
      if (!this.keyPair) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      const offer = copyOffer(request);
      const material = await deriveDeviceRecoveryKeys(
        'requester',
        this.keyPair.privateKey,
        context,
        offer,
        this.assertCurrent
      );
      this.assertCurrent();
      if (material.transcriptHash !== offer.transcript_hash)
        throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      this.offer = offer;
      this.material = material;
    }
    if (request.status === 'approved') {
      if (this.ciphertext && this.ciphertext !== request.encrypted_payload)
        throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      this.ciphertext = request.encrypted_payload;
    }
    this.publish({
      status: request.status === 'approved' ? 'approved-locked' : 'offered',
      fingerprint: this.material?.fingerprint ?? '',
      error: '',
      retryAt: 0,
    });
    if (this.confirmedDigest && this.ciphertext) await this.processApproval();
  }
  private pollHasStopped(): boolean {
    return this.view.status === 'error' || this.view.status === 'complete';
  }
  async confirmMatch(): Promise<void> {
    if (this.processing || this.view.status === 'complete' || Date.now() < this.view.retryAt)
      return;
    try {
      this.confirmDigest();
      if (this.ciphertext) await this.processApproval();
    } catch (error) {
      this.fail(error);
    }
  }
  private async processApproval(): Promise<void> {
    if (this.processing) return;
    this.processing = true;
    try {
      const context = this.requireContext();
      const material = this.material;
      if (this.confirmedDigest !== material?.transcriptHash)
        throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      this.publish({ status: 'completing' });
      if (!this.pkcs8) {
        const bytes = await decryptDeviceRecoveryPayload(material, this.ciphertext);
        try {
          this.assertCurrent();
          await validateRecoveryAccountKey(bytes);
          this.assertCurrent();
          this.pkcs8 = bytes;
          this.imported = true;
        } catch (error) {
          new Uint8Array(bytes).fill(0);
          throw error;
        }
      }
      this.assertCurrent();
      try {
        const ack = await recoveryFetch(
          this.server,
          `/api/v1/auth/recovery/device-request/${context.request_id}/complete`,
          this.token,
          { protocol_version: 2, transcript_hash: material.transcriptHash },
          this.assertCurrent
        );
        this.assertCurrent();
        validateAcknowledgement(ack, context.request_id, 'complete');
        this.publish({ status: 'complete', error: '', retryAt: 0 });
      } catch (error) {
        // Lost completion acknowledgement may be reconciled only with this attempt's imported key.
        if (error instanceof DeviceRecoveryError && (error.retryable || error.status === 409)) {
          this.publish({
            status: 'completing',
            error: 'Checking whether recovery completion was acknowledged.',
            retryAt: Date.now() + error.retryAfterMs,
          });
          this.schedulePoll(error.retryAfterMs);
        } else {
          throw error;
        }
      }
    } finally {
      this.processing = false;
    }
  }
  recoveredAccountKey(): ArrayBuffer {
    this.assertCurrent();
    if (this.view.status !== 'complete' || !this.pkcs8 || !this.imported)
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    return this.pkcs8;
  }
  protected override wipe(): void {
    super.wipe();
    if (this.pkcs8) new Uint8Array(this.pkcs8).fill(0);
    this.pkcs8 = null;
    this.imported = false;
    this.ciphertext = '';
    if (this.pollTimer) clearTimeout(this.pollTimer);
    this.pollTimer = null;
  }
}

export class ResponderDeviceRecoveryAttempt extends RecoveryAttempt {
  private busy = false;
  private offered = false;
  private approvalNeedsReconciliation = false;
  constructor(
    private readonly request: ReviewableDeviceRecoveryRequest,
    onView: (view: DeviceRecoveryView) => void
  ) {
    super(onView, true);
    this.context = copyContext(request);
    this.expiry = Math.min(this.expiry, request.expires_at);
  }
  async start(): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    try {
      const context = this.requireContext();
      if (this.request.status === 'offered')
        throw new DeviceRecoveryError(
          'The comparison key for this offered request is no longer available. Reject it and start a new request on the recovering device.'
        );
      const userId = useUserStore.getState().user?.id;
      if (!userId)
        throw new DeviceRecoveryError('Account identity unavailable. Please sign in again.');
      const binding = await deriveRecoveryAccountBinding(userId);
      this.assertCurrent();
      if (
        binding !== context.account_binding ||
        context.server_origin !== new URL(this.server.apiBase).origin
      )
        throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      await validateRecoveryPublicKey(context.requester_public_key);
      this.assertCurrent();
      const pair = await generateDeviceRecoveryKeyPair();
      this.assertCurrent();
      this.keyPair = pair;
      const publicKey = await exportECDHPublicKey(pair.publicKey);
      this.assertCurrent();
      const proposed = { responder_public_key: publicKey, responder_nonce: randomRecoveryNonce() };
      const material = await deriveDeviceRecoveryKeys(
        'responder',
        pair.privateKey,
        context,
        proposed,
        this.assertCurrent
      );
      this.assertCurrent();
      this.material = material;
      this.offer = Object.freeze({ ...proposed, transcript_hash: material.transcriptHash });
      await this.sendOffer();
    } catch (error) {
      this.fail(error);
    } finally {
      this.busy = false;
    }
  }
  private async sendOffer(): Promise<void> {
    const context = this.requireContext();
    if (!this.offer) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    const ack = await respond(
      context.request_id,
      {
        action: 'offer',
        protocol_version: 2,
        ...this.offer,
      },
      this.assertCurrent
    );
    this.assertCurrent();
    validateAcknowledgement(ack, context.request_id, 'offered');
    this.offered = true;
    this.publish({
      status: 'offered',
      fingerprint: this.material?.fingerprint ?? '',
      error: '',
      retryAt: 0,
    });
  }
  async retryOffer(): Promise<void> {
    if (this.busy || Date.now() < this.view.retryAt || this.offered) return;
    this.busy = true;
    try {
      const context = this.requireContext();
      const userId = useUserStore.getState().user?.id;
      if (!userId || !this.offer) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      const rows = await listDeviceRecoveryRequests(userId, this.assertCurrent);
      this.assertCurrent();
      const row = rows.find((entry) => entry.request_id === context.request_id);
      if (!row || !sameRecoveryContext(row, context))
        throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      if (row.status === 'offered') {
        if (!sameOffer(row, this.offer)) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
        this.offered = true;
        this.publish({
          status: 'offered',
          fingerprint: this.material?.fingerprint ?? '',
          error: '',
          retryAt: 0,
        });
      } else {
        await this.sendOffer();
      }
    } catch (error) {
      this.fail(error);
    } finally {
      this.busy = false;
    }
  }
  private async reconcileApprovalRetry(context: DeviceRecoveryContext): Promise<void> {
    const userId = useUserStore.getState().user?.id;
    const offer = this.offer;
    if (!userId || !offer) throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    const rows = await listDeviceRecoveryRequests(userId, this.assertCurrent);
    this.assertCurrent();
    const row = rows.find((entry) => entry.request_id === context.request_id);
    if (row?.status !== 'offered' || !sameRecoveryContext(row, context) || !sameOffer(row, offer))
      throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
    // Only this exact offered state permits the next attempt to access account keys.
    this.approvalNeedsReconciliation = false;
  }
  async confirmMatch(): Promise<boolean> {
    if (
      this.busy ||
      !this.offered ||
      Date.now() < this.view.retryAt ||
      this.view.status === 'submitted'
    )
      return false;
    this.busy = true;
    try {
      this.confirmDigest();
      const context = this.requireContext();
      const material = this.material;
      if (this.confirmedDigest !== material?.transcriptHash)
        throw new DeviceRecoveryError(RECOVERY_UPDATE_GUIDANCE);
      if (this.approvalNeedsReconciliation) await this.reconcileApprovalRetry(context);
      // Consent is the first operation permitted to access password/account key custody.
      this.assertCurrent();
      const wrappingKey = e2eeService.getWrappingKey();
      const wrapped = e2eeService.getWrappedPrivateKey();
      this.assertCurrent();
      if (!wrappingKey || !wrapped)
        throw new DeviceRecoveryError('E2EE keys not available. Please sign in again.');
      const bytes = await exportRecoveryAccountKey(wrapped, wrappingKey, this.assertCurrent);
      let payload: string;
      try {
        this.assertCurrent();
        payload = await encryptDeviceRecoveryPayload(material, bytes);
        this.assertCurrent();
      } finally {
        new Uint8Array(bytes).fill(0);
      }
      // A lost response may follow a committed approval; never retry custody without reconciliation.
      this.approvalNeedsReconciliation = true;
      const ack = await respond(
        context.request_id,
        {
          action: 'approve',
          protocol_version: 2,
          transcript_hash: material.transcriptHash,
          encrypted_payload: payload,
        },
        this.assertCurrent
      );
      this.assertCurrent();
      validateAcknowledgement(ack, context.request_id, 'approved');
      this.publish({
        status: 'submitted',
        fingerprint: '',
        confirmed: false,
        error: '',
        retryAt: 0,
      });
      this.dispose();
      return true;
    } catch (error) {
      this.confirmedDigest = '';
      this.view = { ...this.view, confirmed: false };
      this.fail(error);
      return false;
    } finally {
      this.busy = false;
    }
  }
}

/** Closing never asserts remote rejection: only a validated acknowledgement resolves a row. */
export async function rejectDeviceRecoveryRequest(
  request: ReviewableDeviceRecoveryRequest,
  assertCurrent: () => void
): Promise<void> {
  assertCurrent();
  const ack = await respond(
    request.request_id,
    { action: 'reject', protocol_version: 2 },
    assertCurrent
  );
  assertCurrent();
  validateAcknowledgement(ack, request.request_id, 'rejected');
}
