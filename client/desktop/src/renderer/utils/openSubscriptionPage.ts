import { useSettingsNavStore } from '../stores/settingsNavStore';
import { useSettingsOverlayStore } from '../stores/settingsOverlayStore';

/**
 * Best-effort deep-link hint for the Subscription page (#1304). The
 * lock variants pass which surface the user came from so #1304 can scroll to
 * the matching feature row. #1301 only NAVIGATES — the hint is accepted and
 * ignored gracefully until #1304 ships the destination subsection.
 */
export type SubscriptionDeepLink =
  | 'audio-tier'
  | 'video-quality'
  | 'music-mode'
  | 'custom-scheme'
  | 'manual-bitrate'
  | 'native-caps'
  | 'message-length'
  | 'username-cadence'
  | 'upload-size';

/**
 * The single navigation route from every lock affordance to the Subscription
 * page. Opens app Settings, then fires the cross-section focus request consumed
 * by SettingsPage's focus effect (switch pane → focus control).
 *
 * `section` is a best-effort deep-link hint (#1304); it is accepted but unused
 * today. The navigation target is always Subscriptions ▸ Current Plan.
 */
export function openSubscriptionPage(_section?: SubscriptionDeepLink): void {
  // `_section` is intentionally accepted-but-unused today (the leading `_`
  // satisfies no-unused-vars); it keeps the signature honest for #1304 callers,
  // which will route to the matching feature row once the destination ships.
  useSettingsOverlayStore.getState().openSettings('app');
  useSettingsNavStore.getState().requestFocus('subscriptions', 'section-current-plan');
}
