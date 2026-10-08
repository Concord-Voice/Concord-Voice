// #3456 §3.7: the bits a server may gate behind MFA are marked in the role
// editor's grid, in both modes, and no other bit is. The grid never masks or
// disables a control: it edits a role, not the viewer's own grants.
//
// Each test names, in its first comment line, the production mutation that
// turns it red.
import { render, screen, within, fireEvent } from '../../../test-utils';
import { readCss, ruleBody } from '../../../helpers/cssRules';

vi.mock('@/renderer/components/Permissions/PermissionGrid.css', () => ({}));

import PermissionGrid from '@/renderer/components/Permissions/PermissionGrid';
import {
  ADMINISTRATOR,
  MANAGE_CRYPTO_ROTATION,
  MANAGE_DEV_RESOURCES,
  PERMISSION_CATEGORIES,
} from '@/renderer/utils/policy/permissions';

const SENTENCE = 'Turning this on needs MFA confirmation on servers that enforce MFA.';

// The X9 set, written out rather than read from the flag under test.
const DANGEROUS = [
  { label: 'Administrator', bit: ADMINISTRATOR },
  { label: 'Manage Developer Resources', bit: MANAGE_DEV_RESOURCES },
  { label: 'Manage E2EE Keys', bit: MANAGE_CRYPTO_ROTATION },
];
const DANGEROUS_LABELS = new Set(DANGEROUS.map((d) => d.label));

const ALL_PERMS = PERMISSION_CATEGORIES.flatMap((c) => c.permissions);
const OTHER_PERMS = ALL_PERMS.filter((p) => !DANGEROUS_LABELS.has(p.label));

const noop = () => {};

/** The `.permission-row` that carries `label`. */
function rowOf(container: HTMLElement, label: string): HTMLElement {
  const row = Array.from(container.querySelectorAll<HTMLElement>('.permission-row')).find(
    (r) => within(r).queryByText(label, { selector: '.permission-label' }) !== null
  );
  if (!row) throw new Error(`no row for ${label}`);
  return row;
}

/** The control a row's note describes: the switch in role mode, the Allow button in override mode. */
function controlOf(row: HTMLElement, mode: 'role' | 'override'): HTMLElement {
  return mode === 'role' ? within(row).getByRole('switch') : within(row).getByTitle('Allow');
}

function renderGrid(mode: 'role' | 'override', extra: { value?: bigint; disabled?: boolean } = {}) {
  return render(
    <PermissionGrid
      value={extra.value ?? 0n}
      onChange={noop}
      mode={mode}
      deny={0n}
      onDenyChange={noop}
      disabled={extra.disabled}
    />
  );
}

