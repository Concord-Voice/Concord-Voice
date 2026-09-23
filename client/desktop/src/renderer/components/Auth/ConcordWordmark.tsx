import React from 'react';
import './ConcordWordmark.css';

/**
 * The CONCORD wordmark for the auth screens, in the variant the active theme can read.
 *
 * The original asset draws its letterforms in `#ffffff`, which reads on a dark background
 * and vanishes on a light one — 1.00:1 against Defacto light and High Contrast light, and
 * no better than 1.20:1 against any of the 17 light blocks. An SVG loaded through an image
 * element cannot be recoloured by page CSS, so a light-theme variant ships alongside it, following the
 * light/dark asset-pair convention the KLIPY logo already uses (`klipyProvider.ts`).
 *
 * Both variants are rendered and CSS hides the wrong one, keyed on `data-theme` — rather
 * than choosing `src` in JS. That attribute is written before React's first render
 * (`main.tsx`) and kept current by the settings store, including a `prefers-color-scheme`
 * listener when the theme is "System" (`settingsStore.ts`). Keying on it inherits all
 * three for free, where a JS choice would have to rebuild them — and the two JS copies
 * that already exist (`GifPicker.tsx`, `UserPopover.tsx`) read `matchMedia` once and go
 * stale if the OS flips while they are mounted.
 *
 * Both images carry the real alt text on purpose: the hidden one is `display: none`, which
 * removes it from the accessibility tree, so exactly one is announced in either theme. An
 * empty alt on the light variant would leave light-theme users with an unlabelled logo.
 */
const DARK_THEME_SRC = './branding/Concord-Voice/logos/main-logo-transparent-vector.svg';
const LIGHT_THEME_SRC = './branding/Concord-Voice/logos/main-logo-transparent-vector-light.svg';

interface ConcordWordmarkProps {
  /** The call site's sizing class (`login-logo`, `register-logo`, `connection-logo`). */
  className: string;
}

const ConcordWordmark: React.FC<ConcordWordmarkProps> = ({ className }) => (
  <>
    <img
      src={DARK_THEME_SRC}
      alt="Concord Voice"
      className={`${className} concord-wordmark concord-wordmark--dark-theme`}
    />
    <img
      src={LIGHT_THEME_SRC}
      alt="Concord Voice"
      className={`${className} concord-wordmark concord-wordmark--light-theme`}
    />
  </>
);

export default ConcordWordmark;
