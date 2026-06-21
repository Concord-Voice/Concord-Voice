/**
 * SSOButton (#270)
 *
 * Provider-branded "Sign in with X" button. Google branding follows
 * https://developers.google.com/identity/branding-guidelines (white background,
 * 1px border, official 4-color "G" mark, 14px Roboto/system text). Apple variant
 * is forward-compat for #271 and uses the conventional black-fill / white-glyph
 * Sign in with Apple HIG style.
 *
 * The component is purely presentational — flow state lives in `useSSOStore`,
 * driven by `useSSOFlow().begin(provider)`. Callers wire the click to `begin`.
 */
import './SSOButton.css';

interface SSOButtonProps {
  provider: 'google' | 'apple';
  onClick: () => void;
  disabled?: boolean;
}

export function SSOButton({ provider, onClick, disabled }: Readonly<SSOButtonProps>) {
  const label = provider === 'google' ? 'Sign in with Google' : 'Sign in with Apple';
  return (
    <button
      type="button"
      className={`sso-button sso-button--${provider}`}
      onClick={onClick}
      disabled={disabled}
      aria-label={label}
    >
      <span className="sso-button__icon" aria-hidden="true">
        {provider === 'google' ? <GoogleLogo /> : <AppleLogo />}
      </span>
      <span className="sso-button__text">{label}</span>
    </button>
  );
}

/**
 * Official Google "G" mark — 18×18, four-color. Source:
 * https://developers.google.com/identity/branding-guidelines (g-logo.svg).
 * Inlined to avoid an extra asset request and keep the button self-contained.
 */
function GoogleLogo() {
  return (
    <svg width="18" height="18" viewBox="0 0 18 18" xmlns="http://www.w3.org/2000/svg">
      <path
        d="M16.51 8.18c0-.59-.05-1.16-.15-1.71H8.5v3.24h4.49c-.19 1.04-.78 1.92-1.66 2.51v2.08h2.69c1.57-1.45 2.49-3.59 2.49-6.12z"
        fill="#4285F4"
      />
      <path
        d="M8.5 17c2.25 0 4.13-.75 5.51-2.03l-2.69-2.08c-.75.5-1.71.79-2.82.79-2.17 0-4.01-1.46-4.66-3.43H1.06v2.15C2.42 14.96 5.25 17 8.5 17z"
        fill="#34A853"
      />
      <path
        d="M3.84 10.25c-.16-.5-.26-1.03-.26-1.58s.09-1.08.26-1.58V4.95H1.06C.39 6.05 0 7.45 0 9c0 1.55.39 2.95 1.06 4.05l2.78-2.05.06-.05z"
        fill="#FBBC04"
      />
      <path
        d="M8.5 3.58c1.22 0 2.32.42 3.18 1.24l2.39-2.39C12.62.89 10.74 0 8.5 0 5.25 0 2.42 2.04 1.06 4.95l2.78 2.15c.65-1.97 2.49-3.52 4.66-3.52z"
        fill="#EA4335"
      />
    </svg>
  );
}

/**
 * Apple logomark — monochrome glyph, painted with `currentColor` so the
 * surrounding button can recolor it (white on black per HIG).
 */
function AppleLogo() {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 24 24"
      xmlns="http://www.w3.org/2000/svg"
      fill="currentColor"
    >
      <path d="M17.05 12.04c-.03-3.02 2.47-4.48 2.58-4.55-1.41-2.06-3.6-2.34-4.38-2.37-1.86-.19-3.64 1.1-4.59 1.1-.95 0-2.41-1.07-3.97-1.04-2.04.03-3.93 1.19-4.98 3.02-2.13 3.69-.54 9.16 1.52 12.16 1.01 1.47 2.21 3.12 3.78 3.06 1.52-.06 2.09-.98 3.93-.98 1.84 0 2.36.98 3.97.95 1.64-.03 2.68-1.49 3.68-2.97 1.16-1.7 1.64-3.35 1.66-3.43-.04-.02-3.18-1.22-3.21-4.84zM14.05 3.4c.84-1.02 1.4-2.43 1.25-3.84-1.21.05-2.67.81-3.54 1.82-.78.9-1.46 2.34-1.28 3.72 1.35.1 2.73-.69 3.57-1.7z" />
    </svg>
  );
}
