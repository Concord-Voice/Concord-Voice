import { afterEach, describe, expect, it } from 'vitest';
import { keyTargetsForeignModal } from '@/renderer/utils/ui/keyTargetsForeignModal';

function keyFrom(target: EventTarget): KeyboardEvent {
  const event = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true });
  Object.defineProperty(event, 'target', { value: target });
  return event;
}

function dialogWithInput(modal: boolean): { dialog: HTMLDialogElement; input: HTMLInputElement } {
  const dialog = document.createElement('dialog');
  const input = document.createElement('input');
  dialog.append(input);
  document.body.append(dialog);
  if (modal) dialog.showModal();
  else dialog.setAttribute('open', '');
  return { dialog, input };
}

describe('keyTargetsForeignModal', () => {
  afterEach(() => {
    document.body.replaceChildren();
  });

  it('is true for a key typed into a modal dialog that does not hold the caller', () => {
    const own = document.createElement('div');
    document.body.append(own);
    const { input } = dialogWithInput(true);
    expect(keyTargetsForeignModal(keyFrom(input), own)).toBe(true);
  });

  it('is true when the caller has no root yet', () => {
    const { input } = dialogWithInput(true);
    expect(keyTargetsForeignModal(keyFrom(input), null)).toBe(true);
  });

  it('is false inside the modal dialog that holds the caller', () => {
    const { dialog, input } = dialogWithInput(true);
    const own = document.createElement('div');
    dialog.append(own);
    expect(keyTargetsForeignModal(keyFrom(input), own)).toBe(false);
  });

  it('is false for a non-modal dialog, which is not in front of anything', () => {
    const own = document.createElement('div');
    document.body.append(own);
    const { input } = dialogWithInput(false);
    expect(keyTargetsForeignModal(keyFrom(input), own)).toBe(false);
  });

  // A proof in flight disables the focused digit, and Chromium drops focus to
  // <body>: the key then comes from outside every dialog (#3423 red-team).
  it('is true for a key from <body> while a modal dialog that does not hold the caller is open', () => {
    const own = document.createElement('div');
    document.body.append(own);
    dialogWithInput(true);
    expect(keyTargetsForeignModal(keyFrom(document.body), own)).toBe(true);
    expect(keyTargetsForeignModal(keyFrom(document), own)).toBe(true);
  });

  it('is false for a key from <body> when the only modal dialog holds the caller', () => {
    const { dialog } = dialogWithInput(true);
    const own = document.createElement('div');
    dialog.append(own);
    expect(keyTargetsForeignModal(keyFrom(document.body), own)).toBe(false);
  });

  it('is false for a key outside any dialog, or a non-element target', () => {
    const own = document.createElement('div');
    const outside = document.createElement('input');
    document.body.append(own, outside);
    expect(keyTargetsForeignModal(keyFrom(outside), own)).toBe(false);
    expect(keyTargetsForeignModal(keyFrom(document), own)).toBe(false);
  });
});
