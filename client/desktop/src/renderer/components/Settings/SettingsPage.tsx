import React, { useState, useEffect, useRef, useTransition, useCallback } from 'react';
import { useDraftSettingsLifecycle, useDraftActions } from '../../hooks/useDraftSettings';
import { useSettingsOverlayStore } from '../../stores/settingsOverlayStore';
import { useSettingsNavStore, type SettingsSection } from '../../stores/settingsNavStore';
import AppearanceSection from './AppearanceSection';
import PrivacySecuritySection from './PrivacySecuritySection';
import VoiceAudioSection from './VoiceAudioSection';
import AccessibilitySection from './AccessibilitySection';
import AccountSection from './AccountSection';
import NotificationSection from './NotificationSection';
import AboutUpdateSection from './AboutUpdateSection';
import ExpandCollapseAllButton from './ExpandCollapseAllButton';
import './SettingsPage.css';

// ─── Nav Items ──────────────────────────────────────────────────────────────

interface NavItem {
  id: SettingsSection;
  label: string;
  icon: React.ReactNode;
  enabled: boolean;
}

const navItems: NavItem[] = [
  {
    id: 'appearance',
    label: 'Appearance',
    enabled: true,
    icon: (
      <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
        <circle cx="9" cy="9" r="4" stroke="currentColor" strokeWidth="1.5" />
        <path
          d="M9 1v2M9 15v2M2.64 2.64l1.41 1.41M13.95 13.95l1.41 1.41M1 9h2M15 9h2M2.64 15.36l1.41-1.41M13.95 4.05l1.41-1.41"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    ),
  },
  {
    id: 'privacy',
    label: 'Privacy & Security',
    enabled: true,
    icon: (
      <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
        <rect x="3" y="8" width="12" height="8" rx="2" stroke="currentColor" strokeWidth="1.5" />
        <path
          d="M6 8V5a3 3 0 016 0v3"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    ),
  },
  {
    id: 'account',
    label: 'Account',
    enabled: true,
    icon: (
      <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
        <circle cx="9" cy="6" r="3" stroke="currentColor" strokeWidth="1.5" />
        <path
          d="M3 16c0-2.76 2.69-5 6-5s6 2.24 6 5"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    ),
  },
  {
    id: 'notifications',
    label: 'Sounds and Notifications',
    enabled: true,
    icon: (
      <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
        <path
          d="M13.73 12.73A15.5 15.5 0 0114.5 9V7.5a5.5 5.5 0 00-11 0V9c0 1.31.26 2.56.77 3.73L3 14h12l-1.27-1.27z"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <path
          d="M7 14v.5a2 2 0 004 0V14"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    ),
  },
  {
    id: 'voice',
    label: 'Audio & Video',
    enabled: true,
    icon: (
      <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
        {/* Headphone arc */}
        <path
          d="M3.5 9a5.5 5.5 0 0111 0"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
        {/* Left ear cup */}
        <rect x="2" y="9" width="3" height="5" rx="1.5" stroke="currentColor" strokeWidth="1.5" />
        {/* Right ear cup */}
        <rect x="13" y="9" width="3" height="5" rx="1.5" stroke="currentColor" strokeWidth="1.5" />
        {/* Camera lens dot centered in arc — signals video */}
        <circle cx="9" cy="6.5" r="1.5" fill="currentColor" />
      </svg>
    ),
  },
  {
    id: 'accessibility',
    label: 'Accessibility',
    enabled: true,
    icon: (
      <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
        <circle cx="9" cy="4" r="2" stroke="currentColor" strokeWidth="1.5" />
        <path
          d="M3 8l6 1 6-1M9 9v4M6 17l3-4 3 4"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    ),
  },
  {
    id: 'about',
    label: 'About & Updates',
    enabled: true,
    icon: (
      <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
        <circle cx="9" cy="9" r="7.5" stroke="currentColor" strokeWidth="1.5" />
        <path d="M9 8v4M9 6h.01" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
    ),
  },
];

