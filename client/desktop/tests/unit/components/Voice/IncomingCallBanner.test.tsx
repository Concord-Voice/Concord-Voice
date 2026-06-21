// Tests for IncomingCallBanner — the root-mounted corner banner for
// incoming DM voice calls (#1209 plan task F1). Covers render-gating on
// voiceStore.callState, avatar fallback (initials), and the accept /
// decline button → callStateMachine dispatch.
//
// Filed per PR #1231 SonarCloud Quality Gate coverage gap.

import React from 'react';
import { render, screen, fireEvent } from '../../../test-utils';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import type { DMConversation } from '@/renderer/stores/dmStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/IncomingCallBanner.css', () => ({}));

// Mocks return resolved Promises so the click-handler `.catch(...)` chains
// (added per Copilot #1231 C9/C10) don't TypeError on undefined.
const mockAccept = vi.fn(() => Promise.resolve());
const mockDecline = vi.fn(() => Promise.resolve());

vi.mock('@/renderer/services/voiceService/callStateMachine', () => ({
  acceptIncomingCall: () => mockAccept(),
  declineIncomingCall: () => mockDecline(),
}));

import { IncomingCallBanner } from '@/renderer/components/Voice/IncomingCallBanner';

const CONVERSATION_ID = '11111111-1111-1111-1111-111111111111';
const RING_ID = '22222222-2222-2222-2222-222222222222';

function setIncomingRingingState(
  callerOverrides: Record<string, unknown> = {},
  isGroup = false
): void {
  useVoiceStore.getState().setCallState({
    kind: 'incoming-ringing',
    conversationId: CONVERSATION_ID,
    ringId: RING_ID,
    caller: {
      userId: 'caller-id',
      username: 'caller-username',
      ...callerOverrides,
    },
    expiresAt: Date.now() + 30000,
    isGroup,
  });
}

function seedGroupConversation(name: string | null = 'Squad'): void {
  useDMStore.setState({
    conversations: [
      {
        id: CONVERSATION_ID,
        isGroup: true,
        isPersonal: false,
        name,
        participants: [
          { userId: 'b', username: 'Bob' },
          { userId: 'c', username: 'Charlie' },
        ],
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2026-05-28T13:00:00.000Z',
      } as DMConversation,
    ],
  });
}

describe('IncomingCallBanner', () => {
  beforeEach(() => {
    resetAllStores();
    mockAccept.mockReset();
    mockDecline.mockReset();
  });

  describe('render gating', () => {
    it('returns null when callState is idle', () => {
      const { container } = render(<IncomingCallBanner />);
      expect(container.firstChild).toBeNull();
    });

    it('returns null when callState is in-call', () => {
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
      const { container } = render(<IncomingCallBanner />);
      expect(container.firstChild).toBeNull();
    });

    it('returns null when callState is outgoing-ringing', () => {
      useVoiceStore.getState().setCallState({
        kind: 'outgoing-ringing',
        conversationId: CONVERSATION_ID,
        ringId: RING_ID,
        calleeUserIds: ['callee'],
        startedAt: 1000,
      });
      const { container } = render(<IncomingCallBanner />);
      expect(container.firstChild).toBeNull();
    });

    it('renders when callState is incoming-ringing', () => {
      setIncomingRingingState();
      render(<IncomingCallBanner />);
      expect(screen.getByRole('alert')).toBeInTheDocument();
    });
  });

  describe('avatar rendering', () => {
    it('renders <img> when caller has avatarUrl', () => {
      setIncomingRingingState({ avatarUrl: 'https://example.com/a.png' });
      render(<IncomingCallBanner />);
      const img = screen.getByRole('img');
      expect(img).toHaveAttribute('src', 'https://example.com/a.png');
    });

    it('renders initials when caller has no avatarUrl (uses displayName)', () => {
      setIncomingRingingState({ displayName: 'Alice Smith' });
      render(<IncomingCallBanner />);
      // Initials are first 2 chars uppercased: 'AL'
      expect(screen.getByText('AL')).toBeInTheDocument();
    });

    it('falls back to username for initials when displayName is not set', () => {
      setIncomingRingingState({ username: 'bob' });
      render(<IncomingCallBanner />);
      // First 2 chars uppercased: 'BO'
      expect(screen.getByText('BO')).toBeInTheDocument();
    });
  });

  describe('name display', () => {
    it('prefers displayName when present', () => {
      setIncomingRingingState({ username: 'bob', displayName: 'Bob Builder' });
      render(<IncomingCallBanner />);
      expect(screen.getByText('Bob Builder')).toBeInTheDocument();
    });

    it('falls back to username when displayName is undefined', () => {
      setIncomingRingingState({ username: 'bob' });
      render(<IncomingCallBanner />);
      expect(screen.getByText('bob')).toBeInTheDocument();
    });
  });

  describe('group context (#1219 R10)', () => {
    it('shows the group name in the subtitle for a group incoming call', () => {
      seedGroupConversation('Squad');
      setIncomingRingingState({ displayName: 'Caller Name' }, true);
      render(<IncomingCallBanner />);
      expect(screen.getByText(/Squad/)).toBeInTheDocument();
      // The 1:1 subtitle wording must NOT appear for a group call.
      expect(screen.queryByText('Incoming voice call…')).not.toBeInTheDocument();
    });

    it('falls back to a generic group label when the group has no name', () => {
      seedGroupConversation(null);
      setIncomingRingingState({ displayName: 'Caller Name' }, true);
      render(<IncomingCallBanner />);
      expect(screen.getByText(/Group voice call/)).toBeInTheDocument();
    });

    it('keeps the 1:1 subtitle wording for a non-group incoming call', () => {
      setIncomingRingingState({ displayName: 'Caller Name' }, false);
      render(<IncomingCallBanner />);
      expect(screen.getByText('Incoming voice call…')).toBeInTheDocument();
    });
  });

  describe('button interactions', () => {
    it('invokes acceptIncomingCall when Accept is clicked', () => {
      setIncomingRingingState();
      render(<IncomingCallBanner />);
      fireEvent.click(screen.getByRole('button', { name: /accept call/i }));
      expect(mockAccept).toHaveBeenCalledTimes(1);
      expect(mockDecline).not.toHaveBeenCalled();
    });

    it('invokes declineIncomingCall when Decline is clicked', () => {
      setIncomingRingingState();
      render(<IncomingCallBanner />);
      fireEvent.click(screen.getByRole('button', { name: /decline call/i }));
      expect(mockDecline).toHaveBeenCalledTimes(1);
      expect(mockAccept).not.toHaveBeenCalled();
    });
  });
});
