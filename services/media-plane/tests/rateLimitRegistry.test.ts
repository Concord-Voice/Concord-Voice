import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import { EVENT_BUDGETS } from '../src/lib/rateLimit.js';

/**
 * Registration invariant lock (#2032).
 *
 * The exhaustive `Record<RoomEventName, RateBudget>` catches a MISSING BUDGET at
 * compile time, but cannot see a bare `socket.on('new-thing', handler)` — the
 * Socket.IO signature accepts any string. This source scan closes that hole.
 *
 * Same discipline as internal/auth/log_emissions_test.go and
 * client/desktop/tests/unit/main/eslint-no-raw-err-console.test.ts.
 *
 * NOTE: it scans the WHOLE file, not just the io.on('connection') block —
 * join-room is registered in registerJoinRoomHandler, outside that callback. If
 * handler registration is ever split across modules, add the new paths here.
 */
const SCANNED_FILES = ['../src/index.ts'];

/**
 * The ONLY `.on()` event names allowed to be registered bare in the scanned
 * file. Everything else must go through `withRateLimit`.
 *
 * This is an allowlist, not a denylist, on purpose — see the fail-closed check
 * below. Adding an entry here is a deliberate, reviewable act; forgetting to
 * add one fails the suite rather than silently widening the surface.
 *
 *  - `connection` / `disconnect` — Socket.IO lifecycle, raised by the server.
 *  - `SIGTERM` / `SIGINT` — process signals, not socket traffic at all.
 */
const LIFECYCLE_ALLOWLIST = new Set(['connection', 'disconnect', 'SIGTERM', 'SIGINT']);

const here = dirname(fileURLToPath(import.meta.url));

function readScanned(): string {
  return SCANNED_FILES.map((p) => readFileSync(resolve(here, p), 'utf8')).join('\n');
}

/**
 * AST-based registration scan.
 *
 * This replaced a regex scan, which had four blind spots — and the dangerous
 * ones were all on the BARE `socket.on` side, i.e. exactly the drift this suite
 * exists to catch:
 *
 *  - a template-literal or double-quoted event name was invisible;
 *  - a renamed receiver (`sock.on(...)`) was invisible;
 *  - code-like text inside a comment or a string literal was counted, so a
 *    commented-out wrapper could make a live unwrapped handler look covered.
 *
 * An earlier fix stripped comments and argued the rest was unreachable because
 * `EVENT_BUDGETS` is keyed by a literal union. That argument only holds for the
 * WRAPPED call: `withRateLimit` will not compile with a non-literal name, but a
 * bare `socket.on` has no such constraint. (CodeRabbit #2793.)
 *
 * The AST walk is receiver-agnostic on purpose: it matches ANY `<expr>.on(...)`
 * and any `withRateLimit(<expr>, ...)`, so renaming the socket variable cannot
 * hide a registration. Non-literal event names are reported separately rather
 * than silently dropped — an unresolvable name is a finding, not a pass.
 */
function findRegistrations(source: string) {
  const sf = ts.createSourceFile('scan.ts', source, ts.ScriptTarget.Latest, true);
  const bare: Array<{ event: string; receiver: string }> = [];
  const wrapped: string[] = [];
  const dynamic: string[] = [];

  /** A literal event name, or null when the argument is not statically known. */
  const literal = (node: ts.Node | undefined): string | null => {
    if (!node) return null;
    if (ts.isStringLiteral(node)) return node.text;
    if (ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
    return null;
  };

  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node)) {
      const callee = node.expression;

      // ANY receiver: `socket.on(...)`, `sock.on(...)`, `this.socket.on(...)`.
      if (ts.isPropertyAccessExpression(callee) && callee.name.text === 'on') {
        const name = literal(node.arguments[0]);
        if (name !== null) bare.push({ event: name, receiver: callee.expression.getText(sf) });
        else if (node.arguments.length > 0) dynamic.push(callee.getText(sf));
      }

      if (ts.isIdentifier(callee) && callee.text === 'withRateLimit') {
        const name = literal(node.arguments[1]);
        if (name !== null) wrapped.push(name);
        else if (node.arguments.length > 1) dynamic.push(callee.getText(sf));
      }
    }
    ts.forEachChild(node, visit);
  };

  visit(sf);
  return { bare, wrapped, dynamic };
}

