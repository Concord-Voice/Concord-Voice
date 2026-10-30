import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import ts from 'typescript';
import { expect, vi } from 'vitest';
import { EnforcementAvailability } from '../../src/lib/enforcementAvailability.js';
import { VoiceAuthConnections } from '../../src/lib/voiceAuthConnection.js';
import { VoiceCommandOwner, type VoiceCommandOwnerRecord } from '../../src/lib/voiceCommandOwner.js';
import { VoiceCommandDispatcher } from '../../src/lib/voiceCommandDispatcher.js';
import { VoiceEnforcementExpiryFence, VoiceEnforcementLease } from '../../src/lib/voiceEnforcementLease.js';

import type { NatsService } from '../../src/lib/nats.js';
const boot = '55555555-5555-4555-8555-555555555555';
const generation = '11111111-1111-4111-8111-111111111111';
function deferred() {
  let resolve!: () => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<void>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
async function settle() { for (let i = 0; i < 50; i++) await Promise.resolve(); }

// Execute the contiguous production construction block, including any newly
// introduced episode locals. Selecting exactly one pair of sibling statements
// prevents a renamed/moved seam silently falling back to copied fixture logic.
function productionBlock(): string {
  const source = ts.createSourceFile('index.ts', readFileSync(new URL('../../src/index.ts', import.meta.url), 'utf8'),
    ts.ScriptTarget.ES2022, true, ts.ScriptKind.TS);
  const named = (name: string): ts.VariableStatement => {
    const matches: ts.VariableStatement[] = [];
    const visit = (node: ts.Node) => {
      if (ts.isVariableStatement(node) && node.declarationList.declarations.some(declaration =>
        ts.isIdentifier(declaration.name) && declaration.name.text === name)) matches.push(node);
      ts.forEachChild(node, visit);
    };
    visit(source);
    expect(matches, `exact production declaration ${name}`).toHaveLength(1);
    return matches[0];
  };
  const start = named('voiceEnforcementExpiryFence');
  const end = named('voiceCommandDispatcher');
  expect(start.parent).toBe(end.parent);
  expect(ts.isBlock(start.parent)).toBe(true);
  const code = source.text.slice(start.getStart(source), end.end);
  return ts.transpileModule(code, { compilerOptions: {
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022,
  } }).outputText;
}

export async function createRecoveryCallerFixture(externalNats?: NatsService) {
  let mono = 0;
  vi.spyOn(performance, 'now').mockImplementation(() => mono);
  const proofs: ReturnType<typeof deferred>[] = [];
  const lease = new VoiceEnforcementLease(() => mono);
  const owners = new VoiceAuthConnections({ nodeBootId: boot, secret: 'test-caller-key',
    controlPlaneUrl: 'https://cp.test.invalid', fetch: async () => { throw new Error('Unexpected HTTP boundary'); } });
  let failure!: () => void;
  let observe!: (ready: boolean) => void;
  let value: VoiceCommandOwnerRecord = { version: 1, streamGeneration: generation, phase: 'released', nodeBootId: null,
    proof: { streamCreatedAt: '2026-10-13T18:07:41.123456789Z',
      ownerStoreCreatedAt: '2026-10-13T18:07:41.987654321Z', initializingRevision: '1' } };
  let revision = 2;
  let actualOwner!: VoiceCommandOwner;
  const continuity = vi.fn(async () => {});
  const isolatedNats = {
    observeRequiredSubscriptions: (callback: typeof observe) => { observe = callback; return () => {}; },
    createVoiceCommandOwner: async (options: { onInvalidated: () => void }) => {
      actualOwner = new VoiceCommandOwner({ streamGeneration: generation, nodeBootId: boot,
        now: () => mono, onInvalidated: options.onInvalidated,
        store: {
          read: async () => ({ value, revision }),
          compareAndSet: async (expected, next) => {
            if (expected !== revision) throw new Error('fixture CAS conflict');
            value = next; return ++revision;
          },
        } });
      await actualOwner.acquire(); return actualOwner;
    },
    requireVoiceCommandAuthority: () => { if (!actualOwner.valid()) throw new Error('Owner unavailable'); },
    confirmVoiceCommandContinuity: continuity,
    requiredSubscriptionsReady: () => true,
    voiceCommandReady: () => actualOwner.valid(),
  };
  const natsService = externalNats ? {
    observeRequiredSubscriptions: externalNats.observeRequiredSubscriptions.bind(externalNats),
    createVoiceCommandOwner: async (options: { nodeBootId: string; onInvalidated: () => void }) => {
      actualOwner = await externalNats.createVoiceCommandOwner(options); return actualOwner;
    },
    requireVoiceCommandAuthority: externalNats.requireVoiceCommandAuthority.bind(externalNats),
    confirmVoiceCommandContinuity: externalNats.confirmVoiceCommandContinuity.bind(externalNats),
    requiredSubscriptionsReady: externalNats.requiredSubscriptionsReady.bind(externalNats),
    voiceCommandReady: externalNats.voiceCommandReady.bind(externalNats),
  } : isolatedNats;
  const roomManager = {
    closeAll: () => { const proof = deferred(); proofs.push(proof); return proof.promise; },
    setNativeTerminationFailureHandler: (callback: typeof failure) => { failure = callback; },
  };
  const sockets = new Map();
  // Missing external bindings throw at evaluation/use. No proxy supplies defaults
  // for unknown dependencies introduced into the production block.
  const runtime = await runInNewContext(`(async () => { ${productionBlock()}
    return { fence: voiceEnforcementExpiryFence, availability: enforcementAvailability,
      quarantine: quarantineVoiceCommands, dispatcher: voiceCommandDispatcher };
  })()`, {
    VoiceEnforcementExpiryFence, EnforcementAvailability, VoiceCommandDispatcher,
    voiceEnforcementLease: lease, voiceAuthConnections: owners,
    voiceEnforcementNodeBootId: boot, roomManager, natsService,
    io: { sockets: { sockets } }, snapshotVoiceEnforcementSessions: () => [],
    voiceEnforcementReleaseQueue: { holdAll: () => {} },
    mediasoupService: { isReady: () => true }, shuttingDown: false,
    securityEvents: { emit: () => {} }, logger: { error: () => {} },
  }) as { fence: VoiceEnforcementExpiryFence; availability: EnforcementAvailability;
    quarantine: (reason: string, episode: object) => Promise<void>; dispatcher: VoiceCommandDispatcher };
  return { ...runtime, lease, owners, proofs, continuity, owner: actualOwner,
    failure: () => failure(), available: (ready: boolean) => observe(ready),
    now: (value: number) => { mono = value; },
    finish: async () => { for (const proof of proofs) proof.resolve(); await settle(); },
  };
}

