import React, { useState } from 'react';
import { DockShell } from '../Layout/DockShell';
import FriendsList from './FriendsList';

interface FriendsFlexSpaceProps {
  onFriendClick?: (userId: string) => void;
}

// #2653 item 2a: the dock renders ONE header row — `[title | actions | pin]` (spec §4 item
// 2a) — instead of floating an actions-only pin over a second row FriendsList draws for
// itself. This host is that row's non-pin half; FriendsList portals its title and actions
// into it (see the `headerHost` prop there), which keeps the pending badge, the search term
// that mutes it, and both modals in the one component that already owns them.
//
// A non-null `header` is what drops `dock-shell__header--actions-only`, and returning `null`
// for compact is deliberate: the spec preserves compact behaviour exactly, so the rail keeps
// its own action row and the dock header carries only the pin. `null` also keeps
// `DockHeader`'s `headerContent === null && !showPinButton` early-return reachable, so an
// interface-locked compact dock still renders no empty padded strip.
// At module scope so the element type is stable across renders. Note this is NOT the remount
// hazard `typescript:S6478` describes — `DockShell` invokes the render prop and embeds the
// returned element, so React never sees a component type here and never remounted the host.
// An earlier commit on this branch hoisted it citing that hazard; the hoist is fine, the
// rationale was wrong. What actually satisfies the rule is the `renderHeader` prop name.
const FriendsDockHeaderHost: React.FC<{ hostRef: (el: HTMLDivElement | null) => void }> = ({
  hostRef,
}) => <div className="friends-list-header friends-list-header--dock" ref={hostRef} />;

const FriendsFlexSpace: React.FC<FriendsFlexSpaceProps> = ({ onFriendClick }) => {
  const [headerHost, setHeaderHost] = useState<HTMLDivElement | null>(null);

  return (
    <DockShell
      context="dm"
      side="right"
      label="Friends"
      renderHeader={(compact) =>
        compact ? null : <FriendsDockHeaderHost hostRef={setHeaderHost} />
      }
      renderBody={(compact) => (
        <FriendsList compact={compact} headerHost={headerHost} onFriendClick={onFriendClick} />
      )}
    />
  );
};

export default FriendsFlexSpace;
