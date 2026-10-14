import { isCanonicalEnforcementUUID } from './enforcementCommand.js';

export interface VoiceCommandOwnerProof {
  streamCreatedAt: string;
  ownerStoreCreatedAt: string;
  initializingRevision: string;
}

interface OwnerIdentity {
  version: 1;
  streamGeneration: string;
  nodeBootId: string | null;
}

type RuntimeOwnerRecord = OwnerIdentity & {
  phase: 'held' | 'released';
  proof: VoiceCommandOwnerProof;
  birth?: never;
};

export type VoiceCommandOwnerRecord =
  | RuntimeOwnerRecord
  | (OwnerIdentity & {
      phase: 'initializing';
      birth: { streamCreatedAt: string; ownerStoreCreatedAt: string };
      proof?: never;
    });

// The broker adapter verifies the immutable proof against BOTH current stream
// creation identities on read and before/after CAS. Missing resources or changed
// birth identities reject; this port never creates, deletes or repairs resources.
export interface VoiceCommandOwnerStore {
  read(): Promise<{ value: VoiceCommandOwnerRecord; revision: number } | null>;
  compareAndSet(revision: number, value: VoiceCommandOwnerRecord): Promise<number>;
  // A live broker adapter coordinates its scheduled renewer before release.
  stopRenewalBeforeRelease?(): Promise<void>;
}

interface OwnerOptions {
  store: VoiceCommandOwnerStore;
  streamGeneration: string;
  nodeBootId: string;
  now: () => number;
  onInvalidated: () => void;
}

const apiDeadlineMs = 2_000;
const localValidityMs = 5_000;

function authorityError(): Error {
  return new Error('Voice command ownership unavailable');
}

function object(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function fields(value: Record<string, unknown>, names: string[]): boolean {
  return (
    Object.keys(value).length === names.length && names.every((key) => Object.hasOwn(value, key))
  );
}

function canonicalCreatedAt(value: unknown): value is string {
  if (
    typeof value !== 'string' ||
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{0,8}[1-9])?Z$/.test(value)
  )
    return false;
  const date = new Date(`${value.slice(0, 19)}Z`);
  return Number.isFinite(date.getTime()) && date.toISOString().slice(0, 19) === value.slice(0, 19);
}

function validProof(value: unknown): value is VoiceCommandOwnerProof {
  return (
    object(value) &&
    fields(value, ['streamCreatedAt', 'ownerStoreCreatedAt', 'initializingRevision']) &&
    canonicalCreatedAt(value.streamCreatedAt) &&
    canonicalCreatedAt(value.ownerStoreCreatedAt) &&
    typeof value.initializingRevision === 'string' &&
    /^[1-9]\d{0,18}$/.test(value.initializingRevision) &&
    BigInt(value.initializingRevision) <= 9223372036854775807n
  );
}

function validRecord(value: unknown, generation: string): value is VoiceCommandOwnerRecord {
  if (
    !object(value) ||
    value.version !== 1 ||
    value.streamGeneration !== generation ||
    (value.nodeBootId !== null && !isCanonicalEnforcementUUID(value.nodeBootId))
  )
    return false;
  if (value.phase === 'initializing') {
    return (
      fields(value, ['version', 'streamGeneration', 'phase', 'nodeBootId', 'birth']) &&
      value.nodeBootId === null &&
      object(value.birth) &&
      fields(value.birth, ['streamCreatedAt', 'ownerStoreCreatedAt']) &&
      canonicalCreatedAt(value.birth.streamCreatedAt) &&
      canonicalCreatedAt(value.birth.ownerStoreCreatedAt)
    );
  }
  return (
    (value.phase === 'held' || value.phase === 'released') &&
    fields(value, ['version', 'streamGeneration', 'phase', 'nodeBootId', 'proof']) &&
    (value.phase !== 'held' || value.nodeBootId !== null) &&
    validProof(value.proof)
  );
}

