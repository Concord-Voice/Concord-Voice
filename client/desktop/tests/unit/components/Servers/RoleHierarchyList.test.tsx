import React from 'react';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '../../../test-utils';
import userEvent from '@testing-library/user-event';
import { resetAllStores } from '../../../helpers/store-helpers';
import RoleHierarchyList, {
  insertionToTargetIndex,
} from '@/renderer/components/Servers/RoleHierarchyList';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import type { ReorderOutcome, Role, RoleReorderPayload, RoleViewer } from '@/renderer/types/server';

/**
 * Tests for the role hierarchy rail (#2359 / #2859).
 *
 * Two things this suite is deliberately built around:
 *
 *  1. `insertionToTargetIndex` is tested DIRECTLY with cases that discriminate
 *     against the naive identity mutant `(from, i) => i`. A drag case that only
 *     moves a row upwards proves nothing about the conversion, because both
 *     implementations agree there.
 *  2. Drag geometry is INJECTED, never read from jsdom. jsdom returns
 *     zero-height rects for everything, so `decideDropIndex` fed real jsdom
 *     rects would resolve every pointer position to the same slot — vacuous at
 *     best and wrong at worst. Every drag test stubs `getBoundingClientRect` on
 *     the band rows with explicit, ordered rects and drives the drag with an
 *     explicit `clientY`.
 */

// ── Fixtures ─────────────────────────────────────────────────────────────────

