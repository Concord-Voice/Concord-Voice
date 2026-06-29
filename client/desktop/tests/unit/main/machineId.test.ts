// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from 'vitest';

const { files, mkdirCalls, randomUUID, writes } = vi.hoisted(() => {
  let counter = 0;
  return {
    files: new Map<string, string>(),
    mkdirCalls: [] as string[],
    randomUUID: vi.fn(() => `machine-${++counter}`),
    writes: [] as unknown[][],
  };
});

vi.mock('electron', () => ({
  app: { getPath: () => '/tmp/td' },
}));

vi.mock('node:crypto', async () => {
  const actual = await vi.importActual<typeof import('node:crypto')>('node:crypto');
  return {
    ...actual,
    randomUUID,
  };
});

vi.mock('fs', () => ({
  default: {
    mkdirSync: (path: string) => {
      mkdirCalls.push(path);
    },
    readFileSync: (path: string) => {
      const value = files.get(path);
      if (value === undefined) throw new Error('ENOENT');
      return value;
    },
    writeFileSync: (...args: unknown[]) => {
      writes.push(args);
      files.set(args[0] as string, args[1] as string);
    },
  },
  mkdirSync: (path: string) => {
    mkdirCalls.push(path);
  },
  readFileSync: (path: string) => {
    const value = files.get(path);
    if (value === undefined) throw new Error('ENOENT');
    return value;
  },
  writeFileSync: (...args: unknown[]) => {
    writes.push(args);
    files.set(args[0] as string, args[1] as string);
  },
}));

async function loadMachineIdModule() {
  vi.resetModules();
  return import('@/main/machineId');
}

describe('machineId', () => {
  beforeEach(() => {
    files.clear();
    mkdirCalls.length = 0;
    randomUUID.mockClear();
    writes.length = 0;
  });

  it('keeps the SaaS machine ID at the pinned userData root', async () => {
    const { getMachineId } = await loadMachineIdModule();

    const first = getMachineId('https://api.concordvoice.chat');
    const second = getMachineId('https://api.concordvoice.chat/');

    expect(first).toBe('machine-1');
    expect(second).toBe(first);
    expect(writes).toEqual([
      ['/tmp/td/machine-id.json', JSON.stringify({ id: 'machine-1' }), 'utf-8'],
    ]);
  });

  it('uses a separate machine ID file for each self-hosted origin profile', async () => {
    const { getMachineId } = await loadMachineIdModule();

    const homelab = getMachineId('https://homelab.lan');
    const homelabAgain = getMachineId('https://homelab.lan/');
    const workshop = getMachineId('https://workshop.lan');

    expect(homelab).toBe('machine-2');
    expect(homelabAgain).toBe(homelab);
    expect(workshop).toBe('machine-3');
    expect(writes.map((call) => call[0] as string)).toEqual([
      expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/machine-id\.json$/),
      expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}\/machine-id\.json$/),
    ]);
    expect(writes[0][0]).not.toBe(writes[1][0]);
    expect(mkdirCalls).toEqual([
      expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}$/),
      expect.stringMatching(/^\/tmp\/td\/profiles\/[0-9a-f]{64}$/),
    ]);
  });
});
