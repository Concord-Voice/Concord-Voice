import React, { useEffect, useState } from 'react';
import {
  useOsPermissionStore,
  type OsPermissionType,
  type OsPermissionStatus,
} from '../../stores/voice/osPermissionStore';
import CollapsibleSection from './CollapsibleSection';

// ─── System Permissions Sub-section (#197) ─────────────────────────────

const PERMISSION_ROWS: {
  type: OsPermissionType;
  label: string;
  description: string;
  critical?: boolean;
}[] = [
  {
    type: 'secureStorage',
    label: 'Secure Storage (Keychain)',
    description: 'Required to safely store authentication tokens and encryption keys.',
    critical: true,
  },
  {
    type: 'microphone',
    label: 'Microphone',
    description: 'Used for voice channels and calls.',
  },
  {
    type: 'camera',
    label: 'Camera',
    description: 'Used for video in voice channels and calls.',
  },
  {
    type: 'screen',
    label: 'Screen Recording',
    description: 'Used for screen sharing in voice channels.',
  },
  {
    type: 'notifications',
    label: 'Notifications',
    description: 'Used for desktop notifications and incoming call alerts.',
  },
];

const PERMISSION_STATUS_BADGES: Record<OsPermissionStatus, { className: string; label: string }> = {
  granted: { className: 'os-perm-badge os-perm-badge--granted', label: 'Granted' },
  denied: { className: 'os-perm-badge os-perm-badge--denied', label: 'Denied' },
  restricted: { className: 'os-perm-badge os-perm-badge--denied', label: 'Restricted' },
  'not-determined': { className: 'os-perm-badge os-perm-badge--pending', label: 'Not Requested' },
  unavailable: { className: 'os-perm-badge os-perm-badge--unavailable', label: 'Unavailable' },
};

function permissionStatusBadge(status: OsPermissionStatus): {
  className: string;
  label: string;
} {
  return PERMISSION_STATUS_BADGES[status] ?? { className: 'os-perm-badge', label: 'Unknown' };
}

const SystemPermissionsSection: React.FC = () => {
  const fetchAll = useOsPermissionStore((s) => s.fetchAll);
  const requestOne = useOsPermissionStore((s) => s.requestOne);
  const openSettings = useOsPermissionStore((s) => s.openSettings);
  const isLoaded = useOsPermissionStore((s) => s.isLoaded);

  // Fetch fresh permission statuses when Settings is opened.
  useEffect(() => {
    fetchAll();
  }, [fetchAll]);

  return (
    <CollapsibleSection id="section-system-permissions" title="System Permissions">
      <p className="settings-section-description">
        These show the status of permissions managed by your operating system — they are not in-app
        switches. Concord requests each permission only when it&apos;s needed. Use &ldquo;Open
        System Settings&rdquo; to review or change a permission in your OS.
      </p>

      {isLoaded ? (
        PERMISSION_ROWS.map((row) => (
          <PermissionRow
            key={row.type}
            type={row.type}
            label={row.label}
            description={row.description}
            critical={row.critical}
            onRequest={requestOne}
            onOpenSettings={openSettings}
          />
        ))
      ) : (
        <div className="settings-row">
          <span className="settings-row-label">Loading permission statuses...</span>
        </div>
      )}
    </CollapsibleSection>
  );
};

const PermissionRow: React.FC<{
  type: OsPermissionType;
  label: string;
  description: string;
  critical?: boolean;
  onRequest: (type: OsPermissionType) => Promise<OsPermissionStatus>;
  onOpenSettings: (type: OsPermissionType) => Promise<void>;
}> = ({ type, label, description, critical, onRequest, onOpenSettings }) => {
  const status = useOsPermissionStore((s) => s[type]);
  const badge = permissionStatusBadge(status);
  const [isRequesting, setIsRequesting] = useState(false);

  const handleRequest = async () => {
    setIsRequesting(true);
    try {
      await onRequest(type);
    } finally {
      setIsRequesting(false);
    }
  };

  const handleOpenSettings = () => {
    onOpenSettings(type);
  };

  return (
    <div
      className={`settings-row ${critical && status !== 'granted' ? 'settings-row--warning' : ''}`}
    >
      <div className="settings-row-info">
        <span className="settings-row-label">
          {label}
          {critical && <span className="os-perm-critical"> (Required)</span>}
        </span>
        <span className="settings-row-hint">{description}</span>
        <span className="settings-row-hint os-perm-managed">Managed by your operating system.</span>
        {critical && status !== 'granted' && (
          <span className="settings-row-hint os-perm-warning">
            Secure storage is required for login. Please enable keychain / credential manager
            access.
          </span>
        )}
      </div>
      <div className="os-perm-actions">
        <span className={badge.className}>{badge.label}</span>
        {status === 'not-determined' ? (
          <button
            className="btn btn-sm btn-primary"
            onClick={handleRequest}
            disabled={isRequesting}
          >
            {isRequesting ? 'Requesting...' : 'Request'}
          </button>
        ) : (
          <button className="btn btn-sm btn-secondary" onClick={handleOpenSettings}>
            Open System Settings
          </button>
        )}
      </div>
    </div>
  );
};

export default SystemPermissionsSection;
