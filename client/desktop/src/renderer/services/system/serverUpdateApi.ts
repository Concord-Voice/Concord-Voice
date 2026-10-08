/**
 * Server Settings' save, `PATCH /api/v1/servers/:id` (#3456 §3.4).
 *
 * On a server that enforces MFA on dangerous actions the save is gated
 * (`servers.update`). The first send carries no code and is exactly what the
 * page sent before #3456; a refusal that asks for verification freezes that
 * body, and `resendServerUpdate` sends the same body again with `mfa_code`. The
 * page never re-reads its form for the second send.
 */
import { z } from 'zod';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import type { ServerWithRole } from '../../types/server';
import { FIRST_SEND_SESSION_CHANGED } from './dangerousActionRequest';
import {
  apiFetchInContext,
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from './requestContext';
import { serverErrorText } from './stepUpRouteAdapters';
import { stepUpSeed } from './stepUpSeed';

/** The save body. An image key is present only when that image changed; `null` removes it. */
export interface ServerUpdateBody {
  name: string;
  icon_url?: string | null;
  banner_url?: string | null;
  allow_embedded_content?: boolean;
}

/** An image as `useImageUpload` holds it. */
interface ImageField {
  removed: boolean;
  imageUrl: string | null;
}

/** What the General form currently says. */
export interface ServerUpdateForm {
  name: string;
  icon: ImageField;
  banner: ImageField;
  allowEmbeddedContent: boolean;
}

type ServerBefore = Pick<
  ServerWithRole,
  'name' | 'icon_url' | 'banner_url' | 'allow_embedded_content'
>;

function changedImage(field: ImageField, current: string | undefined): string | null | undefined {
  if (field.removed) return null;
  return field.imageUrl && field.imageUrl !== current ? field.imageUrl : undefined;
}

/** The body that would save `form` over `server`: the name, and only what differs. */
export function buildServerUpdateBody(
  server: ServerBefore,
  form: ServerUpdateForm
): ServerUpdateBody {
  const body: ServerUpdateBody = { name: form.name.trim() };
  const icon = changedImage(form.icon, server.icon_url);
  if (icon !== undefined) body.icon_url = icon;
  const banner = changedImage(form.banner, server.banner_url);
  if (banner !== undefined) body.banner_url = banner;
  if (form.allowEmbeddedContent !== server.allow_embedded_content) {
    body.allow_embedded_content = form.allowEmbeddedContent;
  }
  return body;
}

/** True when leaving the page would lose edits: saving them would change something. */
export function hasUnsavedChanges(server: ServerBefore, form: ServerUpdateForm): boolean {
  const body = buildServerUpdateBody(server, form);
  return (
    body.name !== server.name ||
    body.icon_url !== undefined ||
    body.banner_url !== undefined ||
    body.allow_embedded_content !== undefined
  );
}

/** What the page keeps of the saved server. A removed image is absent, as the route omits it. */
export type UpdatedServer = Pick<
  ServerWithRole,
  'name' | 'icon_url' | 'banner_url' | 'allow_embedded_content' | 'updated_at'
>;

const savedServerSchema = z.object({
  server: z.object({
    name: z.string(),
    icon_url: z.string().nullish(),
    banner_url: z.string().nullish(),
    allow_embedded_content: z.boolean(),
    updated_at: z.string(),
  }),
});

function savedServerOf(body: unknown): UpdatedServer | null {
  const parsed = savedServerSchema.safeParse(body);
  if (!parsed.success) return null;
  const { server } = parsed.data;
  return {
    name: server.name,
    icon_url: server.icon_url ?? undefined,
    banner_url: server.banner_url ?? undefined,
    allow_embedded_content: server.allow_embedded_content,
    updated_at: server.updated_at,
  };
}

/** The page's sentence for a refusal that carries none of its own. */
export const UPDATE_FAILED = 'Failed to update server';

function serverPath(serverId: string): string {
  return `/api/v1/servers/${serverId}`;
}

function patchInit(body: ServerUpdateBody, mfaCode?: string): RequestInit {
  return {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(mfaCode ? { ...body, mfa_code: mfaCode } : body),
  };
}

/** A response's JSON body, or null when it has none (a proxy's HTML 502, an empty body). */
async function readBody(response: Response): Promise<unknown> {
  return response.json().catch(() => null);
}

export type ServerUpdateResult =
  | { kind: 'ok'; server: UpdatedServer }
  /**
   * The server wants a verified code first: the refusal that seeds the dialog,
   * and the account and server the save went out as, which the re-send keeps.
   */
  | { kind: 'stepUp'; refusal: StepUpFactorRefusal; context: ApiRequestContext }
  | { kind: 'failed'; message: string };

/**
 * Sends the save with no code, as the account and server current now. A transport failure
 * throws, as `apiFetch` does; the page words it. A refusal that asks for verification once that
 * account or server is no longer current opens nothing: it belongs to the old one.
 */
export async function updateServer(
  serverId: string,
  body: ServerUpdateBody
): Promise<ServerUpdateResult> {
  const context = captureApiRequestContext();
  const response = await apiFetchInContext(serverPath(serverId), patchInit(body), context);
  const data = await readBody(response);
  if (response.ok) {
    const server = savedServerOf(data);
    return server === null ? { kind: 'failed', message: UPDATE_FAILED } : { kind: 'ok', server };
  }
  const refusal = stepUpSeed(response.status, data);
  if (refusal === null) return { kind: 'failed', message: serverErrorText(data) ?? UPDATE_FAILED };
  if (!apiRequestContextIsCurrent(context)) {
    return { kind: 'failed', message: FIRST_SEND_SESSION_CHANGED };
  }
  return { kind: 'stepUp', refusal, context };
}

/**
 * What the re-send came to. Assignable to the dialog's `DangerousActionSendResult`, with the
 * saved server kept on success for the page.
 */
export type ServerUpdateResend =
  { kind: 'ok'; server: UpdatedServer } | { kind: 'refused'; status: number; body: unknown };

/** Sends the frozen save again with the proven code, admitted against `context`. */
export async function resendServerUpdate(
  serverId: string,
  body: ServerUpdateBody,
  mfaCode: string | undefined,
  context: ApiRequestContext
): Promise<ServerUpdateResend> {
  const response = await apiFetchInContext(serverPath(serverId), patchInit(body, mfaCode), context);
  const data = await readBody(response);
  if (!response.ok) return { kind: 'refused', status: response.status, body: data };
  const server = savedServerOf(data);
  return server === null
    ? { kind: 'refused', status: response.status, body: { error: UPDATE_FAILED } }
    : { kind: 'ok', server };
}
