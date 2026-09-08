import { randomUUID } from 'node:crypto';
import { closeSync, fstatSync, ftruncateSync, openSync, statSync, writeSync } from 'node:fs';
import { logger } from './logger.js';

export const MEDIA_SECURITY_EVENT_LOG_PATH = '/var/log/concord-security/media-plane/events.jsonl';

const maxLineBytes = 2048;
const maxFileBytes = 20 * 1024 * 1024;
const maxUniqueEvents = 100;
const maxCount = 65535;
const retryDelayMs = 1000;

export type SecurityEventType =
  | 'authentication'
  | 'mfa'
  | 'session'
  | 'credential_epoch'
  | 'privileged_action'
  | 'audit'
  | 'media_admission'
  | 'media_authorization'
  | 'media_integrity'
  | 'security_control'
  | 'dependency'
  | 'pipeline_health';
export type SecurityEventOutcome = 'success' | 'failure' | 'denied' | 'degraded' | 'restored';
export type SecurityEventSeverity = 'informational' | 'low' | 'medium' | 'high' | 'critical';
export type SecurityReasonCode =
  | 'origin_rejected'
  | 'invalid_credentials'
  | 'admission_rejected'
  | 'admission_gate_inactive'
  | 'socket_rate_limited'
  | 'socket_handler_failed'
  | 'room_claim_conflict'
  | 'authorization_denied'
  | 'authorization_revoked'
  | 'identity_authority_mismatch'
  | 'identity_authority_missing'
  | 'crypto_version_invalid'
  | 'permission_denied'
  | 'revocation_enforced'
  | 'structural_limit_exceeded'
  | 'media_schema_rejected'
  | 'dependency_unavailable'
  | 'dependency_recovered'
  | 'writer_validation_drop'
  | 'writer_budget_drop'
  | 'writer_write_failed'
  | 'writer_recovered'
  | 'writer_size_refused';
export type SecurityAuthMethod =
  'password' | 'webauthn' | 'totp' | 'backup_code' | 'sso' | 'recovery' | 'session';
export type SecurityRouteTemplate =
  | 'socket.join'
  | 'socket.produce'
  | 'socket.consume'
  | 'socket.permissions_update'
  | 'socket.force_disconnect';

export interface SecurityEventInput {
  eventType: SecurityEventType;
  outcome: SecurityEventOutcome;
  severity: SecurityEventSeverity;
  reasonCode: SecurityReasonCode;
  authMethod?: SecurityAuthMethod;
  routeTemplate?: SecurityRouteTemplate;
  correlationRef?: string;
  evidenceRef?: string;
}

export type EmitSecurityEvent = (event: Readonly<SecurityEventInput>) => boolean;

export interface SecurityEventHealth {
  status: 'disabled' | 'healthy' | 'degraded';
  accepted: number;
  coalesced: number;
  dropped: number;
  validationFailures: number;
  internalFailures: number;
  writeFailures: number;
  staleFileRetries: number;
  sizeRefusals: number;
  pendingUnique: number;
}

type Pending = {
  event: SecurityEventInput;
  count: number;
  correlationRef: string;
  occurredAt: number;
};

type AppendResult = 'written' | 'retry' | 'discard';
type PartialWriteRepair = { fd: number; size: number };