function sameProof(first: VoiceCommandOwnerProof, second: VoiceCommandOwnerProof): boolean {
  return (
    first.streamCreatedAt === second.streamCreatedAt &&
    first.ownerStoreCreatedAt === second.ownerStoreCreatedAt &&
    first.initializingRevision === second.initializingRevision
  );
}

export function decodeVoiceCommandOwnerRecord(
  value: unknown,
  generation: string
): VoiceCommandOwnerRecord {
  if (!isCanonicalEnforcementUUID(generation) || !validRecord(value, generation)) {
    throw authorityError();
  }
  return value;
}

/** Local authority may expire; permanent held ownership never expires. */
export class VoiceCommandOwner {
  readonly streamGeneration: string;
  readonly nodeBootId: string;
  private readonly options: OwnerOptions;
  private revision: number | undefined;
  private proof: VoiceCommandOwnerProof | undefined;
  private state: 'idle' | 'held' | 'invalid' | 'released' = 'idle';
  private validUntil = Number.NEGATIVE_INFINITY;
  private lastTime = Number.NEGATIVE_INFINITY;
  private epoch = 0;
  private notified = false;
  private tail: Promise<void> = Promise.resolve();

  constructor(options: OwnerOptions) {
    if (
      !isCanonicalEnforcementUUID(options.streamGeneration) ||
      !isCanonicalEnforcementUUID(options.nodeBootId)
    )
      throw authorityError();
    this.options = options;
    this.streamGeneration = options.streamGeneration;
    this.nodeBootId = options.nodeBootId;
  }

  valid(): boolean {
    if (this.state !== 'held') return false;
    const now = this.time();
    if (now === undefined || now >= this.validUntil) {
      this.invalidate();
      return false;
    }
    return true;
  }

  authorityEpoch(): number {
    return this.epoch;
  }

  acquire(): Promise<void> {
    return this.serialize(async () => {
      if (this.state !== 'idle' && this.state !== 'released') throw authorityError();
      await this.grant(async (current) => {
        const record = await this.read();
        current();
        if (record.value.phase !== 'released' || !record.value.proof) throw authorityError();
        if (this.proof && !sameProof(this.proof, record.value.proof)) throw authorityError();
        this.proof = { ...record.value.proof };
        return this.options.store.compareAndSet(record.revision, this.heldRecord());
      });
    });
  }

  // A dispatcher effect needs a fresh, exact owner read without advancing the
  // revision or extending local validity. Serialize with renew/release CAS.
  confirmContinuity(): Promise<void> {
    return this.serialize(async () => {
      if (!this.valid() || this.revision === undefined) throw authorityError();
      const epoch = this.epoch;
      await this.bounded(async (current) => {
        const record = await this.read();
        current();
        this.requireOwnHeld(record.value);
        if (record.revision !== this.revision) throw authorityError();
      }, epoch);
    });
  }

  renew(): Promise<void> {
    return this.serialize(async () => {
      if (!this.valid() || this.revision === undefined) throw authorityError();
      await this.grant(async (current) => {
        // Read validates resource birth and the exact permanent record, rather
        // than treating a successful write to a recreated bucket as continuity.
        const record = await this.read();
        current();
        this.requireOwnHeld(record.value);
        if (record.revision !== this.revision) throw authorityError();
        return this.options.store.compareAndSet(record.revision, this.heldRecord());
      });
    });
  }

  // The caller must await quarantine and all local media terminalization before
  // this method. It cannot adopt another boot or a new resource incarnation.
  recoverAfterTeardown(): Promise<void> {
    return this.serialize(async () => {
      if (this.state !== 'invalid' || !this.proof) throw authorityError();
      await this.grant(async (current) => {
        const record = await this.read();
        current();
        this.requireOwnHeld(record.value);
        return this.options.store.compareAndSet(record.revision, this.heldRecord());
      });
    });
  }

