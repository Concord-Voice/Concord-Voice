import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  uploadAttachmentChunked,
  type UploadSessionContext,
} from '@/renderer/services/attachmentUploadSession';
import {
  CHUNK_PLAINTEXT_BYTES,
  ENVELOPE_HEADER_BYTES,
  expectedBlobLength,
} from '@/renderer/utils/attachmentChunkedCrypto';

const apiFetch = vi.hoisted(() => vi.fn());
vi.mock('@/renderer/services/apiClient', () => ({ apiFetch }));

const aesKey = () =>
  crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);

const ctx: UploadSessionContext = {
  channelId: 'chan-1',
  keyVersion: 3,
  fileType: 'file',
  mimeType: 'application/octet-stream',
};

const fileOf = (bytes: number) =>
  new File([new Uint8Array(bytes)], 'a.bin', { type: 'application/octet-stream' });

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

/** Every apiFetch call, as `METHOD path`. */
const calls = () => apiFetch.mock.calls.map((c) => `${(c[1]?.method ?? 'GET') as string} ${c[0]}`);

const bodyOf = (i: number): Uint8Array => {
  const b = apiFetch.mock.calls[i][1].body as ArrayBuffer | Uint8Array;
  return b instanceof Uint8Array ? b : new Uint8Array(b);
};

const initOK = (id = 'sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz') =>
  json(201, {
    session_id: id,
    file_id: 'file-1',
    chunk_size: CHUNK_PLAINTEXT_BYTES,
    expires_at: '',
  });
const commitOK = () =>
  json(201, { file_id: 'file-1', storage_key: 'attachments/x', file_type: 'file', file_size: 1 });

beforeEach(() => apiFetch.mockReset());

/** Every apiFetch call must carry a path. A call with none silently becomes a
 *  request to the API root, and a mock that tolerates it hides the bug. */
const assertAllCallsHavePaths = () => {
  const bad = apiFetch.mock.calls
    .map((c, i) => ({ i, path: c[0] as unknown }))
    .filter((r) => typeof r.path !== 'string' || r.path === '');
  expect(bad, `apiFetch called without a path: ${JSON.stringify(bad)}`).toEqual([]);
};

