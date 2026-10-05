import './DeviceRecoveryFingerprint.css';

const GROUP_SLOTS = ['one', 'two', 'three', 'four', 'five', 'six', 'seven', 'eight'];

interface DeviceRecoveryFingerprintProps {
  readonly fingerprint: string;
}

export default function DeviceRecoveryFingerprint({ fingerprint }: DeviceRecoveryFingerprintProps) {
  const groups = fingerprint.split(' ');
  if (groups.length !== 8 || groups.some((group) => !/^[0-9A-F]{4}$/.test(group))) return null;
  return (
    <fieldset className="device-recovery-fingerprint" aria-label="Recovery fingerprint">
      {GROUP_SLOTS.map((slot, index) => (
        <span
          role="img"
          key={slot}
          aria-label={`Group ${index + 1}: ${groups[index].split('').join(' ')}`}
        >
          <span aria-hidden="true">{groups[index]}</span>
        </span>
      ))}
    </fieldset>
  );
}
