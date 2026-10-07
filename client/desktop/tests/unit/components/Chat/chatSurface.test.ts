import {
  findSurfaceComposer,
  findSurfaceMessageRow,
  focusSurfaceComposer,
} from '@/renderer/components/Chat/chatSurface';

function makeSurface(id: string): HTMLElement {
  const el = document.createElement('div');
  el.dataset.chatSurface = id;
  document.body.appendChild(el);
  return el;
}

function makeComposer(): HTMLTextAreaElement {
  const el = document.createElement('textarea');
  el.className = 'message-input-textarea';
  return el;
}

function makeRow(messageId: string): HTMLElement {
  const el = document.createElement('div');
  el.dataset.messageId = messageId;
  el.tabIndex = -1;
  return el;
}

describe('chatSurface DOM helpers', () => {
  afterEach(() => {
    document.body.replaceChildren();
  });

  describe('findSurfaceComposer', () => {
    it('returns each sibling surface its own composer', () => {
      const a = makeSurface('surface-a');
      const b = makeSurface('surface-b');
      const composerA = makeComposer();
      const composerB = makeComposer();
      a.appendChild(composerA);
      b.appendChild(composerB);

      expect(findSurfaceComposer('surface-a')).toBe(composerA);
      expect(findSurfaceComposer('surface-b')).toBe(composerB);
    });

    it('returns null for an unknown surface id', () => {
      const a = makeSurface('surface-a');
      const composerA = makeComposer();
      a.appendChild(composerA);

      // Positive gate: the known surface resolves, so a null below is about the id.
      expect(findSurfaceComposer('surface-a')).toBe(composerA);
      expect(findSurfaceComposer('no-such-surface')).toBeNull();
    });

    it('returns null for a surface with no composer even when another surface has one', () => {
      const withComposer = makeSurface('has-composer');
      const without = makeSurface('no-composer');
      const composer = makeComposer();
      withComposer.appendChild(composer);
      without.appendChild(document.createElement('div'));

      expect(findSurfaceComposer('has-composer')).toBe(composer);
      expect(findSurfaceComposer('no-composer')).toBeNull();
    });

    it('skips a composer that belongs to a surface nested inside', () => {
      const outer = makeSurface('outer');
      const inner = document.createElement('div');
      inner.dataset.chatSurface = 'inner';
      const innerComposer = makeComposer();
      const outerComposer = makeComposer();
      // The inner composer comes first in document order, the outer's own composer after it.
      inner.appendChild(innerComposer);
      outer.appendChild(inner);
      outer.appendChild(outerComposer);

      // Positive gate: a plain query returns the inner composer, so the fixture exercises the skip.
      expect(outer.querySelector('.message-input-textarea')).toBe(innerComposer);

      expect(findSurfaceComposer('outer')).toBe(outerComposer);
      expect(findSurfaceComposer('inner')).toBe(innerComposer);
    });

    it('returns null for an outer surface whose only composer belongs to a nested surface', () => {
      const outer = makeSurface('outer');
      const inner = document.createElement('div');
      inner.dataset.chatSurface = 'inner';
      const innerComposer = makeComposer();
      inner.appendChild(innerComposer);
      outer.appendChild(inner);

      expect(findSurfaceComposer('inner')).toBe(innerComposer);
      expect(findSurfaceComposer('outer')).toBeNull();
    });
  });

  describe('findSurfaceMessageRow', () => {
    it('finds a row by id within its surface', () => {
      const a = makeSurface('surface-a');
      const rowOne = makeRow('m-1');
      const rowTwo = makeRow('m-2');
      a.appendChild(rowOne);
      a.appendChild(rowTwo);

      expect(findSurfaceMessageRow('surface-a', 'm-2')).toBe(rowTwo);
      expect(findSurfaceMessageRow('surface-a', 'm-1')).toBe(rowOne);
      expect(findSurfaceMessageRow('surface-a', 'm-missing')).toBeNull();
      expect(findSurfaceMessageRow('no-such-surface', 'm-1')).toBeNull();
    });

    it('resolves the same message id to the row in the asked-for surface', () => {
      const a = makeSurface('surface-a');
      const b = makeSurface('surface-b');
      const rowInA = makeRow('shared');
      const rowInB = makeRow('shared');
      a.appendChild(rowInA);
      b.appendChild(rowInB);

      expect(findSurfaceMessageRow('surface-a', 'shared')).toBe(rowInA);
      expect(findSurfaceMessageRow('surface-b', 'shared')).toBe(rowInB);
    });

    it('skips a same-id row that belongs to a nested surface', () => {
      const outer = makeSurface('outer');
      const inner = document.createElement('div');
      inner.dataset.chatSurface = 'inner';
      const innerRow = makeRow('shared');
      const outerRow = makeRow('shared');
      // The nested row comes first in document order.
      inner.appendChild(innerRow);
      outer.appendChild(inner);
      outer.appendChild(outerRow);

      // Positive gate: a plain query returns the nested row, so the fixture exercises the skip.
      expect(outer.querySelector('[data-message-id="shared"]')).toBe(innerRow);

      expect(findSurfaceMessageRow('outer', 'shared')).toBe(outerRow);
      expect(findSurfaceMessageRow('inner', 'shared')).toBe(innerRow);
    });

    it('returns null for an outer surface whose only matching row is nested', () => {
      const outer = makeSurface('outer');
      const inner = document.createElement('div');
      inner.dataset.chatSurface = 'inner';
      const innerRow = makeRow('only-inner');
      inner.appendChild(innerRow);
      outer.appendChild(inner);

      expect(findSurfaceMessageRow('inner', 'only-inner')).toBe(innerRow);
      expect(findSurfaceMessageRow('outer', 'only-inner')).toBeNull();
    });

    it('finds a message id containing a quote and a bracket without throwing', () => {
      const a = makeSurface('surface-a');
      const awkwardId = 'a"]b';
      const row = makeRow(awkwardId);
      a.appendChild(row);
      a.appendChild(makeRow('plain'));

      expect(() => findSurfaceMessageRow('surface-a', awkwardId)).not.toThrow();
      expect(findSurfaceMessageRow('surface-a', awkwardId)).toBe(row);
    });

    it('does not throw for a surface id containing selector syntax', () => {
      const awkwardSurface = 'x"] .y[';
      const surface = makeSurface(awkwardSurface);
      const row = makeRow('m-1');
      surface.appendChild(row);

      expect(findSurfaceMessageRow(awkwardSurface, 'm-1')).toBe(row);
      expect(findSurfaceComposer(awkwardSurface)).toBeNull();
    });
  });

  describe('focusSurfaceComposer', () => {
    it('moves focus to the composer of the named surface', () => {
      const a = makeSurface('surface-a');
      const b = makeSurface('surface-b');
      const composerA = makeComposer();
      const composerB = makeComposer();
      a.appendChild(composerA);
      b.appendChild(composerB);
      const outside = document.createElement('button');
      document.body.appendChild(outside);
      outside.focus();
      // Positive gate: focus really sits on the third element before the call.
      expect(document.activeElement).toBe(outside);

      focusSurfaceComposer('surface-b');

      expect(document.activeElement).toBe(composerB);

      focusSurfaceComposer('surface-a');

      expect(document.activeElement).toBe(composerA);
    });

    it('leaves focus alone for a surface with no composer, never landing on another surface', () => {
      const withComposer = makeSurface('has-composer');
      const without = makeSurface('no-composer');
      withComposer.appendChild(makeComposer());
      without.appendChild(document.createElement('div'));
      const outside = document.createElement('button');
      document.body.appendChild(outside);
      outside.focus();
      expect(document.activeElement).toBe(outside);

      focusSurfaceComposer('no-composer');

      expect(document.activeElement).toBe(outside);
    });

    it('leaves focus alone for an unknown surface id', () => {
      const a = makeSurface('surface-a');
      a.appendChild(makeComposer());
      const outside = document.createElement('button');
      document.body.appendChild(outside);
      outside.focus();
      expect(document.activeElement).toBe(outside);

      focusSurfaceComposer('no-such-surface');

      expect(document.activeElement).toBe(outside);
    });

    it('focuses the outer composer, not a nested surface composer that comes first', () => {
      const outer = makeSurface('outer');
      const inner = document.createElement('div');
      inner.dataset.chatSurface = 'inner';
      const innerComposer = makeComposer();
      const outerComposer = makeComposer();
      inner.appendChild(innerComposer);
      outer.appendChild(inner);
      outer.appendChild(outerComposer);

      focusSurfaceComposer('outer');

      expect(document.activeElement).toBe(outerComposer);
    });
  });
});