describe('uploadAttachmentChunked', () => {
  it('runs init -> PUT per chunk -> commit, in that order', async () => {
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(commitOK());

    const committed: Array<[number, number]> = [];
    const out = await uploadAttachmentChunked(
      fileOf(CHUNK_PLAINTEXT_BYTES + 10),
      key,
      ctx,
      new AbortController().signal,
      { onChunkCommitted: (i, t) => committed.push([i, t]) }
    );

    expect(calls()).toEqual([
      'POST /api/v1/media/upload/attachment/session',
      'PUT /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/chunk/0',
      'PUT /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/chunk/1',
      'POST /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/commit',
    ]);
    expect(committed).toEqual([
      [0, 2],
      [1, 2],
    ]);
    expect(out.file_id).toBe('file-1');
  }, 20000);

  it('declares the ciphertext length the envelope will actually be', async () => {
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(commitOK());

    const plaintext = 4096;
    await uploadAttachmentChunked(fileOf(plaintext), key, ctx, new AbortController().signal, {
      onChunkCommitted: () => {},
    });

    const sent = JSON.parse(apiFetch.mock.calls[0][1].body as string);
    // The server recomputes this and 400s on disagreement, so a client that
    // guesses here fails closed rather than uploading garbage.
    expect(sent.declared_ciphertext_bytes).toBe(expectedBlobLength(plaintext));
    expect(sent.total_chunks).toBe(1);
    expect(sent.chunk_size).toBe(CHUNK_PLAINTEXT_BYTES);
    expect(sent.key_version).toBe(3);
    expect(sent.channel_id).toBe('chan-1');
  });

  it('re-PUTs only the indices the 409 names, holding fileNonce stable', async () => {
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 204 })) // chunk 0
      .mockResolvedValueOnce(new Response(null, { status: 204 })) // chunk 1
      .mockResolvedValueOnce(json(409, { error: 'Upload is incomplete', missing: [0] }))
      .mockResolvedValueOnce(new Response(null, { status: 204 })) // re-PUT 0 only
      .mockResolvedValueOnce(commitOK());

    await uploadAttachmentChunked(
      fileOf(CHUNK_PLAINTEXT_BYTES + 10),
      key,
      ctx,
      new AbortController().signal,
      { onChunkCommitted: () => {} }
    );

    expect(calls()).toEqual([
      'POST /api/v1/media/upload/attachment/session',
      'PUT /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/chunk/0',
      'PUT /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/chunk/1',
      'POST /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/commit',
      'PUT /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/chunk/0',
      'POST /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/commit',
    ]);

    // fileNonce is bound into EVERY chunk's AAD. Changing it mid-session would
    // invalidate the parts already stored, so the retry must reuse the header.
    const firstHeader = bodyOf(1).slice(0, ENVELOPE_HEADER_BYTES);
    const retryHeader = bodyOf(4).slice(0, ENVELOPE_HEADER_BYTES);
    expect(Array.from(retryHeader)).toEqual(Array.from(firstHeader));
  }, 20000);

  it('draws a NEW fileNonce when a 410 forces a full restart', async () => {
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK('sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(json(410, { error: 'Upload session has expired' }))
      .mockResolvedValueOnce(initOK('sess2zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(commitOK());

    await uploadAttachmentChunked(fileOf(4096), key, ctx, new AbortController().signal, {
      onChunkCommitted: () => {},
    });

    // The old session's parts are gone, so reusing the nonce buys nothing and
    // muddies the audit story.
    const before = bodyOf(1).slice(0, ENVELOPE_HEADER_BYTES);
    const after = bodyOf(4).slice(0, ENVELOPE_HEADER_BYTES);
    expect(Array.from(after)).not.toEqual(Array.from(before));
    expect(calls()[3]).toBe('POST /api/v1/media/upload/attachment/session');
  });

  it('cancels the session on abort and surfaces the abort', async () => {
    const key = await aesKey();
    const ac = new AbortController();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockImplementationOnce(async () => {
        ac.abort();
        return new Response(null, { status: 204 });
      })
      .mockResolvedValueOnce(new Response(null, { status: 204 })); // the DELETE

    await expect(
      uploadAttachmentChunked(fileOf(CHUNK_PLAINTEXT_BYTES + 10), key, ctx, ac.signal, {
        onChunkCommitted: () => {},
      })
    ).rejects.toThrow(/abort/i);

    expect(calls()).toContain(
      'DELETE /api/v1/media/upload/attachment/session/sess1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'
    );
    // It must not keep uploading after the abort.
    expect(calls().filter((c) => c.includes('/chunk/'))).toHaveLength(1);
  }, 20000);

  it('does not retry forever on a repeated 409', async () => {
    const key = await aesKey();
    apiFetch.mockImplementation(async (path?: string, init?: RequestInit) => {
      const p = path ?? '';
      if ((init?.method ?? 'GET') === 'POST' && p.endsWith('/commit')) {
        return json(409, { error: 'Upload is incomplete', missing: [0] });
      }
      if (p.endsWith('/session')) return initOK();
      return new Response(null, { status: 204 });
    });

    await expect(
      uploadAttachmentChunked(fileOf(4096), key, ctx, new AbortController().signal, {
        onChunkCommitted: () => {},
      })
    ).rejects.toThrow();
    // A server that always reports the same part missing must not spin.
    // EXACTLY 3, not "at most 4". The loose bound could not distinguish 1, 2, 3
    // or 4 -- setting MAX_COMMIT_ATTEMPTS to 2 passed every test. Same ceiling+1
    // shape as the chunk-index bound caught earlier in this PR.
    expect(calls().filter((c) => c.endsWith('/commit'))).toHaveLength(3);
    assertAllCallsHavePaths();
  });

  it('DOES allow the full three attempts — two repairs then success', async () => {
    // The other half of the bound: `rejects.toThrow()` with no message would
    // also be satisfied by an abort or a network error, and nothing asserted
    // that three attempts are actually permitted rather than merely capped.
    const key = await aesKey();
    let commits = 0;
    apiFetch.mockImplementation(async (path?: string, init?: RequestInit) => {
      const p = path ?? '';
      if ((init?.method ?? 'GET') === 'POST' && p.endsWith('/commit')) {
        commits += 1;
        return commits < 3 ? json(409, { missing: [0] }) : commitOK();
      }
      if ((init?.method ?? 'GET') === 'POST') return initOK();
      return new Response(null, { status: 200 });
    });

    const out = await uploadAttachmentChunked(
      fileOf(1024),
      key,
      {
        channelId: 'chan-1',
        keyVersion: 3,
        fileType: 'file',
        mimeType: 'application/octet-stream',
      },
      new AbortController().signal,
      { onChunkCommitted: () => {} }
    );

    expect(out.file_id).toBe('file-1');
    expect(commits).toBe(3);
  });
});

// --- paths a refactor moved but nothing watched ---------------------------
//
// Each of these was found by MUTATING the production code after the phase
// extraction and observing the suite stay green. Three behaviours had no test
// at all, which is exactly the class a refactor can break in silence.
describe('uploadAttachmentChunked — previously uncovered expiry and repair paths', () => {
  const noopCb = { onChunkCommitted: () => {} };

  it('restarts with a NEW fileNonce when a chunk PUT returns 410', () => {
    // Expiry mid-upload is recoverable: the session is gone but the file is not.
    // The restart must draw a fresh nonce, because the old session's parts are
    // unreachable and the nonce is bound into every chunk's AAD.
    return (async () => {
      const key = await aesKey();
      apiFetch
        .mockResolvedValueOnce(initOK('sessAzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'))
        .mockResolvedValueOnce(new Response(null, { status: 410 }))
        .mockResolvedValueOnce(initOK('sessBzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'))
        .mockResolvedValueOnce(new Response(null, { status: 200 }))
        .mockResolvedValueOnce(commitOK());

      const out = await uploadAttachmentChunked(
        fileOf(1024),
        key,
        ctx,
        new AbortController().signal,
        noopCb
      );

      expect(out.file_id).toBe('file-1');
      const paths = calls();
      // Two inits: the 410 forced a second session rather than failing.
      expect(
        paths.filter((p) => p === 'POST /api/v1/media/upload/attachment/session')
      ).toHaveLength(2);
      expect(paths).toContain(
        'POST /api/v1/media/upload/attachment/session/sessBzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz/commit'
      );
    })();
  });

  it('gives up after the restart cap rather than looping forever', async () => {
    const key = await aesKey();
    // Every session expires immediately. Without the cap this never terminates.
    apiFetch.mockImplementation((path: string, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'POST' && path.endsWith('/session')) {
        return Promise.resolve(initOK(`sess${apiFetch.mock.calls.length}`.padEnd(43, 'z')));
      }
      return Promise.resolve(new Response(null, { status: 410 }));
    });

    await expect(
      uploadAttachmentChunked(fileOf(1024), key, ctx, new AbortController().signal, noopCb)
    ).rejects.toThrow(/kept expiring/i);

    // MAX_SESSION_RESTARTS = 1, so exactly two sessions are opened, never more.
    expect(
      calls().filter((p) => p === 'POST /api/v1/media/upload/attachment/session')
    ).toHaveLength(2);
  });

  it('refuses a 409 that names no missing parts', async () => {
    // A 409 means "incomplete", so an empty list is the server contradicting
    // itself. Treating it as "nothing to resend" would spin the commit loop to
    // its cap and then report a generic failure, hiding the protocol violation.
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 200 }))
      .mockResolvedValueOnce(json(409, { missing: [] }));

    await expect(
      uploadAttachmentChunked(fileOf(1024), key, ctx, new AbortController().signal, noopCb)
    ).rejects.toThrow(/named no missing parts/i);
  });

  it('cancels the session when a chunk fails for a non-410 reason', async () => {
    // The DELETE is what stops the bytes being orphaned until the sweeper runs.
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK('sessXzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'))
      .mockResolvedValueOnce(new Response('boom', { status: 500 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));

    await expect(
      uploadAttachmentChunked(fileOf(1024), key, ctx, new AbortController().signal, noopCb)
    ).rejects.toThrow(/Chunk 0 failed \(500\)/);

    expect(calls()).toContain(
      'DELETE /api/v1/media/upload/attachment/session/sessXzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'
    );
  });
});

