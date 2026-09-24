/**
 * True when a key event belongs to an open modal `<dialog>` that does not
 * contain `ownRoot`. That dialog sits in the top layer in front of the caller,
 * so the key is its own: a page-wide listener that handles it anyway steals
 * Escape's close request or Tab's focus move from the dialog in front (the MFA
 * challenge over a dock overlay, a popover, or the screen-share picker; #3423).
 *
 * A key typed inside a modal dialog belongs to that dialog. A key from outside
 * every dialog (focus on <body>, which is where Chromium drops it when the
 * focused control is disabled mid-request) belongs to whichever modal dialog is
 * open, so it is foreign while any open modal dialog does not hold the caller.
 * Limit: with the caller inside the top modal and an unrelated modal below it,
 * a <body> key reads as foreign when it need not. The MFA challenge always
 * retakes the top, so the answer is exact for it.
 */
export function keyTargetsForeignModal(event: KeyboardEvent, ownRoot: Element | null): boolean {
  const holdsCaller = (dialog: Element) => !!ownRoot && dialog.contains(ownRoot);
  const dialog = event.target instanceof Element ? event.target.closest('dialog') : null;
  if (dialog?.matches(':modal')) return !holdsCaller(dialog);
  return Array.from(document.querySelectorAll('dialog')).some(
    (d) => d.matches(':modal') && !holdsCaller(d)
  );
}
