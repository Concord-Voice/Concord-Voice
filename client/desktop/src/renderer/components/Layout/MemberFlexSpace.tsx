import React from 'react';
import { useServerStore } from '../../stores/serverStore';
import MemberList from '../Members/MemberList';
import { DockShell } from './DockShell';

const MemberFlexSpace: React.FC = () => {
  const activeServerId = useServerStore((state) => state.activeServerId);
  if (!activeServerId) return null;

  return (
    <DockShell
      context="server"
      side="right"
      label="Members"
      header={null}
      renderBody={(compact) => <MemberList compact={compact} />}
    />
  );
};

export default MemberFlexSpace;