describe('uploadAttachmentChunked — the abort signal must REACH the transport', () => {
  // The bug this locks: every phase received `signal` and called throwIfAborted
  // between requests, but no apiFetch call ever got it. Cancelling mid-chunk
  // therefore waited for an in-flight 8 MiB PUT to finish -- a cancel that does
  // not cancel, for the third time in this PR, one layer deeper each time.
  //
  // The earlier tests could not see it because the MOCK inspected signal.aborted
  // itself, making it a more cooperative citizen than the real transport. They
  // proved the signal was created and aborted; never that anything downstream
  // LISTENED. This asserts the signal arrives at apiFetch, which is the only
  // property that makes cancellation real.
  it('passes the signal to open, chunk, and commit requests', async () => {
    const key = await aesKey();
    const controller = new AbortController();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 200 }))
      .mockResolvedValueOnce(commitOK());

    await uploadAttachmentChunked(fileOf(1024), key, ctx, controller.signal, {
      onChunkCommitted: () => {},
    });

    const paths = calls();
    expect(paths).toHaveLength(3); // open, chunk, commit
    apiFetch.mock.calls.forEach((c, i) => {
      expect(c[1]?.signal, `request ${i} (${paths[i]}) carried no signal`).toBe(controller.signal);
    });
  });

  it('does NOT pass the signal to the cancel DELETE', async () => {
    // The cancel runs BECAUSE the upload aborted. Forwarding an already-aborted
    // signal would cancel the cleanup itself and strand the bytes it exists to
    // release -- the sweeper would then have to do work the client already asked
    // the server to do.
    const key = await aesKey();
    const controller = new AbortController();
    apiFetch
      .mockResolvedValueOnce(initOK('sessCzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'))
      .mockResolvedValueOnce(new Response('boom', { status: 500 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));

    await expect(
      uploadAttachmentChunked(fileOf(1024), key, ctx, controller.signal, {
        onChunkCommitted: () => {},
      })
    ).rejects.toThrow();

    const deleteCall = apiFetch.mock.calls.find((c) => (c[1]?.method as string) === 'DELETE');
    expect(deleteCall, 'the failed upload must still fire a cancel').toBeTruthy();
    expect(deleteCall?.[1]?.signal).toBeUndefined();
  });
});

