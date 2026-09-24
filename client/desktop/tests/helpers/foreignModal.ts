import { afterEach } from 'vitest';

// Modal dialogs opened by openForeignModal, removed after each test so a
// failed assertion cannot leak an open modal into the next one.
const opened: HTMLDialogElement[] = [];
afterEach(() => {
  opened.splice(0).forEach((dialog) => dialog.remove());
});

/**
 * A modal <dialog> opened in front of the component under test, the way the
 * MFA challenge opens over it (#3423). Keys typed into its input belong to it.
 */
export function openForeignModal(): { input: HTMLInputElement; dialog: HTMLDialogElement } {
  const dialog = document.createElement('dialog');
  const input = document.createElement('input');
  dialog.append(input);
  document.body.append(dialog);
  opened.push(dialog);
  dialog.showModal();
  input.focus();
  return { input, dialog };
}