const NAV_SUBSECTIONS: Record<string, { id: string; label: string }[]> = {
  appearance: [
    { id: 'color-scheme', label: 'Color Scheme' },
    { id: 'theme', label: 'Theme' },
  ],
  privacy: [
    { id: 'privacy-settings', label: 'Privacy' },
    { id: 'presence-settings', label: 'Custom Status' },
    { id: 'system-permissions', label: 'System Permissions' },
    { id: 'mfa', label: 'Multi-Factor Auth' },
    { id: 'active-sessions', label: 'Active Sessions' },
    { id: 'past-sessions', label: 'Past Sessions' },
  ],
  account: [{ id: 'nsfw-content', label: 'NSFW Content Access' }],
  notifications: [
    { id: 'desktop-notifications', label: 'Desktop Notifications' },
    { id: 'notification-sounds', label: 'Sounds' },
    { id: 'quiet-hours', label: 'Quiet Hours' },
  ],
  voice: [
    { id: 'device-config', label: 'Device Configuration' },
    { id: 'audio-config', label: 'Audio Configuration' },
    { id: 'video-screen', label: 'Video Configuration' },
  ],
  accessibility: [
    { id: 'display', label: 'Display' },
    { id: 'tts', label: 'Text-to-Speech' },
  ],
  about: [
    { id: 'client-info', label: 'Client Info' },
    { id: 'update-settings', label: 'Update Settings' },
  ],
};

// ─── Settings Page ──────────────────────────────────────────────────────────

