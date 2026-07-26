import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import path from 'node:path';

const NSH = readFileSync(path.resolve(__dirname, '../../../build/installer.nsh'), 'utf8');

/**
 * The script stripped of NSIS comments (`;` / `#` line comments), i.e. only the
 * directives makensis actually executes. Used by the negative assertions below so
 * they constrain behavior rather than forbidding the file from documenting itself.
 */
const DIRECTIVES = NSH.split('\n')
  .filter((line) => !/^\s*[;#]/.test(line))
  .join('\n');

describe('installer.nsh (#2402)', () => {
  it('defines the customInit macro Forge/NSIS looks for', () => {
    // installer.nsi:79-80 guards on !ifmacrodef customInit — a differently
    // named macro is silently ignored and the install path reverts to the
    // sanitizedName default.
    expect(NSH).toMatch(/!macro\s+customInit/);
    expect(NSH).toMatch(/!macroend/);
  });

  it('pins $INSTDIR to LOCALAPPDATA\\ConcordVoice\\app', () => {
    expect(NSH).toMatch(/StrCpy\s+\$INSTDIR\s+"\$LOCALAPPDATA\\ConcordVoice\\app"/);
  });

  it('does not point INSTDIR at the cache or the bare identity root', () => {
    // ConcordVoice\pending holds the in-flight installer and current.blockmap;
    // the bare ConcordVoice root is what Squirrel wiped, taking the cache with it.
    expect(NSH).not.toMatch(/\$INSTDIR\s+"\$LOCALAPPDATA\\ConcordVoice"/);
    // Asserted against DIRECTIVES, not the whole file: the header comment names
    // ConcordVoice\pending as the cache this layout protects, and that rationale
    // is the point of the file. What must never happen is a *directive* aiming
    // the installer at it.
    expect(DIRECTIVES).not.toMatch(/pending/i);
  });
});
