import { describe, it, expect, vi, beforeAll } from 'vitest';
import { generateKeyPairSync, sign as cryptoSign, type KeyObject } from 'node:crypto';
import { writeFile, mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Mutable holder so the mocked key module returns the in-test public key.
// vi.hoisted runs before the vi.mock factory, avoiding the TDZ that a plain
// outer `let` would hit under vi.mock hoisting.
const keyHolder = vi.hoisted(() => ({ publicKeyPem: '' }));

vi.mock('../../../src/main/linuxUpdatePublicKey', () => ({
  get LINUX_UPDATE_PUBLIC_KEY_PEM() {
    return keyHolder.publicKeyPem;
  },
}));

let privateKey: KeyObject;

beforeAll(() => {
  const kp = generateKeyPairSync('ed25519');
  privateKey = kp.privateKey;
  keyHolder.publicKeyPem = kp.publicKey.export({ type: 'spki', format: 'pem' }) as string;
});

async function writeArtifact(bytes: Buffer): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'linsig-'));
  const p = join(dir, 'ConcordVoice-1.2.3-linux-x64.AppImage');
  await writeFile(p, bytes);
  return p;
}

function sigFetch(sig: Buffer, status = 200) {
  return vi.fn(async () => ({
    ok: status >= 200 && status < 300,
    status,
    arrayBuffer: async () =>
      sig.buffer.slice(sig.byteOffset, sig.byteOffset + sig.byteLength) as ArrayBuffer,
  }));
}

describe('verifyLinuxArtifact', () => {
  it('returns verified:true for a valid signature', async () => {
    const { verifyLinuxArtifact } = await import('../../../src/main/verifyLinuxSignature');
    const bytes = Buffer.from('the real artifact bytes');
    const file = await writeArtifact(bytes);
    const sig = cryptoSign(null, bytes, privateKey);
    expect(await verifyLinuxArtifact(file, 'https://api/x.sig', sigFetch(sig))).toEqual({
      verified: true,
    });
  });

  it("flags a tampered artifact as kind:'tampered' (the tamper banner case)", async () => {
    const { verifyLinuxArtifact } = await import('../../../src/main/verifyLinuxSignature');
    const file = await writeArtifact(Buffer.from('tampered bytes'));
    const sig = cryptoSign(null, Buffer.from('original bytes'), privateKey);
    const r = await verifyLinuxArtifact(file, 'https://api/x.sig', sigFetch(sig));
    expect(r).toMatchObject({ verified: false, kind: 'tampered' });
  });

  it("flags a wrong-key signature as kind:'tampered'", async () => {
    const { verifyLinuxArtifact } = await import('../../../src/main/verifyLinuxSignature');
    const bytes = Buffer.from('artifact bytes');
    const file = await writeArtifact(bytes);
    const otherKey = generateKeyPairSync('ed25519').privateKey;
    const sig = cryptoSign(null, bytes, otherKey);
    const r = await verifyLinuxArtifact(file, 'https://api/x.sig', sigFetch(sig));
    expect(r).toMatchObject({ verified: false, kind: 'tampered' });
  });

  it("treats a non-2xx fetch as kind:'unavailable' (retryable, NOT a tamper warning)", async () => {
    const { verifyLinuxArtifact } = await import('../../../src/main/verifyLinuxSignature');
    const file = await writeArtifact(Buffer.from('artifact bytes'));
    const r = await verifyLinuxArtifact(file, 'https://api/x.sig', sigFetch(Buffer.alloc(64), 404));
    expect(r).toMatchObject({ verified: false, kind: 'unavailable' });
  });

  it("treats a wrong-length signature as kind:'unavailable' (malformed/corrupt, not proven tamper)", async () => {
    const { verifyLinuxArtifact } = await import('../../../src/main/verifyLinuxSignature');
    const file = await writeArtifact(Buffer.from('artifact bytes'));
    const r = await verifyLinuxArtifact(file, 'https://api/x.sig', sigFetch(Buffer.alloc(65)));
    expect(r).toMatchObject({ verified: false, kind: 'unavailable' });
  });

  it("treats a thrown fetch as kind:'unavailable' (fail-closed)", async () => {
    const { verifyLinuxArtifact } = await import('../../../src/main/verifyLinuxSignature');
    const file = await writeArtifact(Buffer.from('artifact bytes'));
    const throwingFetch = vi.fn(async () => {
      throw new Error('network down');
    });
    const r = await verifyLinuxArtifact(file, 'https://api/x.sig', throwingFetch);
    expect(r).toMatchObject({ verified: false, kind: 'unavailable' });
  });
});
