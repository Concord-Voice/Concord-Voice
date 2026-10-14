import type { SettingsSection } from '../../stores/ui/settingsNavStore';

export interface SettingsSubsection {
  id: string;
  label: string;
  targetId: string;
  focusControlId?: string;
}

/** Explicit DOM destinations keep navigation aligned with the rendered disclosures. */
export const NAV_SUBSECTIONS: Record<SettingsSection, readonly SettingsSubsection[]> = {
  appearance: [
    { id: 'color-scheme', label: 'Color Scheme', targetId: 'section-color-scheme' },
    { id: 'theme', label: 'Theme', targetId: 'section-theme' },
    { id: 'fonts', label: 'Application Font', targetId: 'section-fonts' },
    { id: 'layout', label: 'Layout', targetId: 'section-layout' },
    { id: 'client-behavior', label: 'Client Behavior', targetId: 'client-behavior' },
  ],
  privacy: [
    { id: 'privacy-settings', label: 'Privacy Controls', targetId: 'section-privacy-settings' },
    { id: 'presence-settings', label: 'Rich Presence', targetId: 'section-presence-settings' },
    {
      id: 'presence-history',
      label: 'Activity History',
      targetId: 'section-presence-history',
    },
  ],
  account: [
    { id: 'profile', label: 'Profile Information', targetId: 'section-profile' },
    { id: 'subscription', label: 'Subscription', targetId: 'section-subscription' },
    { id: 'nsfw-content', label: 'NSFW Content Access', targetId: 'section-nsfw-content' },
  ],
  security: [
    { id: 'password', label: 'Change Password', targetId: 'section-password' },
    { id: 'mfa', label: 'Multi-Factor Authentication', targetId: 'section-mfa' },
    { id: 'sso-security', label: 'SSO Security', targetId: 'section-sso-security' },
    { id: 'active-sessions', label: 'Active Sessions', targetId: 'section-active-sessions' },
    { id: 'past-sessions', label: 'Past Sessions', targetId: 'section-past-sessions' },
  ],
  notifications: [
    {
      id: 'desktop-notifications',
      label: 'Desktop Notifications',
      targetId: 'section-desktop-notifications',
    },
    { id: 'notification-sounds', label: 'Sounds', targetId: 'section-notification-sounds' },
    { id: 'quiet-hours', label: 'Quiet Hours', targetId: 'section-quiet-hours' },
  ],
  voice: [
    { id: 'audio-config', label: 'Audio Configuration', targetId: 'section-audio-config' },
    { id: 'video-screen', label: 'Video Configuration', targetId: 'section-video-screen' },
  ],
  accessibility: [
    { id: 'display', label: 'Display', targetId: 'section-display' },
    { id: 'tts', label: 'Text-to-Speech', targetId: 'section-tts' },
  ],
  about: [
    { id: 'client-info', label: 'Client Info', targetId: 'section-client-info' },
    { id: 'update-settings', label: 'Update Settings', targetId: 'section-update-settings' },
    {
      id: 'system-permissions',
      label: 'System Permissions',
      targetId: 'section-system-permissions',
    },
    { id: 'developer', label: 'Developer', targetId: 'section-developer' },
  ],
};
