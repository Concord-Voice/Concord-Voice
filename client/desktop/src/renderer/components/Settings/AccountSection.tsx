import CollapsibleSection from './CollapsibleSection';
import NsfwContentGate from './NsfwContentGate';

// Account settings. Currently hosts only the NSFW Content Access age gate (#1625);
// future account features add sibling <CollapsibleSection>s here.
const AccountSection: React.FC = () => (
  <CollapsibleSection id="section-nsfw-content" title="NSFW Content Access" defaultOpen>
    <NsfwContentGate />
  </CollapsibleSection>
);

export default AccountSection;
