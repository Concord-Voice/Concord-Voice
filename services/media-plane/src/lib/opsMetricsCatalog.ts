import { createHmac } from 'node:crypto';

type MetricDefinition = Readonly<{
  source: 'media';
  min: number;
  max: number;
}>;

export const MEDIA_METRIC_DEFINITIONS = {
  media_rooms_current: { source: 'media', min: 0, max: 1e15 },
  media_participants_audio_current: { source: 'media', min: 0, max: 1e15 },
  media_participants_webcam_current: { source: 'media', min: 0, max: 1e15 },
  media_participants_screenshare_current: { source: 'media', min: 0, max: 1e15 },
  media_camera_publishers_current: { source: 'media', min: 0, max: 1e15 },
  media_screen_publishers_current: { source: 'media', min: 0, max: 1e15 },
  media_peak_video_publishers_per_room: { source: 'media', min: 0, max: 1e15 },
  media_egress_current_bps: { source: 'media', min: 0, max: 1e15 },
  media_egress_peak_bps: { source: 'media', min: 0, max: 1e15 },
  media_egress_cumulative_bytes: { source: 'media', min: 0, max: 1e18 },
  media_participant_hours_audio: { source: 'media', min: 0, max: 1e12 },
  media_participant_hours_webcam: { source: 'media', min: 0, max: 1e12 },
  media_participant_hours_screenshare: { source: 'media', min: 0, max: 1e12 },
} as const satisfies Record<string, MetricDefinition>;

export type MediaMetricKey = keyof typeof MEDIA_METRIC_DEFINITIONS;
export type MediaMetrics = Partial<Record<MediaMetricKey, number>>;

export type UnsignedOpsMetricsEnvelope = Readonly<{
  version: 1;
  source: 'media';
  node_id: string;
  observed_at: string;
  sequence: number;
  metrics: MediaMetrics;
}>;

export type OpsMetricsEnvelope = UnsignedOpsMetricsEnvelope & Readonly<{ signature: string }>;

const nodeIdPattern = /^cvn_[a-z2-7]{16}$/;

export function validateNodeId(nodeId: string): void {
  if (!nodeIdPattern.test(nodeId)) {
    throw new Error('Node ID must be an assigned cvn_ token with 16 lowercase base32 characters');
  }
}

function validateMetrics(
  metrics: Readonly<Record<string, number>>
): asserts metrics is MediaMetrics {
  const entries = Object.entries(metrics);
  if (entries.length === 0) throw new Error('At least one media metric is required');
  for (const [key, value] of entries) {
    const definition = MEDIA_METRIC_DEFINITIONS[key as MediaMetricKey];
    if (!definition) throw new Error(`Unknown metric key: ${key}`);
    if (!Number.isFinite(value)) throw new Error(`Metric ${key} must be finite`);
    if (value < definition.min || value > definition.max) {
      throw new Error(`Metric ${key} is outside its allowed range`);
    }
  }
}

export function canonicalEnvelopePayload(envelope: UnsignedOpsMetricsEnvelope): string {
  const lines = [
    String(envelope.version),
    envelope.source,
    envelope.node_id,
    envelope.observed_at,
    String(envelope.sequence),
  ];
  const metrics = Object.entries(envelope.metrics).sort(([left], [right]) =>
    left.localeCompare(right, 'en-US')
  );
  for (const [key, value] of metrics) {
    if (value === undefined) throw new Error(`Metric ${key} must be defined`);
    const bytes = Buffer.allocUnsafe(8);
    bytes.writeDoubleBE(value);
    lines.push(`${key}=${bytes.toString('hex')}`);
  }
  return `${lines.join('\n')}\n`;
}

export function buildSignedEnvelope(
  input: Readonly<{
    nodeId: string;
    observedAt: Date;
    sequence: number;
    secret: string;
    metrics: MediaMetrics;
  }>
): OpsMetricsEnvelope {
  validateNodeId(input.nodeId);
  if (!Number.isSafeInteger(input.sequence) || input.sequence <= 0) {
    throw new Error('Sequence must be a positive safe integer');
  }
  if (Buffer.byteLength(input.secret, 'utf8') < 32) {
    throw new Error('Snapshot signing secret must be at least 32 bytes');
  }
  validateMetrics(input.metrics);

  const unsigned: UnsignedOpsMetricsEnvelope = {
    version: 1,
    source: 'media',
    node_id: input.nodeId,
    observed_at: input.observedAt.toISOString(),
    sequence: input.sequence,
    metrics: { ...input.metrics },
  };
  return {
    ...unsigned,
    signature: createHmac('sha256', input.secret)
      .update(canonicalEnvelopePayload(unsigned))
      .digest('hex'),
  };
}
