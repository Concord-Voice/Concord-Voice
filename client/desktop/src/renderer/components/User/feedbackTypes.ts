import { type SystemInfo } from '../../services/systemInfoService';

/**
 * Shared feedback type contracts (#158 infra, consumed by #159 bug panel and
 * #160 feature panel).
 *
 * Extracted into their own module so the panels and the orchestrating
 * `FeedbackModal` can all import the contracts without a component→component
 * import cycle (FeedbackModal renders BugReportPanel; BugReportPanel needs
 * `FeedbackSubmission`). Type-only imports erase at compile time, but a
 * dedicated module keeps the dependency graph a clean tree and avoids any
 * lint pushback on circular relationships.
 */

export type FeedbackMode = 'bug' | 'feature';

/**
 * Diagnostics bundle attached to a bug report ONLY when the user checks
 * "Include diagnostic logs". Mirrors the `diagnostics` object the
 * control-plane `internal/feedback` handler accepts. Every field here is
 * non-PII by construction (the machine ID is truncated to 8 chars; logs are
 * PII-scrubbed at capture time by logBufferService).
 */
export interface FeedbackDiagnostics {
  appVersion: string;
  platform: string;
  machineIdPrefix: string;
  gpu?: SystemInfo['gpu'];
  display?: SystemInfo['display'];
  connectionPhase: string;
  logs: string;
}

export interface FeedbackSubmission {
  type: FeedbackMode;
  title: string;
  description: string;
  /**
   * Feature-only; the human-readable category label the user selected
   * (e.g. 'New Feature', 'Improvement to Existing Feature', 'UI/UX Change',
   * 'Performance', 'Other'). Omitted when no category is chosen. The
   * control-plane renders it verbatim as `**Category:** <value>`.
   */
  category?: string;
  /** Bug-only; populated when the user checks "Include diagnostic logs". */
  diagnostics?: FeedbackDiagnostics;
}
