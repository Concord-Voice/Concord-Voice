import React, { useState, useEffect, useRef, useTransition, useMemo } from 'react';
import { useDraftSettingsLifecycle, useDraftActions } from '../../hooks/ui/useDraftSettings';
import { useSectionObserver } from '../../hooks/ui/useSectionObserver';
import { useSettingsOverlayStore } from '../../stores/ui/settingsOverlayStore';
import { useSettingsNavStore, type SettingsSection } from '../../stores/ui/settingsNavStore';
import { useClientConfigStore } from '../../stores/ui/clientConfigStore';
import AppearanceSection from './AppearanceSection';
import PrivacySection from './PrivacySection';
import SecuritySection from './SecuritySection';
import VoiceAudioSection from './VoiceAudioSection';
import AccessibilitySection from './AccessibilitySection';
import AccountSection from './AccountSection';
import { NAV_SUBSECTIONS } from './settingsSections';
import { useSettingsStore } from '../../stores/ui/settingsStore';
import { useSettingsCollapsibleStore } from '../../stores/ui/settingsCollapsibleStore';
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
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
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
    label: 'Privacy',
    enabled: true,
    icon: (
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
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
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
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
    id: 'security',
    label: 'Security',
    enabled: true,
    icon: (
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
        <path
          d="M9 1.5 15 4v4.5c0 4.1-6 7.5-6 7.5s-6-3.4-6-7.5V4l6-2.5Z"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinejoin="round"
        />
        <circle cx="9" cy="7.5" r="1.5" stroke="currentColor" strokeWidth="1.5" />
        <path d="M9 9v3" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
    ),
  },
  {
    id: 'notifications',
    label: 'Notifications',
    enabled: true,
    icon: (
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
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
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
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
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
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
      <svg aria-hidden="true" width="18" height="18" viewBox="0 0 18 18" fill="none">
        <circle cx="9" cy="9" r="7.5" stroke="currentColor" strokeWidth="1.5" />
        <path d="M9 8v4M9 6h.01" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
    ),
  },
];

// ─── Settings Page ──────────────────────────────────────────────────────────