const eventTypes = new Set<SecurityEventType>([
  'authentication',
  'mfa',
  'session',
  'credential_epoch',
  'privileged_action',
  'audit',
  'media_admission',
  'media_authorization',
  'media_integrity',
  'security_control',
  'dependency',
  'pipeline_health',
]);
const outcomes = new Set<SecurityEventOutcome>([
  'success',
  'failure',
  'denied',
  'degraded',
  'restored',
]);
const severities = new Set<SecurityEventSeverity>([
  'informational',
  'low',
  'medium',
  'high',
  'critical',
]);
const reasons = new Set<SecurityReasonCode>([
  'origin_rejected',
  'invalid_credentials',
  'admission_rejected',
  'admission_gate_inactive',
  'socket_rate_limited',
  'socket_handler_failed',
  'room_claim_conflict',
  'authorization_denied',
  'authorization_revoked',
  'identity_authority_mismatch',
  'identity_authority_missing',
  'crypto_version_invalid',
  'permission_denied',
  'revocation_enforced',
  'structural_limit_exceeded',
  'media_schema_rejected',
  'dependency_unavailable',
  'dependency_recovered',
  'writer_validation_drop',
  'writer_budget_drop',
  'writer_write_failed',
  'writer_recovered',
  'writer_size_refused',
]);
const authMethods = new Set<SecurityAuthMethod>([
  'password',
  'webauthn',
  'totp',
  'backup_code',
  'sso',
  'recovery',
  'session',
]);
const routes = new Set<SecurityRouteTemplate>([
  'socket.join',
  'socket.produce',
  'socket.consume',
  'socket.permissions_update',
  'socket.force_disconnect',
]);
const writerReasons = new Set<SecurityReasonCode>([
  'writer_validation_drop',
  'writer_budget_drop',
  'writer_write_failed',
  'writer_recovered',
  'writer_size_refused',
]);

function validRef(value: string): boolean {
  return /^[A-Za-z0-9_-]{16,64}$/.test(value);
}

function semanticKey(event: SecurityEventInput): string {
  return [
    event.eventType,
    event.outcome,
    event.severity,
    event.reasonCode,
    event.authMethod ?? '',
    event.routeTemplate ?? '',
    event.evidenceRef ?? '',
  ].join('|');
}

/** Bounded best-effort JSONL writer. It deliberately never changes a voice decision. */
export class SecurityEventWriter {
  private readonly path: string;
  private readonly enabled: boolean;
  private readonly now: () => number;
  private readonly newUUID: () => string;
  private readonly pending = new Map<string, Pending>();
  private bucket = -1;
  private closed = false;
  private closeDone = false;
  private failed = false;
  private timer: NodeJS.Timeout | undefined;
  private timerGeneration = 0;
  private partialWriteRepair: PartialWriteRepair | undefined;
  private readonly health: Omit<SecurityEventHealth, 'pendingUnique'>;

  constructor(options: {
    path?: string;
    enabled: boolean;
    now?: () => number;
    randomUUID?: () => string;
  }) {
    this.path = options.path ?? MEDIA_SECURITY_EVENT_LOG_PATH;
    this.enabled = options.enabled;
    this.now = options.now ?? Date.now;
    this.newUUID = options.randomUUID ?? randomUUID;
    this.health = {
      status: options.enabled ? 'healthy' : 'disabled',
      accepted: 0,
      coalesced: 0,
      dropped: 0,
      validationFailures: 0,
      internalFailures: 0,
      writeFailures: 0,
      staleFileRetries: 0,
      sizeRefusals: 0,
    };
  }

  emit(event: Readonly<SecurityEventInput>): boolean {
    try {
      if (!this.enabled || this.closed) return false;
      if (!this.valid(event)) {
        this.health.validationFailures++;
        return false;
      }
      const bucket = Math.floor(this.now() / 1000);
      if (this.bucket !== -1 && bucket !== this.bucket) this.flushPending();
      this.bucket = bucket;
      const key = semanticKey(event);
      const prior = this.pending.get(key);
      if (prior) {
        if (prior.count < maxCount) prior.count++;
        this.health.coalesced++;
        return true;
      }
      if (this.pending.size >= maxUniqueEvents) {
        this.health.dropped++;
        return false;
      }
      this.pending.set(key, {
        event: { ...event },
        count: 1,
        correlationRef: event.correlationRef ?? this.correlation(),
        occurredAt: bucket * 1000,
      });
      this.health.accepted++;
      this.armTimer(bucket);
      return true;
    } catch {
      this.recordInternalFailure();
      return false;
    }
  }

  flush(): void {
    try {
      this.flushPending();
    } catch {
      this.recordInternalFailure();
    }
  }

