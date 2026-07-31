import React from 'react';
import type { SidebarContext } from '../../stores/layoutStore';
import UserPanel from '../User/UserPanel';
import { DockShell } from './DockShell';

interface ChannelPanelProps {
  context: SidebarContext;
  /** Presentation-dependent header, forwarded to DockShell's `renderHeader`. The
   *  `render` prefix is load-bearing — see the note on that prop. */
  renderHeader: (compact: boolean) => React.ReactNode;
  renderContent: (compact: boolean) => React.ReactNode;
  forcePin?: boolean;
}

const renderUserPanelFooter = (compact: boolean) => <UserPanel compact={compact} />;

const ChannelPanel: React.FC<ChannelPanelProps> = ({
  context,
  renderHeader,
  renderContent,
  forcePin = false,
}) => (
  <DockShell
    context={context}
    side="left"
    label={context === 'dm' ? 'Threads' : 'Channels'}
    renderHeader={renderHeader}
    forcePinned={forcePin}
    footer={renderUserPanelFooter}
    renderBody={renderContent}
  />
);

export default ChannelPanel;