describe.each(['role', 'override'] as const)(
  'PermissionGrid dangerous marking (%s mode)',
  (mode) => {
    it('the marked set is exactly the three dangerous bits', () => {
      // Mutation: drop `dangerous: true` from, or add it to, any PERMISSION_CATEGORIES entry.
      const { container } = renderGrid(mode);
      const marked = Array.from(container.querySelectorAll('.permission-row'))
        .filter((r) => r.querySelector('.permission-mfa-note') !== null)
        .map((r) => r.querySelector('.permission-label')?.textContent);
      expect(marked.sort()).toEqual([...DANGEROUS_LABELS].sort());
      expect(screen.getAllByText(SENTENCE)).toHaveLength(3);
    });

    it.each(DANGEROUS)('$label says the sentence next to a decorative glyph', ({ label }) => {
      // Mutation: the note renders the glyph without aria-hidden, or the sentence text changes.
      const { container } = renderGrid(mode);
      const note = rowOf(container, label).querySelector<HTMLElement>('.permission-mfa-note');
      expect(note).not.toBeNull();
      expect(within(note as HTMLElement).getByText(SENTENCE)).toBeVisible();
      const glyph = (note as HTMLElement).querySelector('svg');
      expect(glyph).not.toBeNull();
      expect(glyph).toHaveAttribute('aria-hidden', 'true');
      expect(glyph).toHaveAttribute('width', '14');
    });

    it.each(DANGEROUS)('$label control is described by its own note', ({ label }) => {
      // Mutation: aria-describedby is dropped, or points at the wrong row's id.
      const { container } = renderGrid(mode);
      const row = rowOf(container, label);
      const describedBy = controlOf(row, mode).getAttribute('aria-describedby');
      expect(describedBy).toBeTruthy();
      const target = document.getElementById(describedBy as string);
      expect(target).toBe(row.querySelector('.permission-mfa-note'));
      expect(target).toHaveTextContent(SENTENCE);
    });

    it('no other bit is marked, described or glyphed', () => {
      // Mutation: the note renders for every row (the `dangerous` guard is removed).
      const { container } = renderGrid(mode);
      expect(OTHER_PERMS.length).toBeGreaterThan(10);
      for (const perm of OTHER_PERMS) {
        const row = rowOf(container, perm.label);
        expect(row.querySelector('.permission-mfa-note')).toBeNull();
        expect(row.querySelector('svg')).toBeNull();
        expect(controlOf(row, mode)).not.toHaveAttribute('aria-describedby');
      }
    });

    it('never masks or disables a dangerous control', () => {
      // Mutation: a dangerous row is rendered aria-disabled/disabled (the viewer's grants gate it).
      const { container } = renderGrid(mode);
      for (const { label } of DANGEROUS) {
        const control = controlOf(rowOf(container, label), mode);
        expect(control).not.toBeDisabled();
        expect(control).not.toHaveAttribute('aria-disabled');
      }
    });

    it('a dangerous bit toggles like any other', () => {
      // Mutation: handleToggle/handleOverride skips `dangerous` rows.
      const onChange = vi.fn();
      const { container } = render(
        <PermissionGrid value={0n} onChange={onChange} mode={mode} deny={0n} onDenyChange={noop} />
      );
      fireEvent.click(controlOf(rowOf(container, 'Administrator'), mode));
      expect(onChange).toHaveBeenCalledWith(ADMINISTRATOR);
    });

    it('says it whether or not the bit is on, and while the grid is disabled', () => {
      // Mutation: the note is rendered only for an unset bit, or only for an enabled grid.
      const on = renderGrid(mode, { value: ADMINISTRATOR });
      expect(screen.getAllByText(SENTENCE)).toHaveLength(3);
      on.unmount();
      renderGrid(mode, { disabled: true });
      expect(screen.getAllByText(SENTENCE)).toHaveLength(3);
    });
  }
);

describe('PermissionGrid dangerous marking ids', () => {
  it('two grids on one page do not share note ids', () => {
    // Mutation: the note id is a constant instead of derived from useId().
    const { container } = render(
      <>
        <PermissionGrid value={0n} onChange={noop} />
        <PermissionGrid value={0n} onChange={noop} />
      </>
    );
    const ids = Array.from(container.querySelectorAll('.permission-mfa-note')).map((n) => n.id);
    expect(ids).toHaveLength(6);
    expect(new Set(ids).size).toBe(6);
    // Each switch resolves to the note inside its own grid.
    const grids = container.querySelectorAll('.permission-grid');
    for (const grid of Array.from(grids)) {
      const sw = within(grid as HTMLElement).getByRole('switch', { name: 'Administrator' });
      const target = document.getElementById(sw.getAttribute('aria-describedby') as string);
      expect(grid.contains(target)).toBe(true);
    }
  });
});

describe('PermissionGrid mfa note stylesheet', () => {
  const css = readCss('src/renderer/components/Permissions/PermissionGrid.css');

  it('says the sentence in --text-primary and keeps the glyph at --text-secondary', () => {
    // Mutation: the sentence moves to --text-secondary (4.31:1 in Midnight Sky) or --danger (fails AA).
    const note = ruleBody(css, '.permission-mfa-note') ?? '';
    expect(note).toMatch(/color:\s*var\(--text-primary\)\s*;/);
    expect(note).not.toMatch(/--danger/);
    const glyph = ruleBody(css, '.permission-mfa-note svg') ?? '';
    expect(glyph).toMatch(/color:\s*var\(--text-secondary\)\s*;/);
    expect(glyph).not.toMatch(/--danger/);
  });
});
