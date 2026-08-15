/**
 * The closed range token set accepted by the purge endpoints.
 * Mirrors services/control-plane/internal/purge/rangeutil.go — the server
 * rejects anything outside this set, and `range` is a required request field.
 */
export const PURGE_RANGES = ['1h', '6h', '12h', '1d', '7d', '15d', '30d', '90d', 'all'] as const;

export type PurgeRange = (typeof PURGE_RANGES)[number];

/** Option labels. Copy deck §3. */
export const PURGE_RANGE_LABELS: Record<PurgeRange, string> = {
  '1h': 'Last hour',
  '6h': 'Last 6 hours',
  '12h': 'Last 12 hours',
  '1d': 'Last 24 hours',
  '7d': 'Last 7 days',
  '15d': 'Last 15 days',
  '30d': 'Last 30 days',
  '90d': 'Last 90 days',
  all: 'All messages',
};

/** Mid-sentence phrase for the scope echo. Copy deck §2. */
export const PURGE_RANGE_PHRASES: Record<PurgeRange, string> = {
  '1h': 'all messages from the last hour',
  '6h': 'all messages from the last 6 hours',
  '12h': 'all messages from the last 12 hours',
  '1d': 'all messages from the last 24 hours',
  '7d': 'all messages from the last 7 days',
  '15d': 'all messages from the last 15 days',
  '30d': 'all messages from the last 30 days',
  '90d': 'all messages from the last 90 days',
  all: 'all messages, with no time limit',
};