describe('registration scan', () => {
  // Each case below was a blind spot of the regex scan this replaced. The
  // dangerous ones are all on the BARE side: a registration the scan cannot see
  // is an unwrapped handler that passes the invariant check silently.
  it('ignores registrations inside comments and string literals', () => {
    const src = [
      "// withRateLimit(socket, 'ghost-event', handler);",
      "/* socket.on('other-ghost', handler); */",
      'const doc = "socket.on(\'string-ghost\', handler)";',
      "withRateLimit(socket, 'produce', handler);",
    ].join('\n');

    const { bare, wrapped } = findRegistrations(src);
    expect(wrapped).toEqual(['produce']);
    expect(bare).toEqual([]);
  });

  it('fails an aliased registration of an unbudgeted event (#2793)', () => {
    const src = ['const sock = socket;', "sock.on('brand-new-event', handler);"].join('\n');

    const { bare } = findRegistrations(src);
    const unwrapped = bare.map((r) => r.event).filter((e) => !LIFECYCLE_ALLOWLIST.has(e));

    // The old pair of checks passed this: not on `socket`, not yet budgeted.
    expect(unwrapped).toEqual(['brand-new-event']);
  });

  it('sees a double-quoted event name', () => {
    const { bare } = findRegistrations('socket.on("double-quoted", handler);');
    expect(bare.map((r) => r.event)).toEqual(['double-quoted']);
  });

  it('sees a template-literal event name', () => {
    const { bare } = findRegistrations('socket.on(`templated`, handler);');
    expect(bare.map((r) => r.event)).toEqual(['templated']);
  });

  it('sees a registration on a RENAMED receiver', () => {
    const { bare } = findRegistrations("sock.on('renamed-receiver', handler);");
    expect(bare).toEqual([{ event: 'renamed-receiver', receiver: 'sock' }]);
  });

  // A name the scan cannot resolve statically must be REPORTED, not dropped —
  // silently ignoring it would be the same blind spot in a new costume.
  it('reports a dynamic event name instead of silently skipping it', () => {
    const { bare, dynamic } = findRegistrations('socket.on(eventName, handler);');
    expect(bare).toEqual([]);
    expect(dynamic).toHaveLength(1);
  });
});

describe('rate-limit registration invariant', () => {
  // FAIL CLOSED, and receiver-agnostic.
  //
  // An earlier version ran two checks — bare-on-`socket`, plus any BUDGETED
  // event on any receiver — and an aliased registration slipped between them:
  // `const sock = socket; sock.on('new-event', handler)` failed the first
  // (receiver is not `socket`) and the second (the event is not budgeted yet,
  // precisely because it is new). A brand-new client event on an aliased socket
  // was therefore invisible — the exact drift this suite exists to catch.
  // (#2793 CodeRabbit.)
  //
  // Inverting it removes the gap without needing to resolve aliases at all:
  // EVERY bare registration must be explicitly allowlisted. Renaming the socket
  // no longer helps, because the check never asks what the receiver is called.
  it('leaves no registration bare unless it is explicitly allowlisted', () => {
    const { bare } = findRegistrations(readScanned());
    const unwrapped = bare.map((r) => r.event).filter((e) => !LIFECYCLE_ALLOWLIST.has(e));
    expect(unwrapped).toEqual([]);
  });

  it('resolves every event name statically, so none is silently skipped', () => {
    const { dynamic } = findRegistrations(readScanned());
    expect(dynamic).toEqual([]);
  });

  it('gives every wrapped event a budget', () => {
    const { wrapped } = findRegistrations(readScanned());
    const missing = wrapped.filter((e) => !(e in EVENT_BUDGETS));
    expect(missing).toEqual([]);
  });

  it('has no orphaned budget for a deleted handler', () => {
    const { wrapped } = findRegistrations(readScanned());
    const discovered = [...new Set(wrapped)].sort();
    const declared = Object.keys(EVENT_BUDGETS).sort();
    expect(declared).toEqual(discovered);
  });

  it('still finds join-room, which is registered outside io.on("connection")', () => {
    const { wrapped } = findRegistrations(readScanned());
    expect(wrapped).toContain('join-room');
  });
});
