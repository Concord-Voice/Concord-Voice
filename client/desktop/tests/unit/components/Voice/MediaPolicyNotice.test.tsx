// client/desktop/tests/unit/components/Voice/MediaPolicyNotice.test.tsx
import React from 'react';
import { describe, it, expect, beforeEach } from 'vitest';
import { render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import MediaPolicyNotice, {
  MEDIA_POLICY_NOTICE_COPY,
  mediaPolicyNoticeRowId,
} from '@/renderer/components/Voice/MediaPolicyNotice';

describe('MediaPolicyNotice (#2153 handoff §1a)', () => {
  beforeEach(() => resetAllStores());

  it('keeps its polite live region mounted while empty, with no rows', () => {
    render(<MediaPolicyNotice />);
    const region = screen.getByRole('status');
    expect(region.children).toHaveLength(0);
  });

  it('pins the fixed copy for every source (T4/T5/T19; A4)', () => {
    expect(MEDIA_POLICY_NOTICE_COPY).toEqual({
      mic: 'Your microphone was paused because it sent more data than this call allows. Leave and rejoin to use it again. If it happens again, you may be removed from voice for a while.',
      camera:
        'Your camera was paused because it sent more data than this call allows. Turn your camera off and on to try again. If it happens again, you may be removed from voice for a while.',
      screen:
        'Your screen share was paused because it sent more data than this call allows. Stop and restart the share to try again. If it happens again, you may be removed from voice for a while.',
      'screen-audio':
        'Your shared sound was paused because it sent more data than this call allows. Turn sharing sound off and on to try again. If it happens again, you may be removed from voice for a while.',
    });
  });

  it('renders one row per latched source, in source order, each with its own id', () => {
    useVoiceStore.setState({ mediaPolicyPaused: { screen: 's-1', mic: 'm-1' } });
    render(<MediaPolicyNotice />);
    const rows = screen.getByRole('status').children;
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveAttribute('id', mediaPolicyNoticeRowId('mic'));
    expect(rows[0]).toHaveTextContent(MEDIA_POLICY_NOTICE_COPY.mic);
    expect(rows[1]).toHaveAttribute('id', mediaPolicyNoticeRowId('screen'));
  });

  it('hides the glyph from assistive technology (the sentence carries the meaning)', () => {
    useVoiceStore.setState({ mediaPolicyPaused: { camera: 'c-1' } });
    render(<MediaPolicyNotice />);
    const svg = screen.getByRole('status').querySelector('svg');
    expect(svg).toHaveAttribute('aria-hidden', 'true');
  });
});
