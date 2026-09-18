/**
 * Regression guard: the screen-share audio controls no longer sit on the video
 * permanently.
 *
 * They live inside `.voice-stage__cell-overlay` / `.voice-stage__overlay`,
 * which are ungated gradient bars — so a remote share carrying audio painted a
 * slider across the bottom of the picture for the entire call.
 *
 * jsdom applies no CSS, so no rendering test can see this either way. This
 * pins the declarations; whether it LOOKS right is a visual check.
 */
import { describe, it, expect } from 'vitest';
import { readCss, readSource, ruleBody } from '../../../helpers/cssRules';

const css = readCss('src/renderer/components/Voice/VoiceStage.css');

describe('screen-share audio controls are hover-gated', () => {
  it('finds the gate rule it is about (vacuity guard)', () => {
    expect(css.length).toBeGreaterThan(0);
    expect(ruleBody(css, '.voice-stage__cell-overlay .screenshare-audio-controls')).not.toBeNull();
  });

  it('hides them at rest without making the slider unfocusable', () => {
    const body = ruleBody(css, '.voice-stage__cell-overlay .screenshare-audio-controls') ?? '';
    expect(body).toMatch(/opacity:\s*0\s*;/);
    // display/visibility would take the slider out of the tab order, and
    // :focus-within is what reveals it for keyboard users.
    expect(body).not.toMatch(/display:\s*none/);
    expect(body).not.toMatch(/visibility:\s*hidden/);
  });

  it('scopes each reveal arm to the overlay it serves', () => {
    // Each arm names its own overlay. Unscoped, the stage arm swallowed the
    // cell one: `.voice-stage--equal` also carries `.voice-stage` and the cells
    // live inside it, so hovering anywhere on the stage revealed EVERY tuned-in
    // share's slider and the cell arm could never be the rule that fired.
    for (const sel of [
      /\.voice-stage__cell:hover \.voice-stage__cell-overlay \.screenshare-audio-controls/,
      /\.voice-stage__cell:focus-within \.voice-stage__cell-overlay \.screenshare-audio-controls/,
      /\.voice-stage:hover \.voice-stage__overlay \.screenshare-audio-controls/,
      /\.voice-stage:focus-within \.voice-stage__overlay \.screenshare-audio-controls/,
    ]) {
      expect(css).toMatch(sel);
    }
  });

  it('keeps no unscoped stage arm that would re-widen the grid reveal', () => {
    // The specific regression: a `.voice-stage:hover` immediately followed by
    // the control class, with no overlay between them, matches cell overlays
    // too. Deleting the stage arms outright is NOT the alternative -- see below.
    expect(css).not.toMatch(/\.voice-stage:hover \.screenshare-audio-controls/);
    expect(css).not.toMatch(/\.voice-stage:focus-within \.screenshare-audio-controls/);
  });

  it('still reveals in the spotlight layout, which has no cell ancestor', () => {
    // The reason the stage arms are scoped rather than removed. `.voice-stage__overlay`
    // (VoiceStage.tsx) hangs directly off `.voice-stage`; only
    // `.voice-stage__cell-overlay` sits inside a `.voice-stage__cell`. Drop the
    // stage arms and the spotlight share loses its only trigger.
    const tsx = readSource('src/renderer/components/Voice/VoiceStage.tsx');
    expect(tsx).toMatch(/<div className="voice-stage__overlay">/);
    expect(css).toMatch(/\.voice-stage:hover \.voice-stage__overlay/);
  });

  it('drops the reveal transition under reduced motion', () => {
    // Reachable only since the rule reader learned to descend into at-rules.
    const body = ruleBody(css, '.voice-stage__cell-overlay .screenshare-audio-controls') ?? '';
    expect(body).toMatch(/transition:\s*none/);
  });
});
