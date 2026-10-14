import PrivacySection from './PrivacySection';
import SecuritySection from './SecuritySection';
import SystemPermissionsSection from './SystemPermissionsSection';

export { resolveSSOToggleError } from './SecuritySection';

/** Legacy composition for callers exercising the shared privacy and security controls. */
function PrivacySecuritySection() {
  return (
    <>
      <PrivacySection />
      <SystemPermissionsSection />
      <SecuritySection />
    </>
  );
}

export default PrivacySecuritySection;
