import React from 'react';
import type { SidebarContext } from '../../stores/layoutStore';
import UserPanel from '../User/UserPanel';
import { DockShell } from './DockShell';

interface ChannelPanelProps {
  context: SidebarContext;
  header: React.ReactNode | ((compact: boolean) => React.ReactNode);
  renderContent: (compact: boolean) => React.ReactNode;
  forcePin?: boolean;
}

const renderUserPanelFooter = (compact: boolean) => <UserPanel compact={compact} />;

const ChannelPanel: React.FC<ChannelPanelProps> = ({
  context,
  header,
  renderContent,
  forcePin = false,
}) => (
  <DockShell
    context={context}
    side="left"
    label={context === 'dm' ? 'Threads' : 'Channels'}
    header={header}
    forcePinned={forcePin}
    footer={renderUserPanelFooter}
    renderBody={renderContent}
  />
);

export default ChannelPanel;
