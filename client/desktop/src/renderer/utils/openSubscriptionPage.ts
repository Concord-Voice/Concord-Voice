import { useSettingsNavStore } from '../stores/settingsNavStore';

/**
 * Best-effort deep-link hint for the (future) Subscription page (#1304). The
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
 * page. Fires the cross-section focus request consumed by SettingsPage's focus
 * effect (switch pane → focus control). Until #1304 lands the Subscription
 * subsection, the request lands on the Account pane — graceful, no error.
 *
 * `section` is a best-effort deep-link hint (#1304); it is accepted but unused
 * today. Do NOT invent a destination for it here — the navigation target is
 * always Account ▸ section-subscription.
 */
export function openSubscriptionPage(_section?: SubscriptionDeepLink): void {
  // `_section` is intentionally accepted-but-unused today (the leading `_`
  // satisfies no-unused-vars); it keeps the signature honest for #1304 callers,
  // which will route to the matching feature row once the destination ships.
  useSettingsNavStore.getState().requestFocus('account', 'section-subscription');
}
