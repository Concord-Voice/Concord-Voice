import React from 'react';

interface ToggleSwitchProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
  label?: string;
  ariaLabelledBy?: string;
  id?: string;
}

const ToggleSwitch: React.FC<ToggleSwitchProps> = ({
  checked,
  onChange,
  disabled,
  label,
  ariaLabelledBy,
  id,
}) => (
  <label
    className="settings-toggle"
    {...(ariaLabelledBy
      ? { 'aria-labelledby': ariaLabelledBy }
      : { 'aria-label': label || 'Toggle' })}
  >
    <input
      type="checkbox"
      id={id}
      checked={checked}
      onChange={(e) => onChange(e.target.checked)}
      disabled={disabled}
    />
    <span className="settings-toggle-track" />
    <span className="settings-toggle-thumb" />
  </label>
);

export default ToggleSwitch;
