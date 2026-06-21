/**
 * Color Utilities — pure HSL math for custom theme derivation.
 * Zero external dependencies.
 */

// ─── Types ───────────────────────────────────────────────────────────────────

export interface HSL {
  h: number; // 0–360
  s: number; // 0–100
  l: number; // 0–100
}

export interface CustomColors {
  background: string; // hex e.g. "#0d0821"
  accentPrimary: string; // hex e.g. "#fa709a"
  accentSecondary: string; // hex e.g. "#ffe13f"
}

export interface DerivedThemeVariables {
  '--bg-primary': string;
  '--bg-secondary': string;
  '--bg-tertiary': string;
  '--bg-hover': string;
  '--bg-active': string;
  '--text-primary': string;
  '--text-secondary': string;
  '--text-muted': string;
  '--text-tertiary': string;
  '--accent-primary': string;
  '--accent-secondary': string;
  '--accent-hover': string;
  '--accent-color': string;
  '--gradient-brand': string;
  '--border-color': string;
  '--on-accent': string;
  '--success': string;
  '--danger': string;
  '--danger-hover': string;
  '--error-color': string;
  '--success-color': string;
  '--status-connected': string;
  '--status-connecting': string;
  '--status-disconnected': string;
}

// All CSS property keys that we derive (used by clear function)
const THEME_VARIABLE_KEYS: (keyof DerivedThemeVariables)[] = [
  '--bg-primary',
  '--bg-secondary',
  '--bg-tertiary',
  '--bg-hover',
  '--bg-active',
  '--text-primary',
  '--text-secondary',
  '--text-muted',
  '--text-tertiary',
  '--accent-primary',
  '--accent-secondary',
  '--accent-hover',
  '--accent-color',
  '--gradient-brand',
  '--border-color',
  '--on-accent',
  '--success',
  '--danger',
  '--danger-hover',
  '--error-color',
  '--success-color',
  '--status-connected',
  '--status-connecting',
  '--status-disconnected',
];

// ─── Hex ↔ HSL Conversion ────────────────────────────────────────────────────

export function hexToHsl(hex: string): HSL {
  const h = hex.replace('#', '');
  const r = Number.parseInt(h.substring(0, 2), 16) / 255;
  const g = Number.parseInt(h.substring(2, 4), 16) / 255;
  const b = Number.parseInt(h.substring(4, 6), 16) / 255;

  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const delta = max - min;

  let hue = 0;
  let sat = 0;
  const lit = (max + min) / 2;

  if (delta !== 0) {
    sat = lit > 0.5 ? delta / (2 - max - min) : delta / (max + min);

    if (max === r) {
      hue = ((g - b) / delta + (g < b ? 6 : 0)) * 60;
    } else if (max === g) {
      hue = ((b - r) / delta + 2) * 60;
    } else {
      hue = ((r - g) / delta + 4) * 60;
    }
  }

  return {
    h: Math.round(hue),
    s: Math.round(sat * 100),
    l: Math.round(lit * 100),
  };
}

export function hslToHex(hsl: HSL): string {
  const h = hsl.h;
  const s = Math.max(0, Math.min(100, hsl.s)) / 100;
  const l = Math.max(0, Math.min(100, hsl.l)) / 100;

  const c = (1 - Math.abs(2 * l - 1)) * s;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = l - c / 2;

  let r1 = 0,
    g1 = 0,
    b1 = 0;

  if (h >= 0 && h < 60) {
    r1 = c;
    g1 = x;
  } else if (h >= 60 && h < 120) {
    r1 = x;
    g1 = c;
  } else if (h >= 120 && h < 180) {
    g1 = c;
    b1 = x;
  } else if (h >= 180 && h < 240) {
    g1 = x;
    b1 = c;
  } else if (h >= 240 && h < 300) {
    r1 = x;
    b1 = c;
  } else {
    r1 = c;
    b1 = x;
  }

  const toHex = (n: number) => {
    const val = Math.round((n + m) * 255);
    return Math.max(0, Math.min(255, val)).toString(16).padStart(2, '0');
  };

  return `#${toHex(r1)}${toHex(g1)}${toHex(b1)}`;
}

// ─── Color Manipulation ──────────────────────────────────────────────────────

export function lighten(hex: string, amount: number): string {
  const hsl = hexToHsl(hex);
  hsl.l = Math.min(100, hsl.l + amount);
  return hslToHex(hsl);
}

export function darken(hex: string, amount: number): string {
  const hsl = hexToHsl(hex);
  hsl.l = Math.max(0, hsl.l - amount);
  return hslToHex(hsl);
}

// ─── WCAG Luminance & Contrast ───────────────────────────────────────────────