describe('uploadAttachmentChunked — exactly one upload target', () => {
  // THE BLOCK THIS PR SHIPPED: MessageInput computes
  // `targetId = conversationId || channelId`, so in a DM both parameters hold
  // the same UUID. Forwarding both made the init body carry channel_id AND
  // conversation_id, which the server refuses -- every DM attachment upload
  // 400'd. Nothing caught it because server-channel uploads leave
  // conversationId undefined and the only DM test asserts on FormData, i.e.
  // the legacy path.
  const initBodyOf = (i: number) => JSON.parse(apiFetch.mock.calls[i][1].body as string);

  it('sends conversation_id ALONE for a DM', async () => {
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 200 }))
      .mockResolvedValueOnce(commitOK());

    await uploadAttachmentChunked(
      fileOf(1024),
      key,
      {
        conversationId: 'conv-123',
        keyVersion: 3,
        fileType: 'file',
        mimeType: 'application/octet-stream',
      },
      new AbortController().signal,
      { onChunkCommitted: () => {} }
    );

    const body = initBodyOf(0);
    expect(body.conversation_id).toBe('conv-123');
    expect(body).not.toHaveProperty('channel_id');
  });

  it('sends channel_id ALONE for a server channel', async () => {
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 200 }))
      .mockResolvedValueOnce(commitOK());

    await uploadAttachmentChunked(
      fileOf(1024),
      key,
      {
        channelId: 'chan-1',
        keyVersion: 3,
        fileType: 'file',
        mimeType: 'application/octet-stream',
      },
      new AbortController().signal,
      { onChunkCommitted: () => {} }
    );

    const body = initBodyOf(0);
    expect(body.channel_id).toBe('chan-1');
    expect(body).not.toHaveProperty('conversation_id');
  });

  it('NEVER sends both, whichever way the caller is shaped', async () => {
    // The assertion the shipped bug needed: the server rejects both-present, so
    // "exactly one key" is the property, not "the right one is present".
    const key = await aesKey();
    for (const target of [{ channelId: 'chan-1' }, { conversationId: 'conv-1' }] as const) {
      apiFetch.mockReset();
      apiFetch
        .mockResolvedValueOnce(initOK())
        .mockResolvedValueOnce(new Response(null, { status: 200 }))
        .mockResolvedValueOnce(commitOK());

      await uploadAttachmentChunked(
        fileOf(1024),
        key,
        { ...target, keyVersion: 3, fileType: 'file', mimeType: 'application/octet-stream' },
        new AbortController().signal,
        { onChunkCommitted: () => {} }
      );

      const body = initBodyOf(0);
      const ids = ['channel_id', 'conversation_id'].filter((k) => k in body);
      expect(ids).toHaveLength(1);
    }
  });
});

