import CollapsibleSection from './CollapsibleSection';
import NsfwContentGate from './NsfwContentGate';
import ProfileInfoForm from '../Profile/ProfileInfoForm';
import SubscriptionSection from './subscription/SubscriptionSection';
// Form/avatar/header/links/section styling for the relocated profile editor (#1773).
// Recovered from the former ProfilePage.css (the page is gone; these rules are still
// used by the forms, which import no CSS of their own).
import '../Profile/profileForms.css';

// Account settings. Hosts the profile editor (#1773) and the NSFW Content Access
// age gate (#1625), alongside the compact account subscription. Sign-in protections
// and password changes live in Security.
function AccountSection() {
  return (
    <>
      <CollapsibleSection id="section-profile" title="Profile Information">
        <ProfileInfoForm />
      </CollapsibleSection>
      <SubscriptionSection />
      <CollapsibleSection id="section-nsfw-content" title="NSFW Content Access">
        <NsfwContentGate />
      </CollapsibleSection>
    </>
  );
}

export default AccountSection;