export function relativeLuminance(hex: string): number {
  const h = hex.replace('#', '');
  const r = Number.parseInt(h.substring(0, 2), 16) / 255;
  const g = Number.parseInt(h.substring(2, 4), 16) / 255;
  const b = Number.parseInt(h.substring(4, 6), 16) / 255;

  const linearize = (c: number) => (c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4));

  return 0.2126 * linearize(r) + 0.7152 * linearize(g) + 0.0722 * linearize(b);
}

export function contrastColor(hex: string): '#ffffff' | '#000000' {
  return relativeLuminance(hex) > 0.179 ? '#000000' : '#ffffff';
}

// ─── Validation ──────────────────────────────────────────────────────────────

export function isValidHex(hex: string): boolean {
  return /^#[0-9a-fA-F]{6}$/.test(hex);
}

// ─── Theme Derivation ────────────────────────────────────────────────────────

export function deriveThemeVariables(colors: CustomColors, isDark: boolean): DerivedThemeVariables {
  if (isDark) {
    return deriveDark(colors);
  }
  return deriveLight(colors);
}

function deriveDark(colors: CustomColors): DerivedThemeVariables {
  const bg = colors.background;
  const a1 = colors.accentPrimary;
  const a2 = colors.accentSecondary;

  return {
    '--bg-primary': bg,
    '--bg-secondary': lighten(bg, 5),
    '--bg-tertiary': lighten(bg, 10),
    '--bg-hover': lighten(bg, 8),
    '--bg-active': lighten(bg, 12),
    '--text-primary': contrastColor(bg),
    '--text-secondary': lighten(bg, 55),
    '--text-muted': lighten(bg, 35),
    '--text-tertiary': lighten(bg, 25),
    '--accent-primary': a1,
    '--accent-secondary': a2,
    '--accent-hover': lighten(a1, 15),
    '--accent-color': 'var(--accent-primary)',
    '--gradient-brand': `linear-gradient(90deg, ${a1} 0%, ${a2} 100%)`,
    '--border-color': lighten(bg, 15),
    '--on-accent': contrastColor(a1),
    '--success': '#43b581',
    '--danger': '#f04747',
    '--danger-hover': '#d43b3b',
    '--error-color': 'var(--danger)',
    '--success-color': 'var(--success)',
    '--status-connected': '#1aaa55',
    '--status-connecting': '#e6b432',
    '--status-disconnected': '#fa6464',
  };
}

function deriveLight(colors: CustomColors): DerivedThemeVariables {
  const bgHsl = hexToHsl(colors.background);
  const a1 = colors.accentPrimary;
  const a2 = colors.accentSecondary;

  // Light bg: keep hue from dark bg, desaturate, set high lightness
  const bgLight = hslToHex({ h: bgHsl.h, s: Math.round(bgHsl.s * 0.3), l: 96 });
  // Dark text derived from original dark bg hue
  const textDark = hslToHex({ h: bgHsl.h, s: Math.round(bgHsl.s * 0.5), l: 10 });
  // Slightly darker accents for contrast on light bg
  const lightA1 = darken(a1, 8);
  const lightA2 = darken(a2, 8);

  return {
    '--bg-primary': bgLight,
    '--bg-secondary': lighten(bgLight, 2),
    '--bg-tertiary': darken(bgLight, 5),
    '--bg-hover': darken(bgLight, 8),
    '--bg-active': darken(bgLight, 10),
    '--text-primary': textDark,
    '--text-secondary': lighten(textDark, 20),
    '--text-muted': lighten(textDark, 38),
    '--text-tertiary': lighten(textDark, 30),
    '--accent-primary': lightA1,
    '--accent-secondary': lightA2,
    '--accent-hover': lighten(lightA1, 10),
    '--accent-color': 'var(--accent-primary)',
    '--gradient-brand': `linear-gradient(90deg, ${lightA1} 0%, ${lightA2} 100%)`,
    '--border-color': darken(bgLight, 15),
    '--on-accent': contrastColor(lightA1),
    '--success': '#2d9f6f',
    '--danger': '#e03e3e',
    '--danger-hover': '#c83232',
    '--error-color': 'var(--danger)',
    '--success-color': 'var(--success)',
    '--status-connected': '#1a9a4a',
    '--status-connecting': '#c89a20',
    '--status-disconnected': '#e05050',
  };
}

// ─── DOM Application ─────────────────────────────────────────────────────────

export function applyCustomThemeVariables(vars: DerivedThemeVariables): void {
  const el = document.documentElement;
  for (const [prop, value] of Object.entries(vars)) {
    el.style.setProperty(prop, value);
  }
}

export function clearCustomThemeVariables(): void {
  const el = document.documentElement;
  for (const prop of THEME_VARIABLE_KEYS) {
    el.style.removeProperty(prop);
  }
}
