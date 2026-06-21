// Tests for CallEventMessage — the renderer for dm_messages rows where
// type='call_event' (#1209 plan task F6 component half). The component is
// currently not wired into MessageList (deferred until backend serializer
// + Message interface extension); this file covers the component in
// isolation per its props contract.
//
// Filed per PR #1231 SonarCloud Quality Gate coverage gap.

import React from 'react';
import { render, screen } from '../../../test-utils';
import {
  CallEventMessage,
  type CallEventPayload,
  type CallEventStatus,
} from '@/renderer/components/DirectMessages/CallEventMessage';

vi.mock('@/renderer/components/DirectMessages/CallEventMessage.css', () => ({}));

function basePayload(overrides: Partial<CallEventPayload> = {}): CallEventPayload {
  return {
    // 04:37 UTC → won't format to any duration value tested here (avoids
    // the CI-vs-local timezone collision where '13:00Z' renders to '1:00 PM'
    // and clashes with the `1:00` duration assertion).
    started_at: '2026-05-28T04:37:00.000Z',
    status: 'completed',
    duration_seconds: 90,
    ...overrides,
  };
}

describe('CallEventMessage', () => {
  describe('status variants', () => {
    it.each([
      ['completed' as CallEventStatus, /Voice call/, /1:30/],
      ['missed' as CallEventStatus, /Missed voice call/, null],
      ['declined' as CallEventStatus, /Voice call declined/, null],
      ['canceled' as CallEventStatus, /Voice call canceled/, null],
    ])('renders %s status with the expected label', (status, labelPattern, durationPattern) => {
      render(<CallEventMessage payload={basePayload({ status })} />);
      // The label and the formatted duration share the same span for the
      // 'completed' status, separated by an em-dash inside the same text node.
      const messageText = screen.getByText(labelPattern);
      expect(messageText).toBeInTheDocument();
      if (durationPattern) {
        // Confirm the formatted duration appears in the same text node.
        expect(messageText.textContent).toMatch(durationPattern);
      }
    });
  });

  describe('formatDuration (exercised via completed-status rendering)', () => {
    it('renders 0:00 for zero duration', () => {
      render(
        <CallEventMessage payload={basePayload({ status: 'completed', duration_seconds: 0 })} />
      );
      expect(screen.getByText(/0:00/)).toBeInTheDocument();
    });

    it('renders 0:00 for negative duration (defensive)', () => {
      render(
        <CallEventMessage payload={basePayload({ status: 'completed', duration_seconds: -5 })} />
      );
      expect(screen.getByText(/0:00/)).toBeInTheDocument();
    });

    it('renders M:SS with padded seconds for sub-minute', () => {
      render(
        <CallEventMessage payload={basePayload({ status: 'completed', duration_seconds: 5 })} />
      );
      expect(screen.getByText(/0:05/)).toBeInTheDocument();
    });

    it('renders M:SS for exactly one minute', () => {
      render(
        <CallEventMessage payload={basePayload({ status: 'completed', duration_seconds: 60 })} />
      );
      expect(screen.getByText(/1:00/)).toBeInTheDocument();
    });

    it('renders M:SS for an hour-plus call', () => {
      // No h:mm:ss split in the component — formatDuration uses pure minutes
      render(
        <CallEventMessage payload={basePayload({ status: 'completed', duration_seconds: 3661 })} />
      );
      // 3661s → 61m 01s
      expect(screen.getByText(/61:01/)).toBeInTheDocument();
    });
  });

  describe('formatTime', () => {
    it('renders a formatted time for valid ISO timestamps', () => {
      const { container } = render(
        <CallEventMessage payload={basePayload({ started_at: '2026-05-28T13:45:00.000Z' })} />
      );
      const time = container.querySelector('.call-event-message__time');
      expect(time).not.toBeNull();
      // Avoid asserting the exact string because locale formatting varies;
      // verify it produced *something* matching a time-like pattern.
      expect((time as HTMLElement).textContent).toMatch(/\d/);
    });

    it('renders empty string when started_at is invalid', () => {
      const { container } = render(
        <CallEventMessage payload={basePayload({ started_at: 'not-a-real-date' })} />
      );
      const time = container.querySelector('.call-event-message__time');
      // formatTime now validates via isNaN(date.getTime()) and returns ''
      // for invalid timestamps (was previously try/catch on a non-throwing
      // ctor — Copilot #1231 cycle-1 finding C12). Assert empty so the
      // contract is locked.
      expect(time).not.toBeNull();
      expect((time as HTMLElement).textContent).toBe('');
    });
  });

  describe('group format (#1219)', () => {
    it('renders group format with the duration for a completed group call', () => {
      render(
        <CallEventMessage
          isGroup
          payload={basePayload({
            status: 'completed',
            duration_seconds: 323,
            participant_user_ids: ['a', 'b', 'c'],
          })}
        />
      );
      expect(screen.getByText(/Group voice call — 5:23/)).toBeInTheDocument();
    });

    it('exposes a joiner tooltip from participant_user_ids', () => {
      const { container } = render(
        <CallEventMessage
          isGroup
          payload={basePayload({
            status: 'completed',
            duration_seconds: 60,
            participant_user_ids: ['a', 'b', 'c'],
          })}
        />
      );
      const text = container.querySelector('.call-event-message__text') as HTMLElement;
      expect(text).not.toBeNull();
      // Tooltip is sourced from participant_user_ids, NOT caller_user_id (spec C2).
      expect(text.getAttribute('title')).toBe('a, b, c');
    });

    it('keeps 1:1 wording when isGroup is false', () => {
      render(
        <CallEventMessage payload={basePayload({ status: 'completed', duration_seconds: 90 })} />
      );
      expect(screen.getByText(/Voice call — 1:30/)).toBeInTheDocument();
      expect(screen.queryByText(/Group voice call/)).not.toBeInTheDocument();
    });
  });

  describe('accessibility', () => {
    it('exposes role="listitem" on the root', () => {
      render(<CallEventMessage payload={basePayload()} />);
      expect(screen.getByRole('listitem')).toBeInTheDocument();
    });
  });
});
