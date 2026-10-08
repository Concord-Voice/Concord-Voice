/**
 * MSW harness for gated writes that are not a JSON POST or DELETE (#3456 §3.4): Server Settings'
 * `PATCH`, the expiration policy `PATCH`, and the icon and banner uploads, whose `mfa_code`
 * travels as a multipart field. `gatedRoute.ts` covers the other two verbs; this file reuses its
 * reply type so a fixture reads the same in every suite.
 *
 * A suite that records a multipart upload must install Node's native `FormData`, `Blob` and `File`
 * first (see ImageCropEditor.stepUp.test.tsx): jsdom's cannot carry a file part, and undici's
 * `request.formData()` asserts on it.
 *
 * The recorded writes are the assertion target: what was sent, how many times, and with which
 * body or fields, is the contract these suites pin.
 */
import { http, HttpResponse } from 'msw';
import { server as mswServer } from '../mocks/server';
import { MFA_REQUIRED, type GatedReply } from './gatedRoute';

export interface RecordedWrite {
  /** The parsed JSON body; null for a multipart request or one with no body. */
  json: Record<string, unknown> | null;
  /** The multipart text fields; empty for a JSON request. */
  fields: Record<string, string>;
  /** The multipart `file` part, read back; null when there is none. */
  file: { name: string; type: string; text: string } | null;
  contentType: string | null;
}

export interface GatedWriteOptions {
  method: 'patch' | 'post';
  url: string;
  /** The answer to a write with no `mfa_code`. Omitted: a refusal asking for a code. */
  first?: GatedReply;
  /** The answers to writes that carry `mfa_code`, in order. The last one repeats. Omitted: 200. */
  retries?: readonly GatedReply[];
}

async function readWrite(request: Request): Promise<RecordedWrite> {
  const contentType = request.headers.get('Content-Type');
  if (contentType?.startsWith('multipart/form-data')) {
    const form = await request.formData();
    const fields: Record<string, string> = {};
    let file: RecordedWrite['file'] = null;
    for (const [key, value] of form.entries()) {
      if (typeof value === 'string') fields[key] = value;
      else file = { name: value.name, type: value.type, text: await value.text() };
    }
    return { json: null, fields, file, contentType };
  }
  const text = await request.text();
  const json = text === '' ? null : (JSON.parse(text) as Record<string, unknown>);
  return { json, fields: {}, file: null, contentType };
}

function carriesCode(write: RecordedWrite): boolean {
  return (write.json !== null && 'mfa_code' in write.json) || 'mfa_code' in write.fields;
}

/** Installs the gated route and returns the list its writes are recorded in. */
export function stubGatedWrite(options: GatedWriteOptions): RecordedWrite[] {
  const writes: RecordedWrite[] = [];
  const retries = options.retries ?? [{ status: 200, body: { message: 'ok' } }];
  let retryIndex = 0;
  const handler = async ({ request }: { request: Request }) => {
    const write = await readWrite(request);
    writes.push(write);
    const answer = carriesCode(write)
      ? retries[Math.min(retryIndex++, retries.length - 1)]
      : (options.first ?? MFA_REQUIRED);
    return HttpResponse.json(answer.body ?? {}, { status: answer.status, ...answer.init });
  };
  mswServer.use(
    options.method === 'patch' ? http.patch(options.url, handler) : http.post(options.url, handler)
  );
  return writes;
}

/** The `mfa_code`s sent, in order, from JSON bodies and multipart fields alike. */
export function writeCodes(writes: readonly RecordedWrite[]): string[] {
  return writes.filter(carriesCode).map((w) => String(w.json?.mfa_code ?? w.fields.mfa_code));
}
