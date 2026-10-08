import React, { useCallback, useId } from 'react';
import { ShieldAlert } from 'lucide-react';
import { PERMISSION_CATEGORIES, type PermissionInfo } from '../../utils/policy/permissions';
import './PermissionGrid.css';

interface PermissionGridProps {
  value: bigint;
  onChange: (newValue: bigint) => void;
  disabled?: boolean;
  mode?: 'role' | 'override';
  deny?: bigint;
  onDenyChange?: (newDeny: bigint) => void;
}

type OverrideState = 'allow' | 'neutral' | 'deny';

/** The id of a `dangerous` row's note, which the control that turns the bit on points at. */
function noteIdFor(base: string, perm: PermissionInfo): string | undefined {
  return perm.dangerous ? `${base}-${perm.key}` : undefined;
}

/**
 * Said on every `dangerous` row in both modes (#3456 §3.7). The grid edits a
 * role, not the viewer's own grants, so it never masks or disables a control:
 * which bits a server gates is the server's decision, answered when a save is
 * sent.
 */
const MFA_NOTE = 'Turning this on needs MFA confirmation on servers that enforce MFA.';

function getOverrideState(allow: bigint, deny: bigint, bit: bigint): OverrideState {
  // Deny takes precedence over allow if both bits are set (matches SBAC semantics)
  if ((deny & bit) !== 0n) return 'deny';
  if ((allow & bit) !== 0n) return 'allow';
  return 'neutral';
}

const PermissionGrid: React.FC<PermissionGridProps> = ({
  value,
  onChange,
  disabled = false,
  mode = 'role',
  deny = 0n,
  onDenyChange,
}) => {
  const noteBaseId = useId();

  const handleToggle = useCallback(
    (bit: bigint) => {
      if (disabled) return;
      const isSet = (value & bit) !== 0n;
      onChange(isSet ? value & ~bit : value | bit);
    },
    [value, onChange, disabled]
  );

  const handleOverride = useCallback(
    (bit: bigint, state: OverrideState) => {
      if (disabled) return;

      // Clear the bit from both allow and deny first
      let newAllow = value & ~bit;
      let newDeny = deny & ~bit;

      if (state === 'allow') {
        newAllow = newAllow | bit;
      } else if (state === 'deny') {
        newDeny = newDeny | bit;
      }

      onChange(newAllow);
      onDenyChange?.(newDeny);
    },
    [value, deny, onChange, onDenyChange, disabled]
  );

  const renderRoleControl = useCallback(
    (perm: PermissionInfo) => {
      const isActive = (value & perm.bit) !== 0n;
      return (
        <div
          className={`permission-toggle${isActive ? ' active' : ''}`}
          onClick={() => handleToggle(perm.bit)}
          role="switch"
          aria-checked={isActive}
          aria-label={perm.label}
          aria-describedby={noteIdFor(noteBaseId, perm)}
          tabIndex={0}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              handleToggle(perm.bit);
            }
          }}
        >
          <div className="permission-toggle-knob" />
        </div>
      );
    },
    [value, handleToggle, noteBaseId]
  );

  const renderOverrideControl = useCallback(
    (perm: PermissionInfo) => {
      const state = getOverrideState(value, deny, perm.bit);
      return (
        <div className="permission-tristate">
          <button
            type="button"
            className={`permission-tristate-btn${state === 'allow' ? ' allow-active' : ''}`}
            onClick={() => handleOverride(perm.bit, state === 'allow' ? 'neutral' : 'allow')}
            aria-label={`Allow ${perm.label}`}
            aria-describedby={noteIdFor(noteBaseId, perm)}
            title="Allow"
          >
            &#x2713;
          </button>
          <button
            type="button"
            className={`permission-tristate-btn${state === 'neutral' ? ' neutral-active' : ''}`}
            onClick={() => handleOverride(perm.bit, 'neutral')}
            aria-label={`Neutral ${perm.label}`}
            title="Neutral"
          >
            &#x2014;
          </button>
          <button
            type="button"
            className={`permission-tristate-btn${state === 'deny' ? ' deny-active' : ''}`}
            onClick={() => handleOverride(perm.bit, state === 'deny' ? 'neutral' : 'deny')}
            aria-label={`Deny ${perm.label}`}
            title="Deny"
          >
            &#x2717;
          </button>
        </div>
      );
    },
    [value, deny, handleOverride, noteBaseId]
  );

  return (
    <div className="permission-grid">
      {PERMISSION_CATEGORIES.map((category) => (
        <div className="permission-category" key={category.name}>
          <div className="permission-category-header">{category.name}</div>
          {category.permissions.map((perm) => (
            <div className={`permission-row${disabled ? ' disabled' : ''}`} key={perm.key}>
              <div className="permission-info">
                <span className="permission-label">{perm.label}</span>
                <span className="permission-description">{perm.description}</span>
                {perm.dangerous && (
                  <span className="permission-mfa-note" id={noteIdFor(noteBaseId, perm)}>
                    <ShieldAlert size={14} aria-hidden="true" />
                    <span>{MFA_NOTE}</span>
                  </span>
                )}
              </div>
              {mode === 'override' ? renderOverrideControl(perm) : renderRoleControl(perm)}
            </div>
          ))}
        </div>
      ))}
    </div>
  );
};

export default PermissionGrid;