const SettingsPage: React.FC = () => {
  const closeOverlay = useSettingsOverlayStore((s) => s.close);
  const activityHistoryCapability = useClientConfigStore(
    (state) => state.activityHistoryCapability
  );
  // Unsupported activation can still expose retained history during a drain.
  // Offer its navigation once the pane confirms a real destination.
  const [activityHistoryDestinationAvailable, setActivityHistoryDestinationAvailable] =
    useState(false);
  const hiddenSubsectionId =
    activityHistoryCapability.status === 'confirmed-unsupported' &&
    !activityHistoryDestinationAvailable
      ? 'presence-history'
      : null;
  const [activeSection, setActiveSection] = useState<SettingsSection>('appearance');
  const [isPending, startTransition] = useTransition();
  const [activeSubsection, setActiveSubsection] = useState<string | null>(null);
  const contentRef = useRef<HTMLDivElement>(null);
  const reduceAnimations = useSettingsStore((state) => state.appearance.reduceAnimations);
  const requestFocus = useSettingsNavStore((state) => state.requestFocus);
  const observedIds = useMemo(
    () =>
      NAV_SUBSECTIONS[activeSection]
        .filter((sub) => sub.id !== hiddenSubsectionId)
        .map((sub) => sub.targetId),
    [activeSection, hiddenSubsectionId]
  );

  // Draft settings lifecycle
  useDraftSettingsLifecycle();
  const {
    apply,
    revert,
    hasPendingChanges,
    hwAccelChanged,
    contentProtectionApplying,
    contentProtectionApplyFailed,
  } = useDraftActions();

  // Blob animation state
  const [blobBouncing, setBlobBouncing] = useState(false);
  const [blobFading, setBlobFading] = useState(false);

  // Scroll-spy: track which section is in view and highlight tree nav (#484).
  useSectionObserver(contentRef, activeSection, setActiveSubsection, observedIds);

  // Cross-section focus (#1644): the locked Appearance font picker's back-link
  // requests focus on the Accessibility dyslexic toggle. Switch to the target
  // pane, then focus the control once it mounts. The first attempt waits for the
  // React commit; conditional targets can arrive later with their server response.
  const focusRequest = useSettingsNavStore((s) => s.focusRequest);
  const clearFocusRequest = useSettingsNavStore((s) => s.clearFocusRequest);

  useEffect(() => {
    if (!focusRequest) return;
    if (focusRequest.section !== activeSection) {
      startTransition(() => setActiveSection(focusRequest.section));
      return; // re-runs on activeSection change once the pane mounts
    }
    const root = contentRef.current;
    let targetObserver: MutationObserver | null = null;
    const focusWhenReady = () => {
      // A later link or explicit pane selection owns navigation now.
      if (useSettingsNavStore.getState().focusRequest !== focusRequest) return true;
      const el = document.getElementById(focusRequest.controlId);
      if (el && root?.contains(el) && !el.closest('[hidden]')) {
        const focusTarget =
          el instanceof HTMLDetailsElement
            ? (el.querySelector<HTMLElement>(':scope > summary') ?? el)
            : el;
        for (
          let ancestor: HTMLElement | null = focusTarget;
          ancestor;
          ancestor = ancestor.parentElement
        ) {
          if (ancestor instanceof HTMLDetailsElement) {
            useSettingsCollapsibleStore.getState().setSectionOpen(ancestor.id, true);
            ancestor.open = true;
          }
        }
        const subsection = NAV_SUBSECTIONS[activeSection].find(
          (sub) => sub.targetId === focusTarget.closest('.settings-collapsible')?.id
        );
        if (subsection) setActiveSubsection(subsection.id);
        const behavior: ScrollBehavior =
          reduceAnimations || globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches
            ? 'auto'
            : 'smooth';
        focusTarget.scrollIntoView({ behavior, block: 'center' });
        focusTarget.focus({ preventScroll: true });
        clearFocusRequest();
        return true;
      }
      return false;
    };
    const timer = setTimeout(() => {
      if (focusWhenReady() || !root) return;
      targetObserver = new MutationObserver(() => {
        if (focusWhenReady()) targetObserver?.disconnect();
      });
      targetObserver.observe(root, {
        childList: true,
        subtree: true,
        attributes: true,
        attributeFilter: ['hidden'],
      });
    }, 50);
    return () => {
      clearTimeout(timer);
      targetObserver?.disconnect();
    };
  }, [focusRequest, activeSection, clearFocusRequest, reduceAnimations]);

  // Drop ANY pending cross-section focus request when Settings unmounts (closes) —
  // unconditionally, regardless of which branch the focus effect above was in
  // (the pane-switch phase, OR the deferred-timer phase). The nav store outlives
  // this component, so a request still outstanding at unmount is stale and would
  // otherwise re-fire as a spurious pane-jump on the next open. (#1644 review, 2nd pass.)
  //
  // Only a REAL unmount may clear it (#2365). React.StrictMode (main.tsx) replays every
  // effect on mount in development — cleanup, then setup again — without touching the
  // DOM, and a clear that ran inside that replay dropped a request made BEFORE the page
  // mounted (`openProfilePage`, the purge modal's privacy link) before the focus effect
  // could act on it: the pane switched and the section never opened. A real unmount has
  // already removed this page's DOM by the time passive cleanups run, so the content
  // node's `isConnected` tells the two apart synchronously; the node is captured at setup
  // because React detaches refs before the cleanup runs on both paths.
  useEffect(() => {
    const node = contentRef.current;
    return () => {
      if (node?.isConnected) return; // StrictMode's simulated unmount: still mounted
      useSettingsNavStore.getState().clearFocusRequest();
    };
  }, []);

  const renderSection = () => {
    switch (activeSection) {
      case 'appearance':
        return <AppearanceSection />;
      case 'privacy':
        return (
          <PrivacySection
            onActivityHistoryAvailabilityChange={setActivityHistoryDestinationAvailable}
          />
        );
      case 'account':
        return <AccountSection />;
      case 'security':
        return <SecuritySection />;
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
            <nav className="settings-nav" aria-label="Settings">
              <div className="settings-nav-scroll">
                <button
                  className={`settings-back-btn${hasPendingChanges ? ' settings-back-btn--blocked' : ''}`}
                  onClick={() => {
                    if (!hasPendingChanges) closeOverlay();
                  }}
                  disabled={contentProtectionApplying}
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
                  <svg aria-hidden="true" width="16" height="16" viewBox="0 0 16 16" fill="none">
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

                <ul className="settings-nav-list">
                  {navItems.map((item) => {
                    const isActive = activeSection === item.id;
                    // Render every pane's sub-tree so sub-sections (e.g. System
                    // Permissions) are reachable from the top-level nav (#1743).
                    const subs = NAV_SUBSECTIONS[item.id]?.filter(
                      (sub) => sub.id !== hiddenSubsectionId
                    );

                    return (
                      <li key={item.id} className="settings-nav-group">
                        <button
                          className={`settings-nav-item ${isActive ? 'active' : ''}`}
                          aria-current={isActive ? 'page' : undefined}
                          onClick={() => {
                            if (!item.enabled) return;
                            clearFocusRequest();
                            startTransition(() => setActiveSection(item.id));
                          }}
                          disabled={!item.enabled || contentProtectionApplying}
                        >
                          <span className="settings-nav-item-icon">{item.icon}</span>
                          {item.label}
                          {!item.enabled && <span className="settings-nav-badge">Soon</span>}
                        </button>

                        {subs && subs.length > 0 && (
                          <ul className="settings-nav-tree">
                            {subs.map((sub, i) => (
                              <li
                                key={sub.id}
                                className={`settings-nav-tree-item-wrapper${i === subs.length - 1 ? ' settings-nav-tree-item-wrapper--last' : ''}`}
                              >
                                <button
                                  className={`settings-nav-tree-item${isActive && activeSubsection === sub.id ? ' active' : ''}`}
                                  aria-current={
                                    isActive && activeSubsection === sub.id ? 'location' : undefined
                                  }
                                  onClick={() =>
                                    requestFocus(item.id, sub.focusControlId ?? sub.targetId)
                                  }
                                  disabled={contentProtectionApplying}
                                >
                                  {sub.label}
                                </button>
                              </li>
                            ))}
                          </ul>
                        )}
                      </li>
                    );
                  })}
                </ul>
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
                  {contentProtectionApplyFailed && (
                    <p className="settings-pending-blob-text" role="alert">
                      Couldn’t apply screen-capture protection. Your change has not been saved. Try
                      again.
                    </p>
                  )}
                  <div className="settings-pending-blob-actions">
                    <button
                      className="settings-pending-blob-apply"
                      onClick={() => void apply()}
                      disabled={contentProtectionApplying}
                    >
                      {hwAccelChanged ? 'Apply & Restart' : 'Apply'}
                    </button>
                    <button
                      className="settings-pending-blob-revert"
                      onClick={revert}
                      disabled={contentProtectionApplying}
                    >
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
              inert={contentProtectionApplying}
              onClickCapture={(event) => {
                if (contentProtectionApplying) {
                  event.preventDefault();
                  event.stopPropagation();
                }
              }}
              onChangeCapture={(event) => {
                if (contentProtectionApplying) {
                  event.preventDefault();
                  event.stopPropagation();
                }
              }}
              onKeyDownCapture={(event) => {
                if (contentProtectionApplying) {
                  event.preventDefault();
                  event.stopPropagation();
                }
              }}
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
