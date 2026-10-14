import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen } from '../../../test-utils';
import CollapsibleSection from '@/renderer/components/Settings/CollapsibleSection';
import RememberedDisclosure from '@/renderer/components/Settings/RememberedDisclosure';
import { useSettingsCollapsibleStore } from '@/renderer/stores/ui/settingsCollapsibleStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { resetAllStores } from '../../../helpers/store-helpers';

function details(id: string): HTMLDetailsElement {
  const node = document.getElementById(id);
  if (!(node instanceof HTMLDetailsElement)) throw new Error(`#${id} is not a disclosure`);
  return node;
}

function toggle(id: string) {
  const node = details(id);
  const summary = node.querySelector(':scope > summary');
  if (!summary) throw new Error(`#${id} has no direct summary`);
  fireEvent.click(summary);
  fireEvent(node, new Event('toggle', { bubbles: true }));
}

function NestedDisclosure({ onCollapse }: { onCollapse?: () => void }) {
  return (
    <CollapsibleSection id="section-fonts" title="Application Font" onCollapse={onCollapse}>
      <RememberedDisclosure
        id="font-area-messages"
        summary={<span>Messages font</span>}
        className="font-area-row"
        summaryClassName="font-area-summary"
      >
        <button type="button">Use this font</button>
      </RememberedDisclosure>
    </CollapsibleSection>
  );
}

function NestedOverlay() {
  const open = useSettingsOverlayStore((state) => state.open);
  return open === 'app' ? <NestedDisclosure /> : null;
}

describe('RememberedDisclosure shares Settings expansion memory', () => {
  beforeEach(() => {
    resetAllStores();
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-13T12:00:00Z'));
  });

  afterEach(() => {
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  it('renders a collapsed native disclosure with its provided summary and body', () => {
    render(<NestedDisclosure />);
    const nested = details('font-area-messages');

    expect(nested).not.toHaveAttribute('open');
    expect(nested).toHaveClass('font-area-row');
    expect(nested.querySelector(':scope > summary')).toHaveClass('font-area-summary');
    expect(screen.getByText('Messages font')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Use this font', hidden: true })).toBeInTheDocument();
  });

  it('remembers a nested toggle without changing or closing the outer disclosure', () => {
    const onCollapse = vi.fn();
    render(<NestedDisclosure onCollapse={onCollapse} />);
    toggle('section-fonts');
    toggle('font-area-messages');
    expect(details('section-fonts')).toHaveAttribute('open');
    expect(details('font-area-messages')).toHaveAttribute('open');
    expect(useSettingsCollapsibleStore.getState().openSections['font-area-messages']).toBe(true);

    onCollapse.mockClear();
    toggle('font-area-messages');
    expect(details('font-area-messages')).not.toHaveAttribute('open');
    expect(details('section-fonts')).toHaveAttribute('open');
    expect(useSettingsCollapsibleStore.getState().openSections['section-fonts']).toBe(true);
    expect(onCollapse).not.toHaveBeenCalled();
  });

  it('restores both levels after a pane pivot unmounts and remounts them', () => {
    const pane = render(<NestedDisclosure />);
    toggle('section-fonts');
    toggle('font-area-messages');
    pane.rerender(<p>Another Settings pane</p>);
    expect(screen.getByText('Another Settings pane')).toBeInTheDocument();
    expect(document.getElementById('section-fonts')).toBeNull();

    pane.rerender(<NestedDisclosure />);
    expect(details('section-fonts')).toHaveAttribute('open');
    expect(details('font-area-messages')).toHaveAttribute('open');
  });

  it('restores a nested disclosure before expiry and collapses both levels after expiry', () => {
    render(<NestedOverlay />);
    act(() => useSettingsOverlayStore.getState().openSettings('app'));
    toggle('section-fonts');
    toggle('font-area-messages');
    act(() => useSettingsOverlayStore.getState().close());
    act(() => vi.advanceTimersByTime(29 * 60 * 1000 + 59 * 1000));
    act(() => useSettingsOverlayStore.getState().openSettings('app'));
    expect(details('section-fonts')).toHaveAttribute('open');
    expect(details('font-area-messages')).toHaveAttribute('open');

    act(() => useSettingsOverlayStore.getState().close());
    act(() => vi.advanceTimersByTime(30 * 60 * 1000));
    act(() => useSettingsOverlayStore.getState().openSettings('app'));
    expect(details('section-fonts')).not.toHaveAttribute('open');
    expect(details('font-area-messages')).not.toHaveAttribute('open');
  });

  it('records closing before notifying onCollapse', () => {
    const onCollapse = vi.fn();
    render(
      <RememberedDisclosure id="benefits" summary="View benefits" onCollapse={onCollapse}>
        Current benefits
      </RememberedDisclosure>
    );
    toggle('benefits');
    expect(details('benefits')).toHaveAttribute('open');
    onCollapse.mockImplementation(() => {
      expect(useSettingsCollapsibleStore.getState().openSections.benefits).toBe(false);
    });
    toggle('benefits');

    expect(details('benefits')).not.toHaveAttribute('open');
    expect(onCollapse).toHaveBeenCalledTimes(1);
  });
});
