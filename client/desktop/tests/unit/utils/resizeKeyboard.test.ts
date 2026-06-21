import type { KeyboardEvent } from 'react';
import { createResizeKeyHandler, type ResizeKeyOptions } from '@/renderer/utils/resizeKeyboard';

describe('createResizeKeyHandler', () => {
  const makeHandler = (overrides: Partial<ResizeKeyOptions> = {}) => {
    const opts = {
      axis: 'horizontal' as const,
      direction: 'grow' as const,
      min: 100,
      max: 500,
      getValue: () => 300,
      setValue: vi.fn(),
      ...overrides,
    };
    return { handler: createResizeKeyHandler(opts), opts };
  };

  const keyEvent = (key: string, shiftKey = false) =>
    ({
      key,
      shiftKey,
      preventDefault: vi.fn(),
    }) as unknown as KeyboardEvent;

  it('ArrowRight increases value for horizontal grow', () => {
    const { handler, opts } = makeHandler();
    const e = keyEvent('ArrowRight');
    handler(e);
    expect(opts.setValue).toHaveBeenCalledWith(310);
    expect(e.preventDefault).toHaveBeenCalled();
  });

  it('ArrowLeft decreases value for horizontal grow', () => {
    const { handler, opts } = makeHandler();
    const e = keyEvent('ArrowLeft');
    handler(e);
    expect(opts.setValue).toHaveBeenCalledWith(290);
    expect(e.preventDefault).toHaveBeenCalled();
  });

  it('ArrowDown increases value for vertical grow', () => {
    const { handler, opts } = makeHandler({ axis: 'vertical' });
    const e = keyEvent('ArrowDown');
    handler(e);
    expect(opts.setValue).toHaveBeenCalledWith(310);
  });

  it('ArrowUp decreases value for vertical grow', () => {
    const { handler, opts } = makeHandler({ axis: 'vertical' });
    const e = keyEvent('ArrowUp');
    handler(e);
    expect(opts.setValue).toHaveBeenCalledWith(290);
  });

  it('Shift+Arrow uses large step of 50px', () => {
    const { handler, opts } = makeHandler();
    handler(keyEvent('ArrowRight', true));
    expect(opts.setValue).toHaveBeenCalledWith(350);
  });

  it('clamps to max bound', () => {
    const { handler, opts } = makeHandler({ getValue: () => 495 });
    handler(keyEvent('ArrowRight'));
    expect(opts.setValue).toHaveBeenCalledWith(500);
  });

  it('clamps to min bound', () => {
    const { handler, opts } = makeHandler({ getValue: () => 105 });
    handler(keyEvent('ArrowLeft'));
    expect(opts.setValue).toHaveBeenCalledWith(100);
  });

  it('does not handle unrelated keys', () => {
    const { handler, opts } = makeHandler();
    const e = keyEvent('Enter');
    handler(e);
    expect(opts.setValue).not.toHaveBeenCalled();
    expect(e.preventDefault).not.toHaveBeenCalled();
  });

  it('shrink direction inverts delta', () => {
    const { handler, opts } = makeHandler({ direction: 'shrink' });
    const e = keyEvent('ArrowRight');
    handler(e);
    expect(opts.setValue).toHaveBeenCalledWith(290); // shrink: ArrowRight decreases
  });
});
