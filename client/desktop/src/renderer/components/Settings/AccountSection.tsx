import CollapsibleSection from './CollapsibleSection';
import NsfwContentGate from './NsfwContentGate';
import ProfileInfoForm from '../Profile/ProfileInfoForm';
import PasswordChangeForm from '../Profile/PasswordChangeForm';
// Form/avatar/header/links/section styling for the relocated profile editor (#1773).
// Recovered from the former ProfilePage.css (the page is gone; these rules are still
// used by the two forms, which import no CSS of their own).
import '../Profile/profileForms.css';

// Account settings. Hosts the profile editor (#1773) and the NSFW Content Access
// age gate (#1625). Future account features add sibling <CollapsibleSection>s here.
const AccountSection: React.FC = () => (
  <>
    <CollapsibleSection id="section-profile" title="My Profile" defaultOpen>
      <ProfileInfoForm />
      <PasswordChangeForm />
    </CollapsibleSection>
    <CollapsibleSection id="section-nsfw-content" title="NSFW Content Access" defaultOpen>
      <NsfwContentGate />
    </CollapsibleSection>
  </>
);

export default AccountSection;
