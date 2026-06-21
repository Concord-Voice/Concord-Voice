// Tests for OutgoingCallModal — caller-side centered modal during the
// outgoing ring (#1209 plan task F2). Covers render-gating, peer-name
// resolution via the peerName helper, the missing-userId fallback,
// missing-conversation defensive render, and the Cancel button →
// callStateMachine dispatch.
//
// Filed per PR #1231 SonarCloud Quality Gate coverage gap.

import React from 'react';
import { render, screen, fireEvent } from '../../../test-utils';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/OutgoingCallModal.css', () => ({}));

// Mock returns a resolved Promise so the click-handler `.catch(...)` chain
// (added per Copilot #1231 C11) doesn't TypeError on undefined.
const mockCancel = vi.fn(() => Promise.resolve());
vi.mock('@/renderer/services/voiceService/callStateMachine', () => ({
  cancelOutgoingCall: () => mockCancel(),
}));

import { OutgoingCallModal } from '@/renderer/components/Voice/OutgoingCallModal';

const CONVERSATION_ID = '11111111-1111-1111-1111-111111111111';
const RING_ID = '22222222-2222-2222-2222-222222222222';
const MY_USER_ID = 'my-user-id';
const PEER_USER_ID = 'peer-user-id';

function seedOutgoing(): void {
  useUserStore.setState({
    user: { id: MY_USER_ID, username: 'me', email: 'me@example.com' } as never,
  });
  useDMStore.setState({
    conversations: [
      {
        id: CONVERSATION_ID,
        isGroup: false,
        isPersonal: false,
        name: null,
        participants: [
          { userId: MY_USER_ID, username: 'me' },
          { userId: PEER_USER_ID, username: 'peer', displayName: 'Peer Display' },
        ],
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2026-05-28T13:00:00.000Z',
      } as never,
    ],
  });
  useVoiceStore.getState().setCallState({
    kind: 'outgoing-ringing',
    conversationId: CONVERSATION_ID,
    ringId: RING_ID,
    calleeUserIds: [PEER_USER_ID],
    startedAt: 1000,
    declinedUserIds: [],
  });
}

