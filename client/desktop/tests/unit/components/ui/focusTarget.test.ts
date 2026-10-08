import { focusTargetIn } from '@/renderer/components/ui/focusTarget';

// jsdom has no layout, so `checkVisibility` does not exist there. The "not
// rendered" cases give an element and its subtree the method, answering the way
// a browser does under `display: none`. "Mutation:" comments name the
// production change each case turns red.
function hideFromLayout(el: HTMLElement): void {
  for (const node of [el, ...el.querySelectorAll('*')]) {
    Object.defineProperty(node, 'checkVisibility', { configurable: true, value: () => false });
  }
}

let host: HTMLElement;

/** The element with `id` in the mounted fixture. */
function byId(id: string): HTMLElement {
  const el = host.querySelector<HTMLElement>(`#${id}`);
  if (el === null) throw new Error(`fixture has no #${id}`);
  return el;
}

/** Mounts `html` under `host` and returns the element with `id`. */
function mount(html: string, id: string): HTMLElement {
  host.innerHTML = html;
  return byId(id);
}

describe('focusTargetIn', () => {
  beforeEach(() => {
    host = document.createElement('div');
    document.body.appendChild(host);
  });

  afterEach(() => {
    host.remove();
  });

  it('is null for no region and for one that left the document', () => {
    expect(focusTargetIn(null)).toBeNull();
    const detached = document.createElement('div');
    detached.tabIndex = 0;
    expect(focusTargetIn(detached)).toBeNull();
  });

  it('is the region itself when it takes focus', () => {
    const region = mount('<ul id="r" tabindex="0"><li><button>Row</button></li></ul>', 'r');
    expect(focusTargetIn(region)).toBe(region);
  });

  it('is the first focusable descendant when the region does not take focus', () => {
    const region = mount(
      '<ul id="r"><li><button id="first">A</button><button>B</button></li></ul>',
      'r'
    );
    expect(focusTargetIn(region)).toBe(byId('first'));
  });

  it('is null when nothing in the region is focusable', () => {
    expect(focusTargetIn(mount('<p id="r">Nothing to focus</p>', 'r'))).toBeNull();
  });

  // Mutation: returning `region` on `tabIndex >= 0` alone names a disabled control, whose `.focus()` is a silent no-op (red).
  it('skips a region that is disabled', () => {
    const region = mount('<button id="r" disabled>Delete</button>', 'r');
    expect(focusTargetIn(region)).toBeNull();
  });

  // Mutation: filtering descendants by `[disabled]` alone (getFocusable) misses a control disabled through its fieldset (red).
  it('skips a descendant disabled through its fieldset', () => {
    const region = mount(
      '<div id="r"><fieldset disabled><button>Off</button></fieldset><button id="on">On</button></div>',
      'r'
    );
    expect(focusTargetIn(region)).toBe(byId('on'));
  });

  // Mutation: dropping the `closest('[inert]')` test returns a region the browser will not focus (red).
  it('skips a region inside an inert subtree', () => {
    const region = mount(
      '<div inert><ul id="r" tabindex="0"><li><button>Row</button></li></ul></div>',
      'r'
    );
    expect(focusTargetIn(region)).toBeNull();
  });

  // Mutation: applying the inert test to the region only lets an inert descendant through (red).
  it('skips an inert descendant and takes the next one', () => {
    const region = mount(
      '<div id="r"><div inert><button>Off</button></div><button id="on">On</button></div>',
      'r'
    );
    expect(focusTargetIn(region)).toBe(byId('on'));
  });

  // Mutation: dropping the `checkVisibility` test names a region with no layout box (red).
  it('skips a region that is not rendered', () => {
    const region = mount('<ul id="r" tabindex="0"><li><button>Row</button></li></ul>', 'r');
    hideFromLayout(region);
    expect(focusTargetIn(region)).toBeNull();
  });

  // Mutation: choosing `getFocusable(region)[0]` without the `canTakeFocus` filter names a hidden first descendant (red).
  it('skips a descendant that is not rendered and takes the next one', () => {
    const region = mount(
      '<div id="r"><button id="hidden">Hidden</button><button id="shown">Shown</button></div>',
      'r'
    );
    hideFromLayout(byId('hidden'));
    expect(focusTargetIn(region)).toBe(byId('shown'));
  });
});
