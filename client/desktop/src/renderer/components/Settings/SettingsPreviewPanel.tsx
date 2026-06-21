import React from 'react';
import './SettingsPreviewPanel.css';

/**
 * Live preview of how chat and voice render under the current settings.
 *
 * The previews are pure presentational HTML using the real
 * `.message-*` / `.participant-tile-*` class names so they automatically
 * inherit theme, accent color, font size, UI scale, compact mode,
 * reduce-animations, and high-contrast through the existing CSS cascade
 * — no prop-drilling of draft values needed. The `draftSettingsStore`
 * writes draft changes through to the real store on every keystroke (for
 * the doc-root data-attributes that drive these tokens), so what the user
 * sees in the preview is exactly what the app will render after Save.
 *
 * Rendered as a static panel at the top of Appearance + Accessibility —
 * not a CollapsibleSection. Always-visible matches the issue's intent
 * better than a tray that the user has to remember to open before
 * adjusting a setting.
 *
 * Closes #489 (the "live preview samples" half).
 */
const SettingsPreviewPanel: React.FC = () => {
  return (
    <section
      id="section-live-preview"
      className="settings-section settings-preview-panel"
      aria-label="Live Preview"
    >
      <h2 className="settings-section-title">Live Preview</h2>
      <p className="settings-section-description">
        See how your changes look. Updates as you adjust any setting on this page.
      </p>

      <div className="settings-preview-grid">
        <ChatPreview />
        <VoicePreview />
      </div>
    </section>
  );
};

/* ─── Chat preview ────────────────────────────────────────────────────────── */

const ChatPreview: React.FC = () => {
  return (
    <div className="settings-preview-tile">
      <div className="settings-preview-label">Text Chat</div>
      <div className="settings-preview-chat">
        {/* First message — author + timestamp, with a reply-preview to a
            non-loaded parent (a realistic chat shape worth previewing). */}
        <article className="message message--with-avatar">
          <div className="message-content-wrapper">
            {/* Reply preview lives INSIDE the content-wrapper (matches the real
                Message component layout) — sibling-of-message-content positions
                it horizontally instead of stacked. */}
            <div className="settings-preview-reply">
              <span className="settings-preview-reply-author">@alice</span>
              <span className="settings-preview-reply-text">
                …shipped the migration last night.
              </span>
            </div>
            <div className="message-header">
              <span className="message-username settings-preview-author-a">alice</span>
              <span className="message-timestamp">Today at 10:32 AM</span>
            </div>
            <div className="message-text">Following up — anyone hit issues this morning?</div>
          </div>
        </article>

        {/* Second message — longer content with an emoji reaction. */}
        <article className="message message--with-avatar">
          <div className="message-content-wrapper">
            <div className="message-header">
              <span className="message-username settings-preview-author-b">bob</span>
              <span className="message-timestamp">Today at 10:33 AM</span>
            </div>
            <div className="message-text">
              Clean over here. Took the staging deploy and it just worked. Nice work!
            </div>
            <div className="settings-preview-reactions">
              <span className="settings-preview-reaction">
                <span aria-hidden="true">👍</span>
                <span className="settings-preview-reaction-count">2</span>
              </span>
            </div>
          </div>
        </article>

        {/* Third message — short reply, same author as first (no avatar redraw). */}
        <article className="message">
          <div className="message-content-wrapper">
            <div className="message-text">Cool, thanks for confirming.</div>
          </div>
        </article>
      </div>
    </div>
  );
};

/* ─── Voice preview ──────────────────────────────────────────────────────── */

const VoicePreview: React.FC = () => {
  return (
    <div className="settings-preview-tile">
      <div className="settings-preview-label">Voice Chat</div>
      <div className="settings-preview-voice">
        {/* 1: video tile, speaking */}
        <div className="participant-tile participant-tile--video settings-preview-tile-speaking">
          <div className="participant-tile__video settings-preview-video-stub" aria-hidden="true" />
          <div className="participant-tile__video-name">
            <span className="participant-tile__video-name-text">alice</span>
          </div>
        </div>

        {/* 2: avatar-only, muted */}
        <div className="participant-tile">
          <div className="participant-tile__body">
            <div className="participant-tile__avatar">
              <div className="participant-tile__avatar-fallback">B</div>
            </div>
            <div className="participant-tile__name">bob</div>
            <span className="settings-preview-state-icon" aria-label="Muted" title="Muted">
              🔇
            </span>
          </div>
        </div>

        {/* 3: avatar-only, deafened */}
        <div className="participant-tile">
          <div className="participant-tile__body">
            <div className="participant-tile__avatar">
              <div className="participant-tile__avatar-fallback">C</div>
            </div>
            <div className="participant-tile__name">carol</div>
            <span className="settings-preview-state-icon" aria-label="Deafened" title="Deafened">
              🎧
            </span>
          </div>
        </div>

        {/* 4: avatar-only, normal */}
        <div className="participant-tile">
          <div className="participant-tile__body">
            <div className="participant-tile__avatar">
              <div className="participant-tile__avatar-fallback">D</div>
            </div>
            <div className="participant-tile__name">dave</div>
          </div>
        </div>
      </div>
    </div>
  );
};

export default SettingsPreviewPanel;