describe('OutgoingCallModal', () => {
  beforeEach(() => {
    resetAllStores();
    mockCancel.mockReset();
  });

  describe('render gating', () => {
    it('returns null when callState is idle', () => {
      const { container } = render(<OutgoingCallModal />);
      expect(container.firstChild).toBeNull();
    });

    it('returns null when callState is incoming-ringing', () => {
      useVoiceStore.getState().setCallState({
        kind: 'incoming-ringing',
        conversationId: CONVERSATION_ID,
        ringId: RING_ID,
        caller: { userId: PEER_USER_ID, username: 'peer' },
        expiresAt: Date.now() + 30000,
      });
      const { container } = render(<OutgoingCallModal />);
      expect(container.firstChild).toBeNull();
    });

    it('returns null when callState is in-call', () => {
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
      const { container } = render(<OutgoingCallModal />);
      expect(container.firstChild).toBeNull();
    });

    it('returns null when conversation cannot be found (defensive)', () => {
      useUserStore.setState({
        user: { id: MY_USER_ID, username: 'me', email: 'me@example.com' } as never,
      });
      // Set outgoing-ringing state pointing at a conversation that's not in the
      // store (e.g., evicted mid-ring).
      useVoiceStore.getState().setCallState({
        kind: 'outgoing-ringing',
        conversationId: 'not-in-store',
        ringId: RING_ID,
        calleeUserIds: [PEER_USER_ID],
        startedAt: 1000,
        declinedUserIds: [],
      });
      const { container } = render(<OutgoingCallModal />);
      expect(container.firstChild).toBeNull();
    });

    it('renders modal when outgoing-ringing + conversation found', () => {
      seedOutgoing();
      render(<OutgoingCallModal />);
      expect(screen.getByRole('dialog')).toBeInTheDocument();
    });
  });

  describe('peer name resolution', () => {
    it('displays peer displayName when present', () => {
      seedOutgoing();
      render(<OutgoingCallModal />);
      expect(screen.getByText(/Calling Peer Display/)).toBeInTheDocument();
    });

    it('falls back to "Unknown" when myUserId is undefined', () => {
      // Seed conversation but no user
      useDMStore.setState({
        conversations: [
          {
            id: CONVERSATION_ID,
            isGroup: false,
            isPersonal: false,
            name: null,
            participants: [{ userId: PEER_USER_ID, username: 'peer' }],
            lastMessage: null,
            unreadCount: 0,
            createdAt: '2026-05-28T13:00:00.000Z',
          } as never,
        ],
      });
      useVoiceStore.getState().setCallState({
        kind: 'outgoing-ringing',
        conversationId: CONVERSATION_ID,
        ringId: RING_ID,
        calleeUserIds: [PEER_USER_ID],
        startedAt: 1000,
        declinedUserIds: [],
      });

      render(<OutgoingCallModal />);
      expect(screen.getByText(/Calling Unknown/)).toBeInTheDocument();
    });

    it('renders 2-char uppercased initials', () => {
      seedOutgoing();
      render(<OutgoingCallModal />);
      // 'Peer Display'.slice(0, 2).toUpperCase() === 'PE'
      expect(screen.getByText('PE')).toBeInTheDocument();
    });
  });

  describe('button interactions', () => {
    it('invokes cancelOutgoingCall when Cancel is clicked', () => {
      seedOutgoing();
      render(<OutgoingCallModal />);
      fireEvent.click(screen.getByRole('button', { name: /cancel call/i }));
      expect(mockCancel).toHaveBeenCalledTimes(1);
    });
  });

  // ── Group tally (#1219 R3) ──────────────────────────────────────────────

  const B_USER_ID = 'b-user-id';
  const C_USER_ID = 'c-user-id';
  const D_USER_ID = 'd-user-id';

  function seedGroupOutgoing(declinedUserIds: string[]): void {
    useUserStore.setState({
      user: { id: MY_USER_ID, username: 'me', email: 'me@example.com' } as never,
    });
    useDMStore.setState({
      conversations: [
        {
          id: CONVERSATION_ID,
          isGroup: true,
          isPersonal: false,
          name: 'Squad',
          participants: [
            { userId: MY_USER_ID, username: 'me' },
            { userId: B_USER_ID, username: 'bob', displayName: 'Bob' },
            { userId: C_USER_ID, username: 'charlie', displayName: 'Charlie' },
            { userId: D_USER_ID, username: 'diana', displayName: 'Diana' },
          ],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2026-05-28T13:00:00.000Z',
        } as never,
      ],
    });
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: [B_USER_ID, C_USER_ID, D_USER_ID],
      startedAt: 1000,
      declinedUserIds,
    });
  }

  describe('group tally', () => {
    it('renders the group name as the title (fallback "Group voice call" when unnamed)', () => {
      seedGroupOutgoing([]);
      render(<OutgoingCallModal />);
      expect(screen.getByText('Squad')).toBeInTheDocument();
      // 1:1 "Calling …" copy must NOT appear for a group.
      expect(screen.queryByText(/Calling /)).not.toBeInTheDocument();
    });

    it('falls back to "Group voice call" when the group has no name', () => {
      seedGroupOutgoing([]);
      useDMStore.setState((s) => ({
        conversations: s.conversations.map((c) => ({ ...c, name: null }) as never),
      }));
      render(<OutgoingCallModal />);
      expect(screen.getByText('Group voice call')).toBeInTheDocument();
    });

    it('lists ringing callees by display name and marks decliners', () => {
      seedGroupOutgoing([B_USER_ID]);
      render(<OutgoingCallModal />);
      // Charlie + Diana still ringing; Bob declined.
      expect(screen.getByText(/Charlie/)).toBeInTheDocument();
      expect(screen.getByText(/Diana/)).toBeInTheDocument();
      // Bob is rendered in a declined element (separate node).
      const declined = screen.getByText('Bob');
      expect(declined).toHaveClass('outgoing-call-modal__declinee');
    });

    it('resolves names locally from dmStore participants (no PII in WS payload)', () => {
      seedGroupOutgoing([]);
      render(<OutgoingCallModal />);
      // Names come from dmStore, not user IDs.
      expect(screen.queryByText(new RegExp(B_USER_ID))).not.toBeInTheDocument();
      expect(screen.getByText(/Bob/)).toBeInTheDocument();
    });
  });
});