const SettingsPage: React.FC = () => {
  const closeOverlay = useSettingsOverlayStore((s) => s.close);
  const [activeSection, setActiveSection] = useState<SettingsSection>('appearance');
  const [isPending, startTransition] = useTransition();
  const [activeSubsection, setActiveSubsection] = useState<string | null>(null);
  const contentRef = useRef<HTMLDivElement>(null);

  // Draft settings lifecycle
  useDraftSettingsLifecycle();
  const { apply, revert, hasPendingChanges, hwAccelChanged } = useDraftActions();

  // Blob animation state
  const [blobBouncing, setBlobBouncing] = useState(false);
  const [blobFading, setBlobFading] = useState(false);

  // IntersectionObserver: track which section is in view and highlight tree nav
  // We keep a persistent map of all currently-intersecting sections so the callback
  // (which only receives *changed* entries) can always pick the topmost visible one.
  const visibleSectionsRef = useRef<Map<string, IntersectionObserverEntry>>(new Map());

  useEffect(() => {
    visibleSectionsRef.current.clear();
    const root = contentRef.current?.closest('.settings-page-content') as HTMLElement | null;
    if (!root) return;

    // Created inside the post-render delay below; held in this effect-scoped
    // variable so the cleanup disconnects it directly (no DOM expando — #484).
    let observer: IntersectionObserver | null = null;

    // Small delay to let sections render after tab switch
    const timer = setTimeout(() => {
      observer = new IntersectionObserver(
        (entries) => {
          for (const entry of entries) {
            const id = entry.target.id;
            if (entry.isIntersecting) {
              visibleSectionsRef.current.set(id, entry);
            } else {
              visibleSectionsRef.current.delete(id);
            }
          }

          // Pick the topmost visible section (smallest boundingClientRect.top)
          let best: IntersectionObserverEntry | null = null;
          for (const entry of visibleSectionsRef.current.values()) {
            if (!best || entry.boundingClientRect.top < best.boundingClientRect.top) {
              best = entry;
            }
          }
          if (best) {
            setActiveSubsection(best.target.id.replace('section-', ''));
          }
        },
        { root, threshold: [0, 0.1, 0.25, 0.5], rootMargin: '-10% 0px -50% 0px' }
      );

      const sections = root.querySelectorAll('[id^="section-"]');
      for (const el of sections) observer.observe(el);
    }, 50);

    return () => {
      clearTimeout(timer);
      observer?.disconnect();
    };
  }, [activeSection]);

  const scrollToSection = useCallback((sectionId: string) => {
    const el = document.getElementById(`section-${sectionId}`);
    if (!el) return;
    // Auto-expand collapsed sections when navigating via sidebar
    if (el instanceof HTMLDetailsElement && !el.open) {
      el.open = true;
    }
    el.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, []);

  // Cross-section focus (#1644): the locked Appearance font picker's back-link
  // requests focus on the Accessibility dyslexic toggle. Switch to the target
  // pane, then focus the control once it mounts (deferred past the React commit,
  // matching the 50ms the IntersectionObserver effect uses for the same reason).
  const focusRequest = useSettingsNavStore((s) => s.focusRequest);
  const clearFocusRequest = useSettingsNavStore((s) => s.clearFocusRequest);

  useEffect(() => {
    if (!focusRequest) return;
    if (focusRequest.section !== activeSection) {
      startTransition(() => setActiveSection(focusRequest.section));
      return; // re-runs on activeSection change once the pane mounts
    }
    const timer = setTimeout(() => {
      const el = document.getElementById(focusRequest.controlId);
      if (el) {
        const details = el.closest('details');
        if (details instanceof HTMLDetailsElement && !details.open) details.open = true;
        el.scrollIntoView({ behavior: 'smooth', block: 'center' });
        el.focus();
      }
      clearFocusRequest();
    }, 50);
    return () => clearTimeout(timer);
  }, [focusRequest, activeSection, clearFocusRequest]);

  // Drop ANY pending cross-section focus request when Settings unmounts (closes) —
  // unconditionally, regardless of which branch the focus effect above was in
  // (the pane-switch phase, OR the deferred-timer phase). The nav store outlives
  // this component, so a request still outstanding at unmount is stale and would
  // otherwise re-fire as a spurious pane-jump on the next open. (#1644 review, 2nd pass.)
  useEffect(() => {
    return () => {
      useSettingsNavStore.getState().clearFocusRequest();
    };
  }, []);

  const renderSection = () => {
    switch (activeSection) {
      case 'appearance':
        return <AppearanceSection />;
      case 'privacy':
        return <PrivacySecuritySection />;
      case 'account':
        return <AccountSection />;
      case 'notifications':
        return <NotificationSection />;
      case 'voice':
        return <VoiceAudioSection />;
      case 'accessibility':
        return <AccessibilitySection />;
      case 'about':
        return <AboutUpdateSection />;
      default:
        return null;
    }
  };

  return (
    <div className="view-container settings-fullpage">
      <div className="settings-page-content">
        <div className="settings-page-inner">
          <div className="settings-layout">
            {/* Left sidebar: back button, title, and navigation */}
            <nav className="settings-nav">
              <div className="settings-nav-scroll">
                <button
                  className={`settings-back-btn${hasPendingChanges ? ' settings-back-btn--blocked' : ''}`}
                  onClick={() => {
                    if (!hasPendingChanges) closeOverlay();
                  }}
                  onMouseEnter={() => {
                    if (hasPendingChanges) {
                      setBlobBouncing(true);
                    }
                  }}
                  onMouseLeave={() => {
                    if (hasPendingChanges) {
                      setBlobBouncing(false);
                      setBlobFading(true);
                      setTimeout(() => setBlobFading(false), 3000);
                    }
                  }}
                >
                  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                    <path
                      d="M10 12L6 8l4-4"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                  Back to app
                </button>

                <h1 className="settings-page-title">Settings</h1>

                {navItems.map((item) => {
                  const isActive = activeSection === item.id;
                  const subs = isActive ? NAV_SUBSECTIONS[item.id] : undefined;

                  return (
                    <div key={item.id} className="settings-nav-group">
                      <button
                        className={`settings-nav-item ${isActive ? 'active' : ''}`}
                        onClick={() =>
                          item.enabled && startTransition(() => setActiveSection(item.id))
                        }
                        disabled={!item.enabled}
                      >
                        <span className="settings-nav-item-icon">{item.icon}</span>
                        {item.label}
                        {!item.enabled && <span className="settings-nav-badge">Soon</span>}
                      </button>

                      {subs && subs.length > 0 && (
                        <div className="settings-nav-tree">
                          {subs.map((sub, i) => (
                            <div
                              key={sub.id}
                              className={`settings-nav-tree-item-wrapper${i === subs.length - 1 ? ' settings-nav-tree-item-wrapper--last' : ''}`}
                            >
                              <button
                                className={`settings-nav-tree-item${activeSubsection === sub.id ? ' active' : ''}`}
                                onClick={() => scrollToSection(sub.id)}
                              >
                                {sub.label}
                              </button>
                            </div>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>

              {/* Pending changes blob */}
              {hasPendingChanges && (
                <div
                  className={`settings-pending-blob${
                    blobBouncing ? ' settings-pending-blob--bouncing' : ''
                  }${blobFading ? ' settings-pending-blob--fading' : ''}`}
                >
                  <p className="settings-pending-blob-text">
                    {hwAccelChanged
                      ? 'Settings have changed! Applying will restart the app.'
                      : 'Settings have changed! Would you like to apply?'}
                  </p>
                  <div className="settings-pending-blob-actions">
                    <button className="settings-pending-blob-apply" onClick={apply}>
                      {hwAccelChanged ? 'Apply & Restart' : 'Apply'}
                    </button>
                    <button className="settings-pending-blob-revert" onClick={revert}>
                      Revert
                    </button>
                  </div>
                </div>
              )}
            </nav>

            {/* Right content */}
            <div
              ref={contentRef}
              className={`settings-content${isPending ? ' settings-content-loading' : ''}`}
            >
              {/* Expand/Collapse All control (closes #297). Self-hides when
                  the active panel has no collapsible sections, so this row
                  collapses to zero visual cost on flat-layout future tabs. */}
              <div className="settings-content-header">
                <ExpandCollapseAllButton containerRef={contentRef} />
              </div>
              {renderSection()}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
};

export default SettingsPage;