  private flushPending(): boolean {
    if (!this.enabled) return true;
    this.stopTimer();
    if (this.pending.size === 0) return true;
    for (const [key, pending] of this.pending) {
      const wasFailed = this.failed;
      const failures =
        this.health.writeFailures + this.health.internalFailures + this.health.sizeRefusals;
      const result = this.append(
        pending.event,
        pending.count,
        pending.correlationRef,
        pending.occurredAt
      );
      if (result === 'retry') {
        this.armRetryTimer();
        return false;
      }
      if (
        wasFailed &&
        failures ===
          this.health.writeFailures + this.health.internalFailures + this.health.sizeRefusals
      ) {
        this.failed = false;
        this.health.status = 'healthy';
        logger.info('Security event sink restored');
      }
      this.pending.delete(key);
      if (result === 'discard') this.health.dropped++;
    }
    this.bucket = -1;
    return true;
  }

  async close(): Promise<boolean> {
    if (this.closeDone) return true;
    this.closed = true;
    this.stopTimer();
    for (let attempt = 0; attempt < 2; attempt++) {
      if (this.flushPending() && this.pending.size === 0 && !this.failed) {
        this.closeDone = true;
        return true;
      }
      if (attempt === 0) await new Promise<void>((resolve) => setTimeout(resolve, retryDelayMs));
    }
    return false;
  }

  snapshot(): Readonly<SecurityEventHealth> {
    return { ...this.health, pendingUnique: this.pending.size };
  }

  private armTimer(bucket: number): void {
    if (this.timer) return;
    const delay = Math.max(1, (bucket + 1) * 1000 - this.now());
    const generation = ++this.timerGeneration;
    this.timer = setTimeout(() => {
      if (this.closed || generation !== this.timerGeneration) return;
      this.timer = undefined;
      this.flushPending();
    }, delay);
    this.timer.unref();
  }

  private armRetryTimer(): void {
    if (this.closed || this.timer) return;
    const generation = ++this.timerGeneration;
    this.timer = setTimeout(() => {
      if (this.closed || generation !== this.timerGeneration) return;
      this.timer = undefined;
      this.flushPending();
    }, retryDelayMs);
    this.timer.unref();
  }

  private stopTimer(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    this.timerGeneration++;
  }

  private valid(event: Readonly<SecurityEventInput>): event is SecurityEventInput {
    if (
      Object.keys(event).some(
        (key) =>
          ![
            'eventType',
            'outcome',
            'severity',
            'reasonCode',
            'authMethod',
            'routeTemplate',
            'correlationRef',
            'evidenceRef',
          ].includes(key)
      )
    )
      return false;
    if (
      !eventTypes.has(event.eventType) ||
      !outcomes.has(event.outcome) ||
      !severities.has(event.severity) ||
      !reasons.has(event.reasonCode) ||
      event.eventType === 'pipeline_health'
    )
      return false;
    if (writerReasons.has(event.reasonCode)) return false;
    return (
      (!event.authMethod || authMethods.has(event.authMethod)) &&
      (!event.routeTemplate || routes.has(event.routeTemplate)) &&
      (!event.correlationRef || validRef(event.correlationRef)) &&
      (!event.evidenceRef ||
        /^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/i.test(event.evidenceRef))
    );
  }

  private correlation(): string {
    return this.newUUID().replaceAll('-', '');
  }

  private recordWriteFailure(): false {
    if (!this.failed) logger.error('Security event sink write failed');
    this.health.writeFailures++;
    this.health.status = 'degraded';
    this.failed = true;
    return false;
  }

  private recordInternalFailure(): void {
    if (!this.failed) logger.error('Security event sink write failed');
    this.health.internalFailures++;
    this.health.status = 'degraded';
    this.failed = true;
  }

  private closeDescriptor(fd: number): boolean {
    try {
      closeSync(fd);
      return true;
    } catch {
      return this.recordWriteFailure();
    }
  }

  private writeDescriptor(fd: number, payload: Buffer): number {
    return writeSync(fd, payload);
  }

  private truncateDescriptor(fd: number, size: number): void {
    ftruncateSync(fd, size);
  }

