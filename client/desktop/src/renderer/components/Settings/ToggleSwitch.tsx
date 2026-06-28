import React from 'react';

interface ToggleSwitchProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
  label?: string;
  ariaLabelledBy?: string;
  id?: string;
  inputRole?: 'switch';
  /**
   * Forwarded onto the focusable `<input>` so a wrapping `PremiumGate` can mark a
   * locked toggle dormant WITHOUT removing it from the tab order (#1301 O1 — the
   * gate clones its child to inject these). `aria-disabled` announces dormancy;
   * `aria-describedby` points the AT user at the explanatory chip. Never the HTML
   * `disabled` attribute (that would unfocus the control).
   */
  'aria-disabled'?: boolean | 'true' | 'false';
  'aria-describedby'?: string;
}

const ToggleSwitch: React.FC<ToggleSwitchProps> = ({
  checked,
  onChange,
  disabled,
  label,
  ariaLabelledBy,
  id,
  inputRole,
  'aria-disabled': ariaDisabled,
  'aria-describedby': ariaDescribedBy,
}) => (
  <label
    className="settings-toggle"
    aria-label={ariaLabelledBy ? undefined : label || 'Toggle'}
    aria-labelledby={ariaLabelledBy}
  >
    <input
      type="checkbox"
      role={inputRole}
      id={id}
      checked={checked}
      onChange={(e) => onChange(e.target.checked)}
      disabled={disabled}
      aria-label={ariaLabelledBy ? undefined : label || 'Toggle'}
      aria-labelledby={ariaLabelledBy}
      aria-disabled={ariaDisabled}
      aria-describedby={ariaDescribedBy}
    />
    <span className="settings-toggle-track" />
    <span className="settings-toggle-thumb" />
  </label>
);

export default ToggleSwitch;
