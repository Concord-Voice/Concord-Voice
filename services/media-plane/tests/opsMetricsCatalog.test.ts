import { createHmac } from 'node:crypto';
import { describe, expect, it } from 'vitest';

import {
  MEDIA_METRIC_DEFINITIONS,
  buildSignedEnvelope,
  canonicalEnvelopePayload,
  validateNodeId,
} from '../src/lib/opsMetricsCatalog.js';

const secret = '0123456789abcdef0123456789abcdef'; // pragma: allowlist secret
const observedAt = new Date('2026-07-12T20:00:00.123Z');

describe('media operations metric catalog', () => {
  it('contains only closed numeric scalar definitions', () => {
    expect(Object.keys(MEDIA_METRIC_DEFINITIONS).length).toBeGreaterThan(0);
    for (const [key, definition] of Object.entries(MEDIA_METRIC_DEFINITIONS)) {
      expect(key).not.toMatch(
        /(^|_)(user|email|username|display_name|content|message_id|ip|url|host_name|server_id|channel_id|room_id)($|_)/i
      );
      expect(definition.source).toBe('media');
      expect(definition.min).toBeLessThanOrEqual(definition.max);
    }
  });

  it('rejects hostnames, IPs, UUIDs, and non-opaque node IDs', () => {
    expect(() => validateNodeId('cvn_aaaaaaaaaaaaaaaa')).not.toThrow();
    for (const value of [
      'node-a7',
      'media.concordvoice.chat',
      '10.0.0.3',
      'd7781e5d-e353-46aa-afe2-3ca49f13332a',
      'CVN_AAAAAAAAAAAAAAAA',
    ]) {
      expect(() => validateNodeId(value)).toThrow(/node id/i);
    }
  });

  it('canonicalizes metrics independently of insertion order', () => {
    const first = canonicalEnvelopePayload({
      version: 1,
      source: 'media',
      node_id: 'cvn_aaaaaaaaaaaaaaaa',
      observed_at: observedAt.toISOString(),
      sequence: 9,
      metrics: {
        media_rooms_current: 2,
        media_egress_current_bps: 100,
      },
    });
    const second = canonicalEnvelopePayload({
      version: 1,
      source: 'media',
      node_id: 'cvn_aaaaaaaaaaaaaaaa',
      observed_at: observedAt.toISOString(),
      sequence: 9,
      metrics: {
        media_egress_current_bps: 100,
        media_rooms_current: 2,
      },
    });
    expect(first).toBe(second);
    expect(first).toContain('media_egress_current_bps=4059000000000000');
    expect(first).toContain('media_rooms_current=4000000000000000');
  });

  it('rejects undefined metric values during canonicalization', () => {
    expect(() =>
      canonicalEnvelopePayload({
        version: 1,
        source: 'media',
        node_id: 'cvn_aaaaaaaaaaaaaaaa',
        observed_at: observedAt.toISOString(),
        sequence: 9,
        metrics: { media_rooms_current: undefined } as never,
      })
    ).toThrow(/defined/i);
  });

  it('builds an HMAC signed closed envelope', () => {
    const envelope = buildSignedEnvelope({
      nodeId: 'cvn_aaaaaaaaaaaaaaaa',
      observedAt,
      sequence: 9,
      secret,
      metrics: {
        media_rooms_current: 2,
        media_egress_current_bps: 100,
      },
    });

    const { signature: _signature, ...unsigned } = envelope;
    const expected = createHmac('sha256', secret)
      .update(canonicalEnvelopePayload(unsigned))
      .digest('hex');
    expect(envelope.signature).toBe(expected);
    expect(envelope.signature).toHaveLength(64);
  });

  it('rejects unknown, non-finite, and out-of-range metric values', () => {
    const base = {
      nodeId: 'cvn_aaaaaaaaaaaaaaaa',
      observedAt,
      sequence: 9,
      secret,
    };

    expect(() => buildSignedEnvelope({ ...base, metrics: { unknown_metric: 1 } as never })).toThrow(
      /unknown metric/i
    );
    expect(() =>
      buildSignedEnvelope({ ...base, metrics: { media_rooms_current: Number.NaN } })
    ).toThrow(/finite/i);
    expect(() => buildSignedEnvelope({ ...base, metrics: { media_rooms_current: -1 } })).toThrow(
      /range/i
    );
  });
});