  async release(): Promise<void> {
    // Stop before joining the owner tail: an in-flight renewal may itself be
    // waiting for that tail and must settle before release invalidates authority.
    await this.options.store.stopRenewalBeforeRelease?.();
    return this.serialize(async () => {
      const revision = this.revision;
      this.invalidate();
      if (revision === undefined || !this.proof) throw authorityError();
      const epoch = this.epoch;
      await this.bounded(async (current) => {
        const record = await this.read();
        current();
        this.requireOwnHeld(record.value);
        if (record.revision !== revision) throw authorityError();
        const next = await this.options.store.compareAndSet(revision, {
          ...this.heldRecord(),
          phase: 'released',
        });
        current();
        this.requireRevision(next, revision);
        this.revision = next;
        this.state = 'released';
      }, epoch);
    });
  }

  private serialize(operation: () => Promise<void>): Promise<void> {
    const result = this.tail.then(operation);
    this.tail = result.catch(() => undefined);
    return result;
  }

  private async grant(operation: (current: () => void) => Promise<number>): Promise<void> {
    const start = this.time();
    if (start === undefined) {
      this.invalidate();
      throw authorityError();
    }
    const epoch = this.epoch;
    await this.bounded(async (current) => {
      const revision = await operation(current);
      current();
      this.requireRevision(revision, this.revision);
      const now = this.time();
      if (now === undefined || now >= start + localValidityMs) throw authorityError();
      this.revision = revision;
      this.validUntil = start + localValidityMs;
      this.state = 'held';
      this.notified = false;
    }, epoch);
  }

  private async bounded(
    operation: (current: () => void) => Promise<void>,
    epoch: number
  ): Promise<void> {
    const startedAt = this.time();
    if (startedAt === undefined) {
      this.invalidate();
      throw authorityError();
    }
    let active = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const current = () => {
      const now = this.time();
      if (!active || this.epoch !== epoch || now === undefined || now >= startedAt + apiDeadlineMs)
        throw authorityError();
    };
    const deadline = new Promise<never>((_resolve, reject) => {
      timer = setTimeout(() => {
        active = false;
        this.invalidate();
        reject(authorityError());
      }, apiDeadlineMs);
    });
    try {
      await Promise.race([operation(current), deadline]);
    } catch {
      this.invalidate();
      throw authorityError();
    } finally {
      active = false;
      clearTimeout(timer);
    }
  }

  private async read(): Promise<{ value: VoiceCommandOwnerRecord; revision: number }> {
    const record = await this.options.store.read();
    if (!record || !validRecord(record.value, this.streamGeneration)) throw authorityError();
    this.requireRevision(record.revision);
    return record;
  }

  private heldRecord(): RuntimeOwnerRecord {
    if (!this.proof) throw authorityError();
    return {
      version: 1,
      streamGeneration: this.streamGeneration,
      phase: 'held',
      nodeBootId: this.options.nodeBootId,
      proof: { ...this.proof },
    };
  }

  private requireOwnHeld(record: VoiceCommandOwnerRecord): void {
    if (
      record.phase !== 'held' ||
      record.nodeBootId !== this.options.nodeBootId ||
      !record.proof ||
      !this.proof ||
      !sameProof(record.proof, this.proof)
    )
      throw authorityError();
  }

  private requireRevision(revision: number, previous = 0): void {
    if (!Number.isSafeInteger(revision) || revision <= previous) throw authorityError();
  }

  private time(): number | undefined {
    let now: number;
    try {
      now = this.options.now();
    } catch {
      return undefined;
    }
    if (!Number.isFinite(now) || now < this.lastTime) return undefined;
    this.lastTime = now;
    return now;
  }

  // Connection/consumer uncertainty revokes local authority without changing the
  // permanent held record. Only verified teardown may precede later recovery.
  invalidate(): void {
    this.state = 'invalid';
    this.validUntil = Number.NEGATIVE_INFINITY;
    this.epoch++;
    if (this.notified) return;
    this.notified = true;
    try {
      this.options.onInvalidated();
    } catch {
      // A quarantine callback failure cannot restore authority.
    }
  }
}