  private repairPartialWrite(): boolean {
    if (!this.partialWriteRepair) return true;
    const repair = this.partialWriteRepair;
    try {
      this.truncateDescriptor(repair.fd, repair.size);
    } catch {
      return this.recordWriteFailure();
    }
    this.partialWriteRepair = undefined;
    return this.closeDescriptor(repair.fd);
  }

  private rollbackPartialWrite(fd: number, size: number): boolean {
    try {
      this.truncateDescriptor(fd, size);
      return true;
    } catch {
      this.partialWriteRepair = { fd, size };
      return false;
    }
  }

  private append(
    event: SecurityEventInput,
    count: number,
    correlationRef: string,
    occurredAt: number
  ): AppendResult {
    if (!this.repairPartialWrite()) return 'retry';
    const payload = this.createPayload(event, count, correlationRef, occurredAt);
    if (!payload) return 'discard';
    return this.writePayload(payload);
  }

  private createPayload(
    event: SecurityEventInput,
    count: number,
    correlationRef: string,
    occurredAt: number
  ): Buffer | undefined {
    let line: string;
    let eventId: string;
    let occurredAtISO: string;
    try {
      eventId = this.newUUID();
      occurredAtISO = new Date(occurredAt).toISOString();
      line = `${JSON.stringify({
        schema_version: 'security-event.v1',
        event_id: eventId,
        occurred_at: occurredAtISO,
        service: 'media-plane',
        event_type: event.eventType,
        outcome: event.outcome,
        severity: event.severity,
        reason_code: event.reasonCode,
        ...(event.authMethod ? { auth_method: event.authMethod } : {}),
        ...(event.routeTemplate ? { route_template: event.routeTemplate } : {}),
        count,
        correlation_ref: correlationRef,
        ...(event.evidenceRef ? { evidence_ref: event.evidenceRef } : {}),
      })}\n`;
    } catch {
      this.recordInternalFailure();
      return undefined;
    }
    if (!/^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/i.test(eventId)) {
      this.recordInternalFailure();
      return undefined;
    }
    if (Buffer.byteLength(line) > maxLineBytes) {
      this.recordSizeRefusal();
      return undefined;
    }
    return Buffer.from(line);
  }

  private writePayload(payload: Buffer): AppendResult {
    for (let attempt = 0; attempt < 2; attempt++) {
      const result = this.tryWritePayload(payload);
      if (result) return result;
    }
    this.recordWriteFailure();
    return 'retry';
  }

  private repairFailedWrite(
    fd: number | undefined,
    openedSize: number | undefined
  ): number | undefined {
    if (fd === undefined || openedSize === undefined) return fd;
    try {
      if (fstatSync(fd).size > openedSize && !this.rollbackPartialWrite(fd, openedSize)) {
        return undefined;
      }
      return fd;
    } catch {
      this.partialWriteRepair = { fd, size: openedSize };
      return undefined;
    }
  }

  private tryWritePayload(payload: Buffer): AppendResult | undefined {
    let fd: number | undefined;
    let openedSize: number | undefined;
    let result: AppendResult | undefined;
    try {
      fd = openSync(this.path, 'a', 0o640);
      const opened = fstatSync(fd);
      openedSize = opened.size;
      const current = statSync(this.path);
      if (opened.ino !== current.ino || opened.dev !== current.dev) {
        this.health.staleFileRetries++;
        result = undefined;
      } else if (opened.size + payload.length > maxFileBytes) {
        this.recordSizeRefusal();
        result = 'discard';
      } else {
        const written = this.writeDescriptor(fd, payload);
        if (written === payload.length) {
          result = 'written';
        } else {
          if (!this.rollbackPartialWrite(fd, opened.size)) fd = undefined;
          this.recordWriteFailure();
          result = 'retry';
        }
      }
    } catch {
      fd = this.repairFailedWrite(fd, openedSize);
      this.recordWriteFailure();
      result = 'retry';
    }
    if (fd !== undefined && !this.closeDescriptor(fd)) return 'retry';
    return result;
  }

  private recordSizeRefusal(): void {
    if (!this.failed) logger.error('Security event sink size limit refused a record');
    this.health.sizeRefusals++;
    this.health.status = 'degraded';
    this.failed = true;
  }
}
