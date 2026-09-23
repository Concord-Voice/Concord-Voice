import { randomUUID } from 'node:crypto';

// Process-local route address for durable enforcement commands. It is never a
// lease: a crashed process deliberately leaves its durable rows pending.
export const voiceEnforcementNodeBootId = randomUUID();
