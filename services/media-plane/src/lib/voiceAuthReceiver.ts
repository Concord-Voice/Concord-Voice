import {
  decodeCommand,
  verifyCommand,
  signAuthAck,
  signEpochAck,
  signHealthAck,
  unicodeOK,
} from './voiceAuthProtocol.js';
import type { Command } from './voiceAuthProtocol.js';
import { logger } from './logger.js';

export type RawRequestReceiver = (
  raw: Uint8Array,
  respond: (response: Uint8Array) => void
) => void;

/** These callbacks receive only codec-owned, proof-verified, same-boot commands. */
export interface VoiceAuthReceiverAdapters {
  /**
   * Synchronously fence the exact private handle and return its ONE retained task.
   * Resolve only after native/in-flight proof AND signed durable release. Missing
   * ownership is not proof: return undefined when that witness is unavailable.
   * The owner retains/reuses the task even when this receiver has no waiter slot.
   */
  fenceAndJoin?: (command: Command) => Promise<void> | undefined;
  /** Bounded synchronous readiness snapshot; never await teardown, SQL or flush. */
  healthSnapshot?: (command: Command) => boolean;
}

export interface LegacyVoiceReceiver {
  verify(data: Record<string, unknown>): 'terminal' | 'health' | undefined;
  terminalKey(data: Record<string, unknown>): string;
  terminal(data: Record<string, unknown>): Promise<void>;
  acknowledge(data: Record<string, unknown>): Record<string, unknown> | undefined;
  health(data: Record<string, unknown>): Promise<Record<string, unknown> | undefined>;
}

type Completion = { result?: boolean; waiters: Set<(success: boolean) => void> };
const TERMINAL_WAITERS = 63;
const ATTEMPT_MS = 5_000;

function reject(): never {
  throw new Error('Invalid exact enforcement envelope');
}

// Legacy envelopes are flat. Scan string tokens before JSON.parse can erase
// duplicate/escaped-equivalent names; new recursive variants use the codec.
function legacyEnvelope(text: string, value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) reject();
  const data = value as Record<string, unknown>;
  const keys = new Set<string>();
  for (const match of text.matchAll(/"(?:[^"\\]|\\[\s\S])*"/g)) {
    const decoded: unknown = JSON.parse(match[0]);
    if (typeof decoded !== 'string' || !unicodeOK(decoded)) reject();
    const isKey = /^[\x20\t\r\n]*:/.test(text.slice(match.index + match[0].length));
    if (isKey && keys.has(decoded)) reject();
    if (isKey) keys.add(decoded);
  }
  if (Object.values(data).some((field) => field === null || typeof field === 'object')) reject();
  return data;
}

/**
 * Invariant: at most 63 retained attempt callbacks/timers, plus one health lane.
 * Timeout detaches a waiter, not its identity-owned terminal task. One observer
 * per task prevents repeated timed-out attempts accumulating Promise reactions.
 */
export function createVoiceAuthReceiver(
  sharedSecret: string,
  localNodeBootID: string,
  legacy: LegacyVoiceReceiver,
  adapters: VoiceAuthReceiverAdapters = {}
): RawRequestReceiver {
  const secret = Buffer.from(sharedSecret, 'utf8');
  const completions = new WeakMap<Promise<void>, Completion>();
  const legacyTasks = new Map<string, Promise<void>>();
  let terminalWaiters = 0;
  let healthBusy = false;

  function observe(task: Promise<void>): Completion {
    const existing = completions.get(task);
    if (existing) return existing;
    const state: Completion = { waiters: new Set() };
    completions.set(task, state);
    const finish = (success: boolean) => {
      state.result = success;
      for (const waiter of state.waiters) waiter(success);
      state.waiters.clear();
    };
    void task.then(() => finish(true), () => finish(false));
    return state;
  }

  function wait(task: Promise<void>, reply: () => void): void {
    const state = observe(task);
    if (terminalWaiters >= TERMINAL_WAITERS) return;
    terminalWaiters++;
    let done = false;
    const finish = (success: boolean) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      state.waiters.delete(finish);
      terminalWaiters--;
      if (success) {
        try { reply(); } catch { /* A failed reply never changes task ownership. */ }
      }
    };
    const timer = setTimeout(() => finish(false), ATTEMPT_MS);
    if (state.result === undefined) state.waiters.add(finish);
    else finish(state.result);
  }

  function receiveNew(command: Command, respond: (response: Uint8Array) => void): void {
    verifyCommand(command, secret, BigInt(Math.floor(Date.now() / 1000)));
    const health = command.health();
    const boot = health?.nodeBootId ?? command.auth()?.target.nodeBootId ?? command.epoch()?.target.nodeBootId;
    if (boot !== localNodeBootID) return;
    if (health) {
      // New health is entirely synchronous and cannot wait behind legacy recovery.
      const ready = adapters.healthSnapshot?.(command) === true;
      const granted = health.activation === 'active' && health.admissionAllowed && ready;
      respond(signHealthAck(command, granted, secret));
      return;
    }
    const task = adapters.fenceAndJoin?.(command);
    if (!task) return;
    wait(task, () => respond(command.auth() ? signAuthAck(command, secret) : signEpochAck(command, secret)));
  }

  function receiveLegacy(data: Record<string, unknown>, respond: (response: Uint8Array) => void): void {
    const kind = legacy.verify(data);
    if (kind === 'health') {
      if (healthBusy) return;
      healthBusy = true;
      // Old health retains its existing asynchronous recovery contract. It may
      // occupy this compatibility lane only once; protocol-4 health never joins it.
      let reply: ((response: Uint8Array) => void) | undefined = respond;
      const timer = setTimeout(() => { reply = undefined; }, ATTEMPT_MS);
      void legacy.health(data).then((result) => {
        if (result !== undefined) reply?.(Buffer.from(JSON.stringify(result)));
      }).catch(() => {}).finally(() => {
        clearTimeout(timer);
        healthBusy = false;
      });
      return;
    }
    if (kind !== 'terminal') return;
    const key = legacy.terminalKey(data);
    let task = legacyTasks.get(key);
    if (!task) {
      // Existing room teardown has no connection-handle owner. Keep its retained
      // work bounded until task 5b supplies the shared identity owner.
      if (terminalWaiters >= TERMINAL_WAITERS || legacyTasks.size >= TERMINAL_WAITERS) return;
      task = legacy.terminal(data);
      legacyTasks.set(key, task);
      void task.then(() => legacyTasks.delete(key), () => legacyTasks.delete(key));
    }
    wait(task, () => {
      const result = legacy.acknowledge(data);
      if (result !== undefined) respond(Buffer.from(JSON.stringify(result)));
    });
  }

  return (raw, respond) => {
    try {
      if (raw.byteLength > 4096) return;
      const text = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(raw);
      const value: unknown = JSON.parse(text);
      if (typeof value !== 'object' || value === null || Array.isArray(value)) return;
      // Presence takes precedence even for an unknown, null or malformed schema.
      if (Object.hasOwn(value, 'schema')) receiveNew(decodeCommand(raw), respond);
      else receiveLegacy(legacyEnvelope(text, value), respond);
    } catch {
      logger.warn('Rejected exact enforcement request', { reason: 'invalid_or_unavailable' });
    }
  };
}