describe('uploadAttachmentChunked — server responses are validated, not trusted', () => {
  it('refuses a 201 whose body carries no usable session id', async () => {
    // Casting `as { session_id: string }` yielded `undefined`, which then
    // interpolated into every later URL: the chunk PUT went to
    // /session/undefined/chunk/0 and the user saw "Chunk 0 failed (404): Upload
    // session not found" -- a message that sends whoever debugs it hunting an
    // expiry bug that does not exist. `undefined` also entered liveSessions, so
    // unmount fired a DELETE against /session/undefined.
    const key = await aesKey();
    for (const body of [
      {},
      { session_id: 42 },
      { session_id: '' },
      { session_id: '../../users/me' },
    ]) {
      apiFetch.mockReset();
      apiFetch.mockResolvedValueOnce(json(201, body));

      await expect(
        uploadAttachmentChunked(fileOf(1024), key, ctx, new AbortController().signal, {
          onChunkCommitted: () => {},
        })
      ).rejects.toThrow(/usable upload session id/i);

      // And nothing was ever requested against a malformed path.
      expect(calls().some((c) => c.includes('undefined') || c.includes('..'))).toBe(false);
    }
  });

  it('refuses a 409 naming indices outside the declared range', async () => {
    // Each element is interpolated into a URL and passed to buildUploadPart.
    // The traversal defence was accidental -- it held only because putChunk
    // happens to build the AAD (which rejects a bad index) BEFORE the URL.
    const key = await aesKey();
    apiFetch
      .mockResolvedValueOnce(initOK())
      .mockResolvedValueOnce(new Response(null, { status: 200 }))
      .mockResolvedValueOnce(json(409, { missing: [99, -1, '../../users/me'] }));

    await expect(
      uploadAttachmentChunked(fileOf(1024), key, ctx, new AbortController().signal, {
        onChunkCommitted: () => {},
      })
    ).rejects.toThrow(/not valid chunk indices/i);

    expect(calls().some((c) => c.includes('..'))).toBe(false);
  });
});
