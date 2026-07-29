import React from 'react';
import { DockShell } from '../Layout/DockShell';
import FriendsList from './FriendsList';

interface FriendsFlexSpaceProps {
  onFriendClick?: (userId: string) => void;
}

const FriendsFlexSpace: React.FC<FriendsFlexSpaceProps> = ({ onFriendClick }) => (
  <DockShell
    context="dm"
    side="right"
    label="Friends"
    header={null}
    renderBody={(compact) => <FriendsList compact={compact} onFriendClick={onFriendClick} />}
  />
);

export default FriendsFlexSpace;
