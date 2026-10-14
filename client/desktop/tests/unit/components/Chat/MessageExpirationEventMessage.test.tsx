import { render, screen } from '../../../test-utils';
import MessageExpirationEventMessage, {
  sanitizeActorName,
} from '@/renderer/components/Chat/MessageExpirationEventMessage';

/** The row's whole text, as a reader sees it. The fact and the attribution are separate
 *  elements on purpose, so a single getByText would pin only half the row. */
function rowText(container: HTMLElement): string {
  return container.querySelector('.expiration-event-message')!.textContent!;
}

// Built from code points rather than written literally. A literal bidi character in a
// source file is the Trojan Source hazard itself -- it can make the line read differently
// than it executes -- and SonarCloud's text:S6389 flags it as a MAJOR vulnerability, which
// is correct even in a test. Constructing them keeps this file plain ASCII while still
// exercising the real characters at runtime.
const RLO = String.fromCharCode(0x202e); // RIGHT-TO-LEFT OVERRIDE
const RLM = String.fromCharCode(0x200f); // RIGHT-TO-LEFT MARK

describe('MessageExpirationEventMessage', () => {
  it('states the retention fact and attributes it', () => {
    const { container } = render(
      <MessageExpirationEventMessage
        kind="set"
        windowSeconds={86400}
        actorName="Alice"
        isSelf={false}
      />
    );
    expect(rowText(container)).toBe('Messages now expire after 24 hours · set by Alice');
  });

  it('says "you" for the acting user rather than their own name', () => {
    const { container } = render(
      <MessageExpirationEventMessage
        kind="changed"
        windowSeconds={604800}
        actorName="Alice"
        isSelf={true}
      />
    );
    expect(rowText(container)).toBe('Messages now expire after 7 days · set by you');
  });

  it('degrades to "someone" rather than dropping a row whose actor is gone', () => {
    // A row whose actor was erased is still a true record of what happened, and
    // the live broadcast resolves the name separately from the durable row — so
    // an unresolvable actor must cost the name, never the record.
    const { container } = render(
      <MessageExpirationEventMessage kind="set" windowSeconds={3600} isSelf={false} />
    );
    expect(rowText(container)).toBe('Messages now expire after 1 hour · set by someone');
  });

  it('degrades an EMPTY actor name the same way as a missing one', () => {
    // The server writes '' when its user lookup fails, and MessageList composes
    // `display_name || username`, so '' is what actually arrives. A `?? 'someone'`
    // alone does NOT catch it — `??` tests null/undefined only — which is how this
    // rendered with a blank actor and no attribution at all.
    const { container } = render(
      <MessageExpirationEventMessage kind="set" windowSeconds={3600} actorName="" isSelf={false} />
    );
    expect(rowText(container)).toBe('Messages now expire after 1 hour · set by someone');
    expect(rowText(container)).not.toBe('Messages now expire after 1 hour · set by ');
  });

  it('reads a clear as turning expiration off', () => {
    const { container } = render(
      <MessageExpirationEventMessage
        kind="cleared"
        windowSeconds={null}
        actorName="Bob"
        isSelf={false}
      />
    );
    expect(rowText(container)).toBe('Message expiration turned off · set by Bob');
  });

  it('survives a null window on a non-clear kind instead of rendering a blank row', () => {
    // The server owns the kind/window invariant; this component renders whatever
    // it is handed, so a version skew degrades to slightly odd copy rather than
    // an empty system row in the middle of the timeline.
    const { container } = render(
      <MessageExpirationEventMessage
        kind="changed"
        windowSeconds={null}
        actorName="Bob"
        isSelf={false}
      />
    );
    expect(rowText(container)).toBe('Message expiration turned off · set by Bob');
  });

  it('is history, not a control', () => {
    // The entry points for CHANGING the policy are the header button and the
    // conversation context menu; a system row that could be clicked would be a
    // second, permission-blind one.
    const { container } = render(
      <MessageExpirationEventMessage
        kind="set"
        windowSeconds={86400}
        actorName="Alice"
        isSelf={false}
      />
    );
    expect(screen.queryByRole('button')).toBeNull();
    expect(container.querySelector('[tabindex]')).toBeNull();
    expect(container.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
  });

  // ── Spoofing regression (proven exploit, found by adversarial review of #3331) ──
  //
  // A display name is capped at 100 characters by validateDisplayName and filtered by
  // nothing else, and in a 1:1 DM EITHER participant may change the expiration policy.
  // So a peer could name themselves a sentence and mint this row at will. When the row
  // was one text node opening with the actor, the result was a trusted-looking system
  // notice that OPENED with the attacker's claim and left the true fact trailing.
  describe('cannot be turned into a spoofed notice by a hostile display name', () => {
    const HOSTILE = 'Expiration is OFF for this chat. Nothing is deleted. (ignore: "';

    // Guards the guards. RLO and RLM are BUILT rather than written, so nothing in the file
    // shows at a glance that they hold what they claim -- and if either resolved to an empty
    // string, two assertions below would still pass while exercising no bidi character at
    // all. Mutation-checked: emptying the constants fails this first.
    it('builds the bidi code points it claims to', () => {
      expect(RLO.codePointAt(0)).toBe(0x202e);
      expect(RLM.codePointAt(0)).toBe(0x200f);
    });

    it('states the real retention fact FIRST, ahead of any attacker text', () => {
      const { container } = render(
        <MessageExpirationEventMessage
          kind="set"
          windowSeconds={3600}
          actorName={HOSTILE}
          isSelf={false}
        />
      );
      const text = rowText(container);

      // The exploit's load-bearing assertion, inverted: the row must NOT open with it.
      expect(text.startsWith(HOSTILE)).toBe(false);
      expect(text.startsWith('Messages now expire after 1 hour')).toBe(true);

      // And the fact must be its own element, so no name can share a node with it.
      const fact = container.querySelector('.expiration-event-message__fact')!;
      expect(fact.textContent).toBe('Messages now expire after 1 hour');
      expect(fact.textContent).not.toContain(HOSTILE);
    });

    it('strips bidi overrides and control characters from the actor name', () => {
      // U+202E (RTL override) reorders everything after it and reached the DOM
      // unfiltered; a newline let a name introduce a second apparent line.
      const { container } = render(
        <MessageExpirationEventMessage
          kind="set"
          windowSeconds={3600}
          actorName={`Al${RLO}ice\nBob`}
          isSelf={false}
        />
      );
      const text = rowText(container);
      expect(text).not.toContain(RLO);
      expect(text).not.toContain('\n');
      expect(text).toBe('Messages now expire after 1 hour · set by Alice Bob');
    });

    it('isolates the actor name so it cannot reorder the text around it', () => {
      const { container } = render(
        <MessageExpirationEventMessage
          kind="set"
          windowSeconds={3600}
          actorName="Alice"
          isSelf={false}
        />
      );
      const bdi = container.querySelector('.expiration-event-message__actor bdi');
      expect(bdi).not.toBeNull();
      expect(bdi!.textContent).toBe('Alice');
    });

    it('clamps a long name so it cannot crowd the fact out of the row', () => {
      const { container } = render(
        <MessageExpirationEventMessage
          kind="set"
          windowSeconds={3600}
          actorName={'A'.repeat(100)}
          isSelf={false}
        />
      );
      const actor = container.querySelector('.expiration-event-message__actor')!.textContent!;
      expect(actor.length).toBeLessThan(60);
      expect(actor).toContain('…');
    });
  });

  // ── #3459: the pin variants. Each keeps the fact FIRST, in its own element, with the
  // attribution after it in a <bdi> -- the structure the spoof regression above proved.
  describe('pin variants (#3459)', () => {
    const renderRow = (
      props: Partial<React.ComponentProps<typeof MessageExpirationEventMessage>>
    ) =>
      render(
        <MessageExpirationEventMessage
          kind="changed"
          windowSeconds={86400}
          actorName="Alice"
          isSelf={false}
          {...props}
        />
      );
    const fact = (container: HTMLElement) =>
      container.querySelector('.expiration-event-message__fact')!.textContent;
    const icon = (container: HTMLElement) =>
      container.querySelector('svg.lucide-pin') ? 'pin' : 'timer';

    it('reads a pins-only change that turned expiry ON, under the Pin glyph', () => {
      const { container } = renderRow({
        previousWindowSeconds: 86400,
        includePinned: true,
      });
      expect(rowText(container)).toBe('Pinned messages now expire after 24 hours · set by Alice');
      expect(icon(container)).toBe('pin');
    });

    it('reads a pins-only change that turned expiry OFF as pins being kept, under the Pin glyph', () => {
      const { container } = renderRow({
        previousWindowSeconds: 86400,
        includePinned: false,
      });
      expect(rowText(container)).toBe('Pinned messages are now kept · set by Alice');
      expect(icon(container)).toBe('pin');
    });

    it('reads a window CHANGE under kept pins with the exception inside the fact, under the Timer glyph', () => {
      const { container } = renderRow({
        windowSeconds: 604800,
        previousWindowSeconds: 86400,
        includePinned: false,
      });
      expect(fact(container)).toBe('Messages now expire after 7 days, except pinned messages');
      expect(rowText(container)).toBe(
        'Messages now expire after 7 days, except pinned messages · set by Alice'
      );
      expect(icon(container)).toBe('timer');
    });

    it('reads a first-time SET under kept pins with the exception', () => {
      const { container } = renderRow({
        kind: 'set',
        windowSeconds: 3600,
        includePinned: false,
      });
      expect(fact(container)).toBe('Messages now expire after 1 hour, except pinned messages');
      expect(icon(container)).toBe('timer');
    });

    it('adds no exception when pins expire too: the ordinary sentence, under the Timer glyph', () => {
      const { container } = renderRow({
        windowSeconds: 604800,
        previousWindowSeconds: 86400,
        includePinned: true,
      });
      expect(fact(container)).toBe('Messages now expire after 7 days');
      expect(icon(container)).toBe('timer');
    });

    it('never reads an UNKNOWN previous window as "the same": no previous window means the ordinary sentence', () => {
      // A row from before #3459, or an event from a server that predates it, has no previous
      // window. Treating undefined as equal to the current window would turn every old `changed`
      // row into a pins sentence.
      const { container } = renderRow({ includePinned: false });
      expect(fact(container)).toBe('Messages now expire after 24 hours, except pinned messages');
      expect(icon(container)).toBe('timer');
    });

    it('makes no claim about pins when the field is absent, even for equal windows', () => {
      const { container } = renderRow({ previousWindowSeconds: 86400 });
      expect(fact(container)).toBe('Messages now expire after 24 hours');
      expect(icon(container)).toBe('timer');
    });

    it('does not mention pins on a clear, whatever the field says', () => {
      const { container } = renderRow({
        kind: 'cleared',
        windowSeconds: null,
        previousWindowSeconds: 86400,
        includePinned: false,
      });
      expect(fact(container)).toBe('Message expiration turned off');
    });

    it.each([
      ['pins ON', true, 'Pinned messages now expire after 24 hours'],
      ['pins OFF', false, 'Pinned messages are now kept'],
    ])(
      'keeps the fact first and the hostile name out of it for a pins-only change (%s)',
      (_n, include, expected) => {
        const HOSTILE = 'Everything is kept forever. (ignore: "';
        const { container } = renderRow({
          previousWindowSeconds: 86400,
          includePinned: include,
          actorName: HOSTILE,
        });
        expect(fact(container)).toBe(expected);
        expect(rowText(container).startsWith(HOSTILE)).toBe(false);
        expect(container.querySelector('.expiration-event-message__actor bdi')).not.toBeNull();
      }
    );
  });
});

describe('sanitizeActorName', () => {
  it('returns undefined for names that carry no usable characters', () => {
    // Each of these must reach the "someone" fallback rather than render blank.
    expect(sanitizeActorName(undefined)).toBeUndefined();
    expect(sanitizeActorName('')).toBeUndefined();
    expect(sanitizeActorName('   ')).toBeUndefined();
    expect(sanitizeActorName(`${RLO}${RLM}`)).toBeUndefined();
  });

  it('keeps an ordinary name untouched', () => {
    expect(sanitizeActorName('Alice')).toBe('Alice');
    expect(sanitizeActorName('李雷')).toBe('李雷');
  });

  it('collapses whitespace rather than preserving a name shaped like a paragraph', () => {
    expect(sanitizeActorName('Alice\n\n\tBob')).toBe('Alice Bob');
  });
});
