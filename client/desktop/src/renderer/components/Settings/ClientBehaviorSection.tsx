import React from 'react';
import { useSettingsStore } from '../../stores/settingsStore';
import {
  isOptionDisabled,
  type ClientBehavior,
  type ToTrayChoice,
  type ToToolbarChoice,
} from '../../../shared/clientBehavior';
import { ClientBehaviorExplanation } from './ClientBehaviorExplanation';
import CollapsibleSection from './CollapsibleSection';
import './ClientBehaviorSection.css';

const TO_TRAY_OPTIONS: { value: ToTrayChoice; label: string }[] = [
  { value: 'close', label: 'Close [X]' },
  { value: 'minimize', label: 'Minimize [-]' },
  { value: 'none', label: 'None' },
];

const TO_TOOLBAR_OPTIONS: { value: ToToolbarChoice; label: string }[] = [
  { value: 'minimize', label: 'Minimize [-]' },
  { value: 'close', label: 'Close [X]' },
];

/**
 * Produce the user-facing reason an option is disabled, surfaced via the
 * native title="" tooltip. The only disable rule is the mutex: the same
 * destination value is already assigned to the other setting. (There is no
 * "coverage" rule — every button always resolves to a destination; see
 * clientBehavior.ts and #1148.)
 */
function disabledReason(
  setting: 'toTray' | 'toToolbar',
  option: string,
  cb: ClientBehavior
): string | undefined {
  if (!isOptionDisabled(setting, option, cb)) return undefined;
  return `Already selected for ${setting === 'toTray' ? 'To Toolbar' : 'To Tray'}.`;
}

interface OptionRowProps<V extends string> {
  readonly setting: 'toTray' | 'toToolbar';
  readonly options: ReadonlyArray<{ value: V; label: string }>;
  readonly clientBehavior: ClientBehavior;
  readonly current: V;
  readonly onChange: (value: V) => void;
}

function OptionRow<V extends string>({
  setting,
  options,
  clientBehavior,
  current,
  onChange,
}: OptionRowProps<V>): React.ReactElement {
  return (
    <div className="client-behavior-options" role="radiogroup">
      {options.map((opt) => {
        const disabled = isOptionDisabled(setting, opt.value, clientBehavior);
        const selected = current === opt.value;
        const reason = disabledReason(setting, opt.value, clientBehavior);
        const labelDisplay =
          setting === 'toTray' ? `To Tray: ${opt.label}` : `To Toolbar: ${opt.label}`;
        return (
          <label
            key={opt.value}
            className={[
              'client-behavior-option',
              selected ? 'client-behavior-option--selected' : '',
              disabled ? 'client-behavior-option--disabled' : '',
            ]
              .filter(Boolean)
              .join(' ')}
            title={reason}
          >
            <input
              type="radio"
              className="client-behavior-option-input"
              name={setting}
              value={opt.value}
              checked={selected}
              disabled={disabled}
              onChange={() => onChange(opt.value)}
              aria-label={labelDisplay}
            />
            {opt.label}
          </label>
        );
      })}
    </div>
  );
}

export const ClientBehaviorSection: React.FC = () => {
  const clientBehavior = useSettingsStore((s) => s.clientBehavior);
  const setClientBehavior = useSettingsStore((s) => s.setClientBehavior);

  const onTrayChange = (value: ToTrayChoice): void => {
    setClientBehavior({ ...clientBehavior, toTray: value });
  };

  const onToolbarChange = (value: ToToolbarChoice): void => {
    setClientBehavior({ ...clientBehavior, toToolbar: value });
  };

  return (
    <CollapsibleSection id="client-behavior" title="Client Behavior" defaultOpen>
      <p className="settings-section-description">
        Choose where the [×] close and [—] minimize buttons send Concord Voice. The dynamic panel
        below explains what each configuration does.
      </p>

      <div className="client-behavior-section">
        <fieldset className="client-behavior-fieldset">
          <legend className="client-behavior-legend">To Tray</legend>
          <OptionRow
            setting="toTray"
            options={TO_TRAY_OPTIONS}
            clientBehavior={clientBehavior}
            current={clientBehavior.toTray}
            onChange={onTrayChange}
          />
        </fieldset>

        <fieldset className="client-behavior-fieldset">
          <legend className="client-behavior-legend">To Toolbar</legend>
          <OptionRow
            setting="toToolbar"
            options={TO_TOOLBAR_OPTIONS}
            clientBehavior={clientBehavior}
            current={clientBehavior.toToolbar}
            onChange={onToolbarChange}
          />
        </fieldset>

        <ClientBehaviorExplanation clientBehavior={clientBehavior} />
      </div>
    </CollapsibleSection>
  );
};
