import fs from 'node:fs';
import path from 'node:path';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, act, waitFor } from '../../../test-utils';
import CollapsibleSection from '@/renderer/components/Settings/CollapsibleSection';
import { useSettingsCollapsibleStore } from '@/renderer/stores/ui/settingsCollapsibleStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// jsdom dispatches `toggle` on an `open` attribute change through a queued task, so
// every test that depends on the store learning a DOM change gates on
// `await waitFor(...)` against the store (real timers; never fake timers here).

const storeOpen = (id: string) => useSettingsCollapsibleStore.getState().openSections[id];

function renderSection(id = 'x', children: React.ReactNode = <p>body</p>) {
  return render(
    <CollapsibleSection id={id} title="Section X">
      {children}
    </CollapsibleSection>
  );
}

function detailsFor(container: HTMLElement, id = 'x'): HTMLDetailsElement {
  const el = container.querySelector<HTMLDetailsElement>(`details#${id}`);
  if (!el) throw new Error(`details#${id} not rendered`);
  return el;
}

describe('CollapsibleSection (#2365)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('C1 renders collapsed for an id the store has never seen', () => {
    useSettingsCollapsibleStore.setState({ openSections: { other: true } });

    const { container } = renderSection('x');

    // Positive gate: the section rendered, so "no open attribute" is not vacuous.
    const details = detailsFor(container, 'x');
    expect(details).toHaveClass('settings-collapsible');
    expect(details).not.toHaveAttribute('open');
    expect(details.open).toBe(false);
  });

  it('C2 reopens after a remount once the user opened it', async () => {
    const first = renderSection('x');
    const firstDetails = detailsFor(first.container);
    // Pair against vacuity: it starts collapsed, so the reopen below is the store's doing.
    expect(firstDetails).not.toHaveAttribute('open');

    // User path: clicking the <summary> toggles `open` through jsdom's activation behaviour.
    const summary = firstDetails.querySelector('summary');
    expect(summary).not.toBeNull();
    act(() => {
      summary?.click();
    });
    expect(firstDetails.open).toBe(true);
    await waitFor(() => expect(storeOpen('x')).toBe(true));

    first.unmount();

    const second = renderSection('x');
    // Synchronous: the remembered state is in the first commit, not an effect away.
    expect(detailsFor(second.container)).toHaveAttribute('open');
  });

  it('C3 stays collapsed after a remount once the user closed it', async () => {
    useSettingsCollapsibleStore.setState({ openSections: { x: true } });

    const first = renderSection('x');
    const firstDetails = detailsFor(first.container);
    expect(firstDetails).toHaveAttribute('open');

    const summary = firstDetails.querySelector('summary');
    expect(summary).not.toBeNull();
    act(() => {
      summary?.click();
    });
    expect(firstDetails.open).toBe(false);
    await waitFor(() => expect(storeOpen('x')).toBe(false));

    first.unmount();

    const second = renderSection('x');
    expect(detailsFor(second.container)).not.toHaveAttribute('open');
  });

  it('C4 records a script-driven details.open = true', async () => {
    const { container } = renderSection('x');
    const details = detailsFor(container);
    expect(storeOpen('x')).toBeUndefined();

    // The four untouched DOM writers (Expand all, sub-nav scroll, focus request, update
    // indicator) all do exactly this; the store must learn it from the `toggle` event.
    details.open = true;

    await waitFor(() => expect(storeOpen('x')).toBe(true));
  });

  it('C5 a nested <details> toggling does not rewrite the section memory', async () => {
    useSettingsCollapsibleStore.setState({ openSections: { x: true } });

    const { container } = renderSection(
      'x',
      <details data-testid="inner">
        <summary>Inner</summary>
        <p>inner body</p>
      </details>
    );
    const section = detailsFor(container, 'x');
    expect(section).toHaveAttribute('open');

    const inner = container.querySelector<HTMLDetailsElement>('[data-testid="inner"]');
    expect(inner).not.toBeNull();
    if (!inner) return;

    // Registered AFTER render, so React's own listener on the inner element runs first in
    // the same dispatch: once this spy has fired, any (wrong) write has already happened.
    const spy = vi.fn();
    inner.addEventListener('toggle', spy);

    inner.open = true;
    await waitFor(() => expect(spy).toHaveBeenCalledTimes(1));

    inner.open = false;
    await waitFor(() => expect(spy).toHaveBeenCalledTimes(2));

    // Bare assertion: the gate above already proves the handler ran for both toggles.
    expect(storeOpen('x')).toBe(true);
    expect(section).toHaveAttribute('open');
    expect(inner.open).toBe(false);
  });

  it('C6 follows the store while mounted', () => {
    const { container } = renderSection('x');
    const details = detailsFor(container);
    expect(details).not.toHaveAttribute('open');

    act(() => {
      useSettingsCollapsibleStore.getState().setSectionOpen('x', true);
    });

    expect(details).toHaveAttribute('open');

    act(() => {
      useSettingsCollapsibleStore.getState().setSectionOpen('x', false);
    });

    expect(details).not.toHaveAttribute('open');
  });

  it('C7 every CollapsibleSection id is a unique string literal', () => {
    const root = path.resolve(process.cwd(), 'src/renderer/components/Settings');
    const files = (fs.readdirSync(root, { recursive: true }) as string[])
      .filter((f) => f.endsWith('.tsx') && !/\.test\.tsx$/.test(f))
      .map((f) => path.join(root, f));
    expect(files.length).toBeGreaterThan(0);

    let usages = 0;
    const literalIds: string[] = [];
    for (const file of files) {
      const source = fs.readFileSync(file, 'utf8');
      usages += [...source.matchAll(/<CollapsibleSection\s/g)].length;
      for (const match of source.matchAll(/<CollapsibleSection\s+id="([^"]+)"/g)) {
        literalIds.push(match[1]);
      }
    }

    // Positive gate: the scan found real call sites, so equal counts are not 0 === 0.
    expect(usages).toBeGreaterThan(0);
    // A dynamic `id={...}` (or any non-literal first prop) would make these differ.
    expect(literalIds).toHaveLength(usages);
    // A duplicate id would make two sections share one remembered state.
    const duplicates = literalIds.filter((id, i) => literalIds.indexOf(id) !== i);
    expect(duplicates).toEqual([]);
  });

  it('C8 onCollapse fires once per close of THIS section, after the store recorded it (#3635)', async () => {
    useSettingsCollapsibleStore.setState({ openSections: { x: true } });
    // Ordering pin: record what the memory said when the callback ran, and assert on it in
    // the test body — an `expect` inside a React event handler is not a reliable failure.
    const storeAtCallback: Array<boolean | undefined> = [];
    const onCollapse = vi.fn(() => {
      storeAtCallback.push(storeOpen('x'));
    });

    const { container } = render(
      <CollapsibleSection id="x" title="Section X" onCollapse={onCollapse}>
        <details data-testid="inner">
          <summary>Inner</summary>
          <p>inner body</p>
        </details>
      </CollapsibleSection>
    );
    const section = detailsFor(container, 'x');
    const summary = section.querySelector('summary');
    expect(summary).not.toBeNull();

    // Close by click: one call, after the store already said closed.
    act(() => {
      summary?.click();
    });
    expect(section.open).toBe(false);
    await waitFor(() => expect(onCollapse).toHaveBeenCalledTimes(1));
    expect(storeAtCallback).toEqual([false]);

    // Reopen: the store learns it, the callback does not fire.
    act(() => {
      summary?.click();
    });
    await waitFor(() => expect(storeOpen('x')).toBe(true));
    expect(onCollapse).toHaveBeenCalledTimes(1);

    // A nested <details> closing bubbles through the same handler and must not count.
    const inner = container.querySelector<HTMLDetailsElement>('[data-testid="inner"]');
    expect(inner).not.toBeNull();
    if (!inner) return;
    const innerToggles = vi.fn();
    inner.addEventListener('toggle', innerToggles);
    inner.open = true;
    await waitFor(() => expect(innerToggles).toHaveBeenCalledTimes(1));
    inner.open = false;
    await waitFor(() => expect(innerToggles).toHaveBeenCalledTimes(2));
    expect(onCollapse).toHaveBeenCalledTimes(1);
    expect(storeOpen('x')).toBe(true);

    // A script-driven close (Collapse all) counts like a click.
    section.open = false;
    await waitFor(() => expect(onCollapse).toHaveBeenCalledTimes(2));
  });

  it('C9 a nested <details> toggling under a CLOSED section fires no onCollapse (#3635)', async () => {
    // This is the case that makes the same-target guard load-bearing: with the section
    // collapsed, an unguarded handler would read `currentTarget.open === false` for the
    // INNER toggle and fire onCollapse for a close the user never made.
    const onCollapse = vi.fn();
    const { container } = render(
      <CollapsibleSection id="x" title="Section X" onCollapse={onCollapse}>
        <details data-testid="inner">
          <summary>Inner</summary>
          <p>inner body</p>
        </details>
      </CollapsibleSection>
    );
    const section = detailsFor(container, 'x');
    expect(section.open).toBe(false);

    const inner = container.querySelector<HTMLDetailsElement>('[data-testid="inner"]');
    expect(inner).not.toBeNull();
    if (!inner) return;
    const innerToggles = vi.fn();
    inner.addEventListener('toggle', innerToggles);
    inner.open = true;
    await waitFor(() => expect(innerToggles).toHaveBeenCalledTimes(1));
    inner.open = false;
    await waitFor(() => expect(innerToggles).toHaveBeenCalledTimes(2));

    // Bare negatives, gated by the two inner toggles above having dispatched.
    expect(onCollapse).not.toHaveBeenCalled();
    expect(storeOpen('x')).toBeUndefined();
    expect(section.open).toBe(false);

    // Positive control: the section's OWN close reaches the callback through the same harness.
    act(() => {
      useSettingsCollapsibleStore.getState().setSectionOpen('x', true);
    });
    expect(section.open).toBe(true);
    const summary = section.querySelector('summary');
    act(() => {
      summary?.click();
    });
    await waitFor(() => expect(onCollapse).toHaveBeenCalledTimes(1));
  });
});