function makeRole(overrides: Partial<Role> & Pick<Role, 'id' | 'name' | 'position'>): Role {
  return {
    server_id: 'server-1',
    color: '#ff0000',
    permissions: '0',
    is_default: false,
    is_managed: false,
    display_separately: false,
    mentionable: false,
    emoji: '',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

/** Highest authority first: Alpha, Bravo, Charlie, Delta. */
const roleA = makeRole({ id: 'role-a', name: 'Alpha', position: 5 });
const roleB = makeRole({ id: 'role-b', name: 'Bravo', position: 4 });
const roleC = makeRole({ id: 'role-c', name: 'Charlie', position: 3 });
const roleD = makeRole({ id: 'role-d', name: 'Delta', position: 2 });
const roleOwner = makeRole({ id: 'role-owner', name: 'Owner', position: 7 });
const roleEveryone = makeRole({
  id: 'role-everyone',
  name: '@all',
  position: 0,
  is_default: true,
  is_managed: true,
});
const roleBot = makeRole({ id: 'role-bot', name: 'BotIntegration', position: 4, is_managed: true });

const BAND_ROLES = [roleA, roleB, roleC, roleD];
const INTERACTIVE_ROLES = [roleOwner, ...BAND_ROLES, roleEveryone];

const BOUNDED_VIEWER: RoleViewer = { kind: 'bounded', maxRolePosition: 6 };

const reorderRoles =
  vi.fn<(serverId: string, payload: RoleReorderPayload) => Promise<ReorderOutcome>>();
// `fetchRoles` reports whether THIS call refreshed the view (#2859). Apply
// reconciles before it builds a payload, so a mock that resolves anything falsy
// makes the rail bail before it sends — the default here must be `true`.
const fetchRoles = vi.fn<(serverId: string) => Promise<boolean>>();

/** Seed the store the rail reads: the viewer, both actions, and the roles the
 *  Apply reconcile re-reads. The store copy matches the rendered prop, so the
 *  reconcile is a no-op unless a test deliberately diverges them. */
function seedStore(viewer: RoleViewer, roles: Role[]): void {
  usePermissionStore.setState({
    roleViewer: { 'server-1': viewer },
    serverRoles: { 'server-1': roles },
    reorderRoles,
    fetchRoles,
  });
}

interface RenderOpts {
  roles?: Role[];
  viewer?: RoleViewer;
  selectedRoleId?: string | null;
}

function renderRail(opts: RenderOpts = {}) {
  const roles = opts.roles ?? INTERACTIVE_ROLES;
  const viewer = opts.viewer ?? BOUNDED_VIEWER;
  seedStore(viewer, roles);
  const onSelectRole = vi.fn();
  const onCreateRole = vi.fn();
  const utils = render(
    <RoleHierarchyList
      serverId="server-1"
      roles={roles}
      selectedRoleId={opts.selectedRoleId ?? null}
      onSelectRole={onSelectRole}
      onCreateRole={onCreateRole}
    />
  );
  return { ...utils, onSelectRole, onCreateRole };
}

// ── DOM helpers ──────────────────────────────────────────────────────────────

function bandRows(container: HTMLElement): HTMLLIElement[] {
  return Array.from(container.querySelectorAll<HTMLLIElement>('li.role-item')).filter(
    (li) => li.querySelector('.role-drag-handle') !== null
  );
}

function allRows(container: HTMLElement): HTMLLIElement[] {
  return Array.from(container.querySelectorAll<HTMLLIElement>('li.role-item'));
}

function rowNames(container: HTMLElement): string[] {
  return allRows(container).map((li) => li.querySelector('.role-item-name')?.textContent ?? '');
}

function bandOrder(container: HTMLElement): string[] {
  return bandRows(container).map((li) => li.querySelector('.role-item-name')?.textContent ?? '');
}

function handleFor(container: HTMLElement, name: string): HTMLButtonElement {
  const row = bandRows(container).find(
    (li) => li.querySelector('.role-item-name')?.textContent === name
  );
  if (!row) throw new Error(`no movable row named ${name}`);
  const handle = row.querySelector<HTMLButtonElement>('.role-drag-handle');
  if (!handle) throw new Error(`row ${name} has no drag handle`);
  return handle;
}

const ROW_HEIGHT = 36;
const ROW_PITCH = 40;

function rectForIndex(index: number): DOMRect {
  const top = index * ROW_PITCH;
  return {
    top,
    bottom: top + ROW_HEIGHT,
    height: ROW_HEIGHT,
    left: 0,
    right: 200,
    width: 200,
    x: 0,
    y: top,
    toJSON: () => ({}),
  } as DOMRect;
}

/**
 * Give every band row a rect derived from its CURRENT position in the list, so
 * the injected geometry survives a re-order (React moves the same nodes).
 */
function injectBandGeometry(container: HTMLElement): void {
  for (const row of bandRows(container)) {
    Object.defineProperty(row, 'getBoundingClientRect', {
      configurable: true,
      value: () => rectForIndex(bandRows(container).indexOf(row)),
    });
  }
}

/** Midpoint-relative pointer Y that resolves to a given INSERTION index. */
function clientYForInsertion(insertionIndex: number): number {
  // `decideDropIndex` returns the first index whose vertical midpoint is below
  // the pointer, so land just above the midpoint of that row.
  return insertionIndex * ROW_PITCH + ROW_HEIGHT / 2 - 1;
}

interface FakeDataTransfer {
  setData: (format: string, data: string) => void;
  getData: (format: string) => string;
  effectAllowed: string;
  dropEffect: string;
}

function makeDataTransfer(): FakeDataTransfer {
  return { setData: vi.fn(), getData: vi.fn(() => ''), effectAllowed: '', dropEffect: '' };
}

/**
 * jsdom implements neither `DragEvent` nor `DataTransfer`, and Testing Library's
 * `fireEvent.dragOver` therefore falls back to a bare `Event` that silently
 * DROPS `clientY` — every drop then resolves to the end of the list, which is
 * also what the naive off-by-one mutant produces. Constructing a `MouseEvent`
 * with the native drag type keeps the pointer coordinate real (React 19
 * dispatches on the native type name, so `onDragOver` still fires).
 */
function fireDragEvent(
  el: Element,
  type: 'dragstart' | 'dragover' | 'drop',
  dataTransfer: FakeDataTransfer,
  clientY = 0
): void {
  const event = new MouseEvent(type, { bubbles: true, cancelable: true, clientY });
  Object.defineProperty(event, 'dataTransfer', { value: dataTransfer });
  fireEvent(el, event);
}

/** Drag `name` and drop it at `insertionIndex` (0..bandLength). */
function dragTo(container: HTMLElement, name: string, insertionIndex: number): void {
  injectBandGeometry(container);
  const handle = handleFor(container, name);
  const dataTransfer = makeDataTransfer();
  fireDragEvent(handle, 'dragstart', dataTransfer);
  const rows = bandRows(container);
  const overRow = rows[Math.min(insertionIndex, rows.length - 1)];
  fireDragEvent(overRow, 'dragover', dataTransfer, clientYForInsertion(insertionIndex));
  fireDragEvent(overRow, 'drop', dataTransfer);
}

async function makeDirtyViaKeyboard(container: HTMLElement, name: string): Promise<void> {
  const user = userEvent.setup();
  const handle = handleFor(container, name);
  handle.focus();
  await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
}

beforeEach(() => {
  resetAllStores();
  reorderRoles.mockReset();
  fetchRoles.mockReset();
  reorderRoles.mockResolvedValue({ ok: true, reconciled: true });
  fetchRoles.mockResolvedValue(true);
});

// ── PRIORITY 1 — the insertion → target off-by-one ───────────────────────────

describe('insertionToTargetIndex (#2859 off-by-one)', () => {
  /** The mutant this helper exists to be different from. */
  const naive = (_from: number, insertionIndex: number): number => insertionIndex;

  const cases: Array<{
    label: string;
    from: number;
    insertion: number;
    expected: number;
    discriminates: boolean;
  }> = [
    // [a,b,c,d]: a dropped before d -> [b,c,a,d], a occupies index 2.
    {
      label: 'downward past two neighbours',
      from: 0,
      insertion: 3,
      expected: 2,
      discriminates: true,
    },
    // a dropped at the very end -> [b,c,d,a], a occupies index 3.
    { label: 'downward to end-of-list', from: 0, insertion: 4, expected: 3, discriminates: true },
    // d dropped before a -> [d,a,b,c]; nothing shifts, conversion must not fire.
    {
      label: 'upward drag does not convert',
      from: 3,
      insertion: 0,
      expected: 0,
      discriminates: false,
    },
    // dropped on itself.
    { label: 'drop on self is a no-op', from: 2, insertion: 2, expected: 2, discriminates: false },
    // dropped in the slot immediately after itself — the classic trap.
    {
      label: 'drop immediately after self is a no-op',
      from: 2,
      insertion: 3,
      expected: 2,
      discriminates: true,
    },
  ];

  for (const c of cases) {
    it(`${c.label}: from=${c.from}, insertion=${c.insertion} -> ${c.expected}`, () => {
      expect(insertionToTargetIndex(c.from, c.insertion)).toBe(c.expected);
    });
  }

  it('discriminating cases disagree with the naive identity mutant', () => {
    const disagreements = cases.filter(
      (c) => insertionToTargetIndex(c.from, c.insertion) !== naive(c.from, c.insertion)
    );
    // Non-vacuity proof: at least three of the cases above go RED against
    // `(from, i) => i`. If this ever drops below three the suite has stopped
    // testing the conversion.
    expect(disagreements.map((c) => c.label)).toEqual(
      cases.filter((c) => c.discriminates).map((c) => c.label)
    );
    expect(disagreements.length).toBeGreaterThanOrEqual(3);
  });

  it('is a no-op for every insertion index at or below `from`', () => {
    for (let from = 0; from <= 4; from += 1) {
      for (let insertion = 0; insertion <= from; insertion += 1) {
        expect(insertionToTargetIndex(from, insertion)).toBe(insertion);
      }
    }
  });
});

describe('drag end-to-end (the path where the conversion bites)', () => {
  it('moves a role DOWN past one neighbour to the expected order', () => {
    const { container } = renderRail();
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);

    // Alpha (from=0) dropped before Charlie (insertion=2) -> [Bravo, Alpha, Charlie, Delta].
    dragTo(container, 'Alpha', 2);

    expect(bandOrder(container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);
    // The naive mutant would have produced this instead:
    expect(bandOrder(container)).not.toEqual(['Bravo', 'Charlie', 'Alpha', 'Delta']);
  });

  it('moves a role DOWN past two neighbours to the expected order', () => {
    const { container } = renderRail();

    dragTo(container, 'Alpha', 3);

    expect(bandOrder(container)).toEqual(['Bravo', 'Charlie', 'Alpha', 'Delta']);
    // Naive mutant order:
    expect(bandOrder(container)).not.toEqual(['Bravo', 'Charlie', 'Delta', 'Alpha']);
  });

  it('drops at end-of-list to the last slot', () => {
    const { container } = renderRail();

    dragTo(container, 'Alpha', 4);

    expect(bandOrder(container)).toEqual(['Bravo', 'Charlie', 'Delta', 'Alpha']);
  });

  it('treats a drop immediately after itself as a no-op (no Apply bar)', () => {
    const { container } = renderRail();

    dragTo(container, 'Bravo', 2);

    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
  });

  it('an abandoned drag (dragend) clears the drag state without reordering', () => {
    const { container } = renderRail();
    injectBandGeometry(container);
    const dataTransfer = makeDataTransfer();
    const handle = handleFor(container, 'Alpha');

    fireDragEvent(handle, 'dragstart', dataTransfer);
    expect(dataTransfer.setData).toHaveBeenCalledWith('application/concord-role', 'role-a');
    fireDragEvent(bandRows(container)[2], 'dragover', dataTransfer, clientYForInsertion(2));
    expect(container.querySelector('.role-drag-ghost')).not.toBeNull();

    fireEvent(handle, new MouseEvent('dragend', { bubbles: true, cancelable: true }));

    expect(container.querySelector('.role-drag-ghost')).toBeNull();
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
  });

  it('moves a role upward without applying the conversion', () => {
    const { container } = renderRail();

    dragTo(container, 'Delta', 0);

    expect(bandOrder(container)).toEqual(['Delta', 'Alpha', 'Bravo', 'Charlie']);
  });
});

// ── PRIORITY 2 — the three mechanisms agree ─────────────────────────────────

describe('drag, keyboard and toolbar produce identical orders', () => {
  const EXPECTED_AFTER_BRAVO_DOWN = ['Alpha', 'Charlie', 'Bravo', 'Delta'];

  it('drag moves Bravo down one', () => {
    const { container } = renderRail();
    // Bravo from=1, dropped before Delta (insertion=3) -> target 2.
    dragTo(container, 'Bravo', 3);
    expect(bandOrder(container)).toEqual(EXPECTED_AFTER_BRAVO_DOWN);
  });

  it('Alt+ArrowDown on the handle moves Bravo down one', async () => {
    const { container } = renderRail();
    await makeDirtyViaKeyboard(container, 'Bravo');
    expect(bandOrder(container)).toEqual(EXPECTED_AFTER_BRAVO_DOWN);
  });

  it('the Move down toolbar button moves Bravo down one', async () => {
    const user = userEvent.setup();
    const { container } = renderRail({ selectedRoleId: 'role-b' });
    await user.click(screen.getByRole('button', { name: 'Move down' }));
    expect(bandOrder(container)).toEqual(EXPECTED_AFTER_BRAVO_DOWN);
  });

  it('all three mechanisms commit the same payload for the same logical move', async () => {
    const user = userEvent.setup();
    const payloads: string[][] = [];

    // 1. drag
    {
      const view = renderRail({ selectedRoleId: 'role-b' });
      dragTo(view.container, 'Bravo', 3);
      await user.click(screen.getByRole('button', { name: 'Apply Order' }));
      await waitFor(() => expect(reorderRoles).toHaveBeenCalled());
      payloads.push([...reorderRoles.mock.calls[0][1].role_ids]);
      view.unmount();
      reorderRoles.mockClear();
    }

    // 2. keyboard
    {
      const view = renderRail({ selectedRoleId: 'role-b' });
      await makeDirtyViaKeyboard(view.container, 'Bravo');
      await user.click(screen.getByRole('button', { name: 'Apply Order' }));
      await waitFor(() => expect(reorderRoles).toHaveBeenCalled());
      payloads.push([...reorderRoles.mock.calls[0][1].role_ids]);
      view.unmount();
      reorderRoles.mockClear();
    }

    // 3. toolbar
    {
      const view = renderRail({ selectedRoleId: 'role-b' });
      await user.click(screen.getByRole('button', { name: 'Move down' }));
      await user.click(screen.getByRole('button', { name: 'Apply Order' }));
      await waitFor(() => expect(reorderRoles).toHaveBeenCalled());
      payloads.push([...reorderRoles.mock.calls[0][1].role_ids]);
      view.unmount();
    }

    expect(payloads[0]).toEqual(['role-a', 'role-c', 'role-b', 'role-d']);
    expect(payloads[1]).toEqual(payloads[0]);
    expect(payloads[2]).toEqual(payloads[0]);
  });

  it('Alt+ArrowUp mirrors the Move up toolbar button', async () => {
    const user = userEvent.setup();

    const keyboardView = renderRail();
    const handle = handleFor(keyboardView.container, 'Charlie');
    handle.focus();
    await user.keyboard('{Alt>}{ArrowUp}{/Alt}');
    const keyboardOrder = bandOrder(keyboardView.container);
    keyboardView.unmount();

    const toolbarView = renderRail({ selectedRoleId: 'role-c' });
    await user.click(screen.getByRole('button', { name: 'Move up' }));

    expect(keyboardOrder).toEqual(['Alpha', 'Charlie', 'Bravo', 'Delta']);
    expect(bandOrder(toolbarView.container)).toEqual(keyboardOrder);
  });

  it('Alt+Arrow at a band edge is a no-op and mounts no Apply bar', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    handleFor(container, 'Alpha').focus();
    await user.keyboard('{Alt>}{ArrowUp}{/Alt}');
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
  });

  it('the toolbar buttons disable at the band edges', () => {
    const first = renderRail({ selectedRoleId: 'role-a' });
    expect(screen.getByRole('button', { name: 'Move up' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Move down' })).toBeEnabled();
    first.unmount();

    renderRail({ selectedRoleId: 'role-d' });
    expect(screen.getByRole('button', { name: 'Move up' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Move down' })).toBeDisabled();
  });

  it('the toolbar is inert with nothing selected', () => {
    renderRail({ selectedRoleId: null });
    expect(screen.getByRole('button', { name: 'Move up' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Move down' })).toBeDisabled();
  });
});

describe('unmodified arrows rove focus and never reorder', () => {
  it('ArrowDown moves focus to the next handle, leaving the order alone', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    handleFor(container, 'Alpha').focus();

    await user.keyboard('{ArrowDown}');

    expect(document.activeElement).toBe(handleFor(container, 'Bravo'));
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
  });

  it('ArrowUp roves upward and clamps at the top', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    handleFor(container, 'Bravo').focus();

    await user.keyboard('{ArrowUp}');
    expect(document.activeElement).toBe(handleFor(container, 'Alpha'));

    await user.keyboard('{ArrowUp}');
    expect(document.activeElement).toBe(handleFor(container, 'Alpha'));
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
  });
});

// ── PRIORITY 3 — outcome → UI mapping ───────────────────────────────────────

describe('reorderRoles outcome mapping', () => {
  async function arrangeAndApply(outcome: ReorderOutcome) {
    const user = userEvent.setup();
    reorderRoles.mockResolvedValue(outcome);
    const view = renderRail();
    await makeDirtyViaKeyboard(view.container, 'Alpha');
    expect(bandOrder(view.container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);
    await user.click(screen.getByRole('button', { name: 'Apply Order' }));
    await waitFor(() => expect(reorderRoles).toHaveBeenCalledTimes(1));
    return view;
  }

  it('ok + reconciled clears the draft, unmounts the Apply bar and announces politely', async () => {
    const { container } = await arrangeAndApply({ ok: true, reconciled: true });

    // Gate on the POSITIVE post-await write (tests.md § Async assertions);
    // the absence checks below are then sound as bare assertions.
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Role order saved.'));
    expect(screen.getByRole('status')).toHaveAttribute('aria-live', 'polite');
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('ok + NOT reconciled clears the draft but raises an alert instead of claiming success', async () => {
    await arrangeAndApply({ ok: true, reconciled: false });

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('could not be refreshed');
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
    expect(screen.getByRole('status')).not.toHaveTextContent('Role order saved.');
  });

  it('denied clears and reverts the draft, refetches, and renders the reason verbatim', async () => {
    const reason = 'You cannot move a role above your own highest role.';
    const { container } = await arrangeAndApply({ ok: false, kind: 'denied', reason });

    const alert = await screen.findByRole('alert');
    expect(alert.textContent ?? '').toContain(reason);
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    // Twice: the pre-build reconcile, then the post-denial revert refetch.
    expect(fetchRoles).toHaveBeenCalledWith('server-1');
    expect(fetchRoles).toHaveBeenCalledTimes(2);
  });

  it('throttled PRESERVES the draft and keeps the Apply bar', async () => {
    const { container } = await arrangeAndApply({ ok: false, kind: 'throttled' });

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Too many role changes');
    expect(screen.getByRole('button', { name: 'Apply Order' })).toBeInTheDocument();
    expect(bandOrder(container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);
    // Exactly once — the pre-build reconcile. A throttle must NOT trigger the
    // revert refetch that `denied` does; the draft is still good.
    expect(fetchRoles).toHaveBeenCalledTimes(1);
  });

  it('network failure preserves the draft and keeps the Apply bar', async () => {
    const { container } = await arrangeAndApply({ ok: false, kind: 'network' });

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Could not reach the server.');
    expect(screen.getByRole('button', { name: 'Apply Order' })).toBeInTheDocument();
    expect(bandOrder(container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);
  });

  it('unexpected failure preserves the draft and keeps the Apply bar', async () => {
    const { container } = await arrangeAndApply({ ok: false, kind: 'unexpected' });

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Something went wrong saving the new order.');
    expect(screen.getByRole('button', { name: 'Apply Order' })).toBeInTheDocument();
    expect(bandOrder(container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);
  });

  it('Discard drops the draft without calling the store', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    await makeDirtyViaKeyboard(container, 'Alpha');

    await user.click(screen.getByRole('button', { name: 'Discard' }));

    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('Role order changes discarded.');
    expect(reorderRoles).not.toHaveBeenCalled();
  });

  it('sends the band projection only — no @all, no above-ceiling role', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    await makeDirtyViaKeyboard(container, 'Alpha');
    await user.click(screen.getByRole('button', { name: 'Apply Order' }));

    await waitFor(() => expect(reorderRoles).toHaveBeenCalledTimes(1));
    const [serverId, payload] = reorderRoles.mock.calls[0];
    expect(serverId).toBe('server-1');
    expect(payload.role_ids).toEqual(['role-b', 'role-a', 'role-c', 'role-d']);
    expect(payload.role_ids).not.toContain('role-everyone');
    expect(payload.role_ids).not.toContain('role-owner');
  });
});

// ── PRIORITY 3b — Apply reconciles before it builds ─────────────────────────

describe('Apply reconciles before building the payload (#2859)', () => {
  // The payload must be the COMPLETE band as the server currently sees it. A
  // band missing a role yields a payload that omits it, and the server renumbers
  // only the ids it is given — leaving the omitted role where it sits and
  // committing DUPLICATE POSITIONS under HTTP 200, with no error anywhere. So
  // the rail re-reads immediately before the write rather than trusting the
  // render's `roles` prop.
  it('refetches before it sends, not after', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    await makeDirtyViaKeyboard(container, 'Alpha');

    await user.click(screen.getByRole('button', { name: 'Apply Order' }));

    await waitFor(() => expect(reorderRoles).toHaveBeenCalledTimes(1));
    expect(fetchRoles).toHaveBeenCalledWith('server-1');
    expect(fetchRoles.mock.invocationCallOrder[0]).toBeLessThan(
      reorderRoles.mock.invocationCallOrder[0]
    );
  });

  it('a failed reconcile blocks the write and keeps the draft', async () => {
    // No fresh read means no trustworthy band, so there is no payload this rail
    // may honestly construct. The draft survives because the arrangement is
    // still good — only the read failed.
    const user = userEvent.setup();
    fetchRoles.mockResolvedValue(false);
    const { container } = renderRail();
    await makeDirtyViaKeyboard(container, 'Alpha');

    await user.click(screen.getByRole('button', { name: 'Apply Order' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Could not refresh the role list');
    expect(reorderRoles).not.toHaveBeenCalled();
    expect(bandOrder(container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);
    expect(screen.getByRole('button', { name: 'Apply Order' })).toBeInTheDocument();
  });

  it('discards the draft when the live band is a different id set', async () => {
    // Delta is deleted by another admin mid-arrangement. The draft is a
    // permutation of a band that no longer exists: sending it names a role the
    // server has dropped (404 → `unexpected`, a retry that can never succeed),
    // and sending only the survivors is the silent duplicate-position commit.
    const user = userEvent.setup();
    fetchRoles.mockImplementation(async () => {
      usePermissionStore.setState({
        serverRoles: { 'server-1': [roleOwner, roleA, roleB, roleC, roleEveryone] },
      });
      return true;
    });
    const { container } = renderRail();
    await makeDirtyViaKeyboard(container, 'Alpha');
    expect(bandOrder(container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);

    await user.click(screen.getByRole('button', { name: 'Apply Order' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('changed while you were reordering');
    expect(reorderRoles).not.toHaveBeenCalled();
    expect(bandOrder(container)).toEqual(['Alpha', 'Bravo', 'Charlie', 'Delta']);
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
  });
});

// ── PRIORITY 4 — locked rows and read-only fallbacks ────────────────────────

describe('locked rows', () => {
  it('renders no drag handle for above-ceiling, managed or default rows', () => {
    const { container } = renderRail();
    const rows = allRows(container);
    const named = (name: string) =>
      rows.find((li) => li.querySelector('.role-item-name')?.textContent === name);

    const owner = named('Owner');
    const everyone = named('@all');
    expect(owner?.querySelector('.role-drag-handle')).toBeNull();
    expect(everyone?.querySelector('.role-drag-handle')).toBeNull();
    expect(owner?.querySelector('[draggable="true"]')).toBeNull();
    expect(everyone?.querySelector('[draggable="true"]')).toBeNull();

    // Every band row, by contrast, carries a draggable handle.
    for (const row of bandRows(container)) {
      expect(row.querySelector('.role-drag-handle')).toHaveAttribute('draggable');
    }
  });

  it('reaches each lock reason through aria-describedby on the select button', () => {
    const { container } = renderRail();

    const ownerBtn = screen.getByRole('button', { name: 'Owner' });
    const ownerDescribedBy = ownerBtn.getAttribute('aria-describedby');
    expect(ownerDescribedBy).toBeTruthy();
    expect(document.getElementById(ownerDescribedBy ?? '')?.textContent).toBe(
      'Ranked at or above your own highest role — you cannot move it.'
    );

    const everyoneBtn = screen.getByRole('button', { name: '@all' });
    const everyoneDescribedBy = everyoneBtn.getAttribute('aria-describedby');
    expect(document.getElementById(everyoneDescribedBy ?? '')?.textContent).toBe(
      'The default role always stays last.'
    );

    // The lock glyph slot is not focusable and carries no description itself.
    const lockSlots = container.querySelectorAll('.role-lock-slot');
    for (const slot of Array.from(lockSlots)) {
      expect(slot).not.toHaveAttribute('aria-describedby');
      expect(slot).not.toHaveAttribute('tabindex');
    }
  });

  it('keeps the @all row last', () => {
    const { container } = renderRail();
    const names = rowNames(container);
    expect(names[names.length - 1]).toBe('@all');
  });

  it('keeps the @all row last after a reorder', () => {
    const { container } = renderRail();
    dragTo(container, 'Alpha', 4);
    const names = rowNames(container);
    expect(names[names.length - 1]).toBe('@all');
    expect(names[0]).toBe('Owner');
  });

  it('shows the ceiling boundary chip for a bounded viewer with roles above them', () => {
    renderRail();
    expect(
      screen.getByText('Your role — everything above this line is locked')
    ).toBeInTheDocument();
  });

  it('omits the boundary chip for an owner (nothing is above them)', () => {
    const { container } = renderRail({ viewer: { kind: 'owner' } });
    expect(screen.queryByText('Your role — everything above this line is locked')).toBeNull();
    // The former above-ceiling role is movable for an owner.
    expect(bandOrder(container)).toEqual(['Owner', 'Alpha', 'Bravo', 'Charlie', 'Delta']);
  });
});

describe('read-only presentations', () => {
  function expectReadOnly(container: HTMLElement, noticeFragment: string) {
    const note = screen.getByRole('note');
    expect(note).toHaveTextContent(noticeFragment);
    expect(bandRows(container)).toHaveLength(0);
    expect(container.querySelector('.role-drag-handle')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Move up' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Move down' })).toBeNull();
  }

  it('unknown viewer renders a note and locks every row', () => {
    const { container } = renderRail({ viewer: { kind: 'unknown' } });
    expectReadOnly(container, 'your own place in the role hierarchy could not be determined');
    // Under `{kind:'unknown'}` every role lands in the above-ceiling group,
    // because `isAboveCeiling` fails closed to `true`. The lock REASON must
    // still be the mode-level one: an unknown viewer is by definition one whose
    // rank we could not determine, so "ranked at or above your own highest
    // role" would explain the lock with a fact we do not have.
    const alphaBtn = screen.getByRole('button', { name: 'Alpha' });
    const describedBy = alphaBtn.getAttribute('aria-describedby');
    expect(document.getElementById(describedBy ?? '')?.textContent).toBe(
      'Reordering is unavailable on this server.'
    );
  });

  it('band-exceeds-ceiling renders a note and locks every row', () => {
    // Non-dense positions: three band roles under a ceiling of 3 -> 3 > 3 - 1.
    const roles = [
      makeRole({ id: 'r1', name: 'One', position: 2 }),
      makeRole({ id: 'r2', name: 'Two', position: 2 }),
      makeRole({ id: 'r3', name: 'Three', position: 1 }),
      roleEveryone,
    ];
    const { container } = renderRail({
      roles,
      viewer: { kind: 'bounded', maxRolePosition: 3 },
    });
    expectReadOnly(container, 'more movable roles than your own position allows');
  });

  it('managed-gap renders a note and locks every row', () => {
    const { container } = renderRail({ roles: [roleA, roleB, roleBot, roleEveryone] });
    expectReadOnly(container, 'roles managed by an integration');
    const botBtn = screen.getByRole('button', { name: 'BotIntegration' });
    const describedBy = botBtn.getAttribute('aria-describedby');
    expect(document.getElementById(describedBy ?? '')?.textContent).toBe(
      'Managed by an integration and cannot be moved.'
    );

    // Band rows are locked by the MODE, not by their own group.
    const alphaBtn = screen.getByRole('button', { name: 'Alpha' });
    const alphaDescribedBy = alphaBtn.getAttribute('aria-describedby');
    expect(document.getElementById(alphaDescribedBy ?? '')?.textContent).toBe(
      'Reordering is unavailable on this server.'
    );
  });

  it('a band of fewer than two movable roles renders silently', () => {
    const { container } = renderRail({ roles: [roleA, roleEveryone] });
    expect(screen.queryByRole('note')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Move up' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Move down' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Apply Order' })).toBeNull();
    expect(container.querySelector('.role-drag-handle')).toBeNull();
    // Nothing to explain, so the single row carries no lock description either.
    expect(screen.getByRole('button', { name: 'Alpha' })).not.toHaveAttribute('aria-describedby');
  });

  it('an EMPTY band under a zero ceiling renders silently, not as a ceiling error', () => {
    const { container } = renderRail({
      roles: [roleEveryone],
      viewer: { kind: 'bounded', maxRolePosition: 0 },
    });
    expect(screen.queryByRole('note')).toBeNull();
    expect(container.querySelector('.role-drag-handle')).toBeNull();
    expect(rowNames(container)).toEqual(['@all']);
  });

  it('renders no list at all when the server has no roles', () => {
    const { container } = renderRail({ roles: [] });
    expect(container.querySelector('ul.role-hierarchy')).toBeNull();
    expect(screen.queryByRole('note')).toBeNull();
    expect(screen.getByRole('button', { name: '+ Create Role' })).toBeInTheDocument();
  });
});

// ── PRIORITY 5 — a11y structure ─────────────────────────────────────────────

describe('accessibility structure', () => {
  it('.role-item is a plain li with no nested interactive elements', () => {
    const { container } = renderRail();
    const rows = allRows(container);
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row.tagName).toBe('LI');
      expect(row).not.toHaveAttribute('role');
      expect(row).not.toHaveAttribute('tabindex');
      expect(row.querySelector('button button')).toBeNull();
      expect(row.querySelector('button a')).toBeNull();
      expect(row.querySelector('button [tabindex]')).toBeNull();
      // The select button and the handle are siblings, never nested.
      const interactive = Array.from(row.querySelectorAll('button'));
      for (const el of interactive) {
        expect(el.parentElement).toBe(row);
      }
    }
  });

  it('selects a role through the row button, movable or locked', async () => {
    const user = userEvent.setup();
    const view = renderRail();

    await user.click(screen.getByRole('button', { name: 'Alpha' }));
    expect(view.onSelectRole).toHaveBeenCalledWith('role-a');

    await user.click(screen.getByRole('button', { name: '@all' }));
    expect(view.onSelectRole).toHaveBeenCalledWith('role-everyone');
  });

  it('marks the selected row with aria-current', () => {
    renderRail({ selectedRoleId: 'role-b' });
    expect(screen.getByRole('button', { name: 'Bravo' })).toHaveAttribute('aria-current', 'true');
    expect(screen.getByRole('button', { name: 'Alpha' })).not.toHaveAttribute('aria-current');
  });

  it('exposes the list with an ordering-explicit accessible name', () => {
    renderRail();
    const list = screen.getByRole('list', { name: 'Roles, highest rank first' });
    expect(within(list).getAllByRole('listitem').length).toBeGreaterThan(0);
  });

  it('labels each handle with its ordinal within the movable range', () => {
    const { container } = renderRail();
    expect(handleFor(container, 'Alpha')).toHaveAttribute(
      'aria-label',
      'Reorder Alpha, position 1 of 4 in the movable range'
    );
    expect(handleFor(container, 'Delta')).toHaveAttribute(
      'aria-label',
      'Reorder Delta, position 4 of 4 in the movable range'
    );
  });

  it('keeps focus on the moved row handle after a keyboard move', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    const handle = handleFor(container, 'Alpha');
    handle.focus();

    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');

    expect(bandOrder(container)).toEqual(['Bravo', 'Alpha', 'Charlie', 'Delta']);
    expect(document.activeElement).toBe(handle);
    expect(document.activeElement).toBe(handleFor(container, 'Alpha'));
    expect(handle).toHaveAttribute(
      'aria-label',
      'Reorder Alpha, position 2 of 4 in the movable range'
    );
  });

  it('announces a keyboard move relative to its new neighbour', async () => {
    const user = userEvent.setup();
    const { container } = renderRail();
    handleFor(container, 'Alpha').focus();

    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');

    expect(screen.getByRole('status')).toHaveTextContent('Alpha moved below Bravo');
  });

  it('uses role="status" for the live region and role="alert" for the error banner', async () => {
    const user = userEvent.setup();
    reorderRoles.mockResolvedValue({ ok: false, kind: 'throttled' });
    const { container } = renderRail();
    expect(screen.getByRole('status')).toHaveAttribute('aria-live', 'polite');

    await makeDirtyViaKeyboard(container, 'Alpha');
    await user.click(screen.getByRole('button', { name: 'Apply Order' }));

    const alert = await screen.findByRole('alert');
    expect(alert).not.toHaveAttribute('aria-live', 'polite');
    expect(alert.className).toContain('role-reorder-alert');
  });
});
