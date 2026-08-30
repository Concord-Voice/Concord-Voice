/**
 * Shared harness for the purge-on-moderation suites (#1354).
 *
 * `MemberList` and `MemberListPanel` are two surfaces over ONE request path
 * (`moderateMember` in `components/Members/purgeOnModeration.tsx`), so their
 * wire fixtures, MSW stubs and context-menu double were byte-identical apart
 * from the server id. Two copies of the same wire assertions drift the moment
 * the `purge` response shape changes — which is the whole point of asserting
 * them. They live here instead.
 *
 * What deliberately does NOT live here: each suite's own render/mount setup.
 * They mount different components with different props, and folding that in
 * would buy nothing while making both suites harder to read.
 */
import { http, HttpResponse } from 'msw';
import { server as mswServer } from '../mocks/server';
import type { ServerMember } from '@/renderer/stores/chat/memberStore';

export const MODERATION_API_BASE = 'http://localhost:8080';

/** The checkbox label both surfaces render, asserted by accessible name. */
export const PURGE_CHECKBOX = 'Also purge their messages in this server';

/** The one member both suites moderate. */
export const moderationMember: ServerMember = {
  user_id: 'u1',
  username: 'alice',
  display_name: 'Alice',
  role: 'member',
  joined_at: '2025-01-01T00:00:00Z',
  roles: [],
};

export interface ModerationPurgeOutcome {
  requested: boolean;
  status: string;
  purged_count: number;
}

const banUrl = (serverId: string) => `${MODERATION_API_BASE}/api/v1/servers/${serverId}/bans/u1`;
const kickUrl = (serverId: string) =>
  `${MODERATION_API_BASE}/api/v1/servers/${serverId}/members/u1`;

/**
 * Captures the request bodies the ban endpoint receives, optionally returning a
 * purge outcome fragment. The captured array is the assertion target — the wire
 * body is the contract these suites exist to pin.
 */
export function stubBan(serverId: string, purge?: ModerationPurgeOutcome): unknown[] {
  const bodies: unknown[] = [];
  mswServer.use(
    http.post(banUrl(serverId), async ({ request }) => {
      bodies.push(await request.json());
      return HttpResponse.json(purge ? { message: 'banned', purge } : { message: 'banned' });
    })
  );
  return bodies;
}

export function stubKick(serverId: string, purge?: ModerationPurgeOutcome): unknown[] {
  const bodies: unknown[] = [];
  mswServer.use(
    http.delete(kickUrl(serverId), async ({ request }) => {
      bodies.push(await request.json());
      return HttpResponse.json(purge ? { message: 'kicked', purge } : { message: 'kicked' });
    })
  );
  return bodies;
}

/**
 * A ban endpoint that fails with a non-JSON body — the proxy-502 shape.
 * `moderateMember` must surface its OWN message rather than letting a JSON
 * parse error ("Expected JSON but got text/html…") reach the user.
 */
export function stubBanNonJsonFailure(serverId: string): void {
  mswServer.use(
    http.post(
      banUrl(serverId),
      () =>
        new HttpResponse('<html>502 Bad Gateway</html>', {
          status: 502,
          headers: { 'Content-Type': 'text/html' },
        })
    )
  );
}

/**
 * The real `MemberContextMenu` pulls in the router, permission store and voice
 * store. This double only fires the two callbacks both surfaces wire to it.
 * Call from a module-scope `vi.mock` factory.
 */
export function memberContextMenuDouble() {
  return {
    default: ({
      member,
      onBan,
      onKick,
    }: {
      member: ServerMember;
      onBan: (m: ServerMember) => void;
      onKick: (m: ServerMember) => void;
    }) => (
      <div>
        <button type="button" onClick={() => onBan(member)}>
          Open ban dialog
        </button>
        <button type="button" onClick={() => onKick(member)}>
          Open kick dialog
        </button>
      </div>
    ),
  };
}
