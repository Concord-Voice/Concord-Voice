import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/react';
import { useRef } from 'react';
import { useDialogSync } from '@/renderer/hooks/ui/useDialogSync';

function Harness({ open, onRef }: { open: boolean; onRef: (el: HTMLDialogElement) => void }) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  useDialogSync(dialogRef, open);
  return (
    <dialog
      ref={(el) => {
        dialogRef.current = el;
        if (el) onRef(el);
      }}
    />
  );
}

describe('useDialogSync', () => {
  afterEach(() => {
    cleanup();
  });

  it('calls showModal() once when open flips true and the dialog was not already open', () => {
    let dialogEl: HTMLDialogElement | undefined;
    const showModal = vi.fn(function (this: HTMLDialogElement) {
      this.open = true;
    });

    const { rerender } = render(
      <Harness
        open={false}
        onRef={(el) => {
          dialogEl = el;
          el.showModal = showModal;
        }}
      />
    );
    expect(dialogEl?.open).toBeFalsy();

    rerender(
      <Harness
        open={true}
        onRef={(el) => {
          dialogEl = el;
        }}
      />
    );

    expect(showModal).toHaveBeenCalledTimes(1);
  });

  it('does not call showModal when the dialog element is already open', () => {
    let dialogEl: HTMLDialogElement | undefined;
    const showModal = vi.fn();

    const { rerender } = render(
      <Harness
        open={false}
        onRef={(el) => {
          dialogEl = el;
          el.showModal = showModal;
        }}
      />
    );
    expect(dialogEl).toBeDefined();
    // Simulate a dialog already open at the DOM level before the open prop
    // flips true — set AFTER the initial effect ran (the initial open=false
    // pass would otherwise close() it). The `open && !el.open` guard must
    // skip showModal; removing the guard makes this call showModal.
    dialogEl!.open = true;

    rerender(
      <Harness
        open={true}
        onRef={() => {
          /* no-op */
        }}
      />
    );

    expect(showModal).not.toHaveBeenCalled();
  });

  it('falls back to setAttribute("open") when showModal throws', () => {
    let dialogEl: HTMLDialogElement | undefined;
    const showModal = vi.fn(() => {
      throw new Error('not supported');
    });
    const setAttributeSpy = vi.fn();

    const { rerender } = render(
      <Harness
        open={false}
        onRef={(el) => {
          dialogEl = el;
          el.showModal = showModal;
          el.setAttribute = setAttributeSpy as unknown as typeof el.setAttribute;
        }}
      />
    );
    expect(dialogEl).toBeDefined();

    rerender(
      <Harness
        open={true}
        onRef={() => {
          /* no-op */
        }}
      />
    );

    expect(showModal).toHaveBeenCalledTimes(1);
    expect(setAttributeSpy).toHaveBeenCalledWith('open', '');
  });

  it('falls back to setAttribute("open") when showModal is absent', () => {
    let dialogEl: HTMLDialogElement | undefined;
    const setAttributeSpy = vi.fn();

    const { rerender } = render(
      <Harness
        open={false}
        onRef={(el) => {
          dialogEl = el;
          Object.defineProperty(el, 'showModal', {
            value: undefined,
            configurable: true,
          });
          el.setAttribute = setAttributeSpy as unknown as typeof el.setAttribute;
        }}
      />
    );
    expect(dialogEl).toBeDefined();

    rerender(
      <Harness
        open={true}
        onRef={() => {
          /* no-op */
        }}
      />
    );

    expect(setAttributeSpy).toHaveBeenCalledWith('open', '');
  });

  it('calls close() when open flips false while the dialog is open', () => {
    let dialogEl: HTMLDialogElement | undefined;
    const close = vi.fn(function (this: HTMLDialogElement) {
      this.open = false;
    });

    const { rerender } = render(
      <Harness
        open={true}
        onRef={(el) => {
          dialogEl = el;
          el.open = true;
          el.close = close;
        }}
      />
    );
    expect(dialogEl).toBeDefined();

    rerender(
      <Harness
        open={false}
        onRef={() => {
          /* no-op */
        }}
      />
    );

    expect(close).toHaveBeenCalledTimes(1);
  });

  it('does not call close() when open is already false', () => {
    let dialogEl: HTMLDialogElement | undefined;
    const close = vi.fn();

    const { rerender } = render(
      <Harness
        open={false}
        onRef={(el) => {
          dialogEl = el;
          el.close = close;
        }}
      />
    );
    expect(dialogEl).toBeDefined();

    rerender(
      <Harness
        open={false}
        onRef={() => {
          /* no-op */
        }}
      />
    );

    expect(close).not.toHaveBeenCalled();
  });
});
