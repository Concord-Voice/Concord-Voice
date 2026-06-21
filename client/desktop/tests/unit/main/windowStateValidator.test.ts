import { describe, it, expect } from 'vitest';
import {
  validateBounds,
  type SavedState,
  type DisplayInfo,
} from '../../../src/main/windowStateValidator';

const primaryDisplay: DisplayInfo = {
  workArea: { x: 0, y: 0, width: 1920, height: 1080 },
};

const secondaryDisplay: DisplayInfo = {
  workArea: { x: 1920, y: 0, width: 1920, height: 1080 },
};

const validSaved: SavedState = {
  x: 100,
  y: 100,
  width: 1200,
  height: 800,
  isMaximized: false,
};

describe('validateBounds — Check 1: NaN / missing / non-numeric', () => {
  it('rejects NaN x', () => {
    expect(validateBounds({ ...validSaved, x: NaN }, [primaryDisplay])).toBeNull();
  });

  it('rejects string-typed numeric field', () => {
    expect(
      validateBounds({ ...validSaved, width: '1200' as unknown as number }, [primaryDisplay])
    ).toBeNull();
  });

  it('rejects missing field', () => {
    const incomplete = { width: 1200, height: 800 } as unknown as SavedState;
    expect(validateBounds(incomplete, [primaryDisplay])).toBeNull();
  });

  it('rejects null input', () => {
    expect(validateBounds(null as unknown as SavedState, [primaryDisplay])).toBeNull();
  });

  it('rejects Infinity', () => {
    expect(validateBounds({ ...validSaved, x: Infinity }, [primaryDisplay])).toBeNull();
  });
});

describe('validateBounds — Check 2: display intersection', () => {
  it('accepts saved bounds fully within primary display', () => {
    expect(validateBounds(validSaved, [primaryDisplay])).toEqual(validSaved);
  });

  it('accepts saved bounds with partial visibility ≥ 50×50 px', () => {
    expect(validateBounds({ ...validSaved, x: 1870 }, [primaryDisplay])).not.toBeNull();
  });

  it('rejects saved bounds on detached display (monitor unplugged)', () => {
    expect(validateBounds({ ...validSaved, x: 2120 }, [primaryDisplay])).toBeNull();
  });

  it('accepts saved bounds when secondary display is still present', () => {
    expect(
      validateBounds({ ...validSaved, x: 2120 }, [primaryDisplay, secondaryDisplay])
    ).not.toBeNull();
  });

  it('rejects when intersection is below 50×50 threshold', () => {
    expect(validateBounds({ ...validSaved, x: 1900 }, [primaryDisplay])).toBeNull();
  });
});

describe('validateBounds — Check 3: min/max size sanity', () => {
  it('rejects width < 800 (below minWidth)', () => {
    expect(validateBounds({ ...validSaved, width: 200, height: 800 }, [primaryDisplay])).toBeNull();
  });

  it('rejects height < 600 (below minHeight)', () => {
    expect(
      validateBounds({ ...validSaved, width: 1200, height: 100 }, [primaryDisplay])
    ).toBeNull();
  });

  it('rejects oversized width beyond all display widths', () => {
    expect(
      validateBounds({ ...validSaved, width: 99999, height: 800 }, [primaryDisplay])
    ).toBeNull();
  });

  it('accepts width at the limit (sum of all display widths)', () => {
    expect(
      validateBounds({ x: 0, y: 0, width: 3840, height: 800, isMaximized: false }, [
        primaryDisplay,
        secondaryDisplay,
      ])
    ).not.toBeNull();
  });
});

describe('validateBounds — Check 4: negative coords off-screen guard', () => {
  it('rejects x pushing window fully off-screen left', () => {
    expect(validateBounds({ ...validSaved, x: -1300, width: 1200 }, [primaryDisplay])).toBeNull();
  });

  it('accepts x with 50px+ visible on screen', () => {
    expect(validateBounds({ ...validSaved, x: -100 }, [primaryDisplay])).not.toBeNull();
  });

  it('rejects y pushing window fully off-screen top', () => {
    expect(validateBounds({ ...validSaved, y: -1000, height: 800 }, [primaryDisplay])).toBeNull();
  });
});

describe('validateBounds — preserves isMaximized through validation', () => {
  it('returns isMaximized: true when valid', () => {
    expect(validateBounds({ ...validSaved, isMaximized: true }, [primaryDisplay])).toEqual(
      expect.objectContaining({ isMaximized: true })
    );
  });
});
