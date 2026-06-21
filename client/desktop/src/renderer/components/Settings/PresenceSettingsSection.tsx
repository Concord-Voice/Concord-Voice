import React, { useState, useEffect, useCallback, useRef } from 'react';
import { apiFetch } from '../../services/apiClient';
import { useAuthStore } from '../../stores/authStore';
import { useRichPresenceStore } from '../../stores/richPresenceStore';
import CollapsibleSection from './CollapsibleSection';
import './PresenceSettingsSection.css';

/**
 * Custom-status visibility tier (#1233 B6).
 *
 * 0 = Off    — nobody sees your custom status.
 * 1 = Friends — friends (and, if Friends-of-Friends is on, mutual-friend FoF).
 * 2 = Servers — anyone who shares a server with you (plus friends).
 *
 * Audience filtering is enforced server-side; this control only sets the
 * declared tier via PATCH /users/me/presence-settings.
 */
const TIERS: { value: number; label: string; hint: string }[] = [
  { value: 0, label: 'Off', hint: 'Nobody can see your custom status.' },
  { value: 1, label: 'Friends', hint: 'Only your friends can see your custom status.' },
  { value: 2, label: 'Servers', hint: 'Friends and members of servers you share can see it.' },
];

const PresenceSettingsSection: React.FC = () => {
  const accessToken = useAuthStore((s) => s.accessToken);
  const tier = useRichPresenceStore((s) => s.self.tier);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  // Once the user changes the tier (or a PATCH is in flight), a late-resolving
  // hydration GET must NOT clobber that intent. The ref flips on first mutation.
  const userHasMutatedRef = useRef(false);

  // Load the current presence settings on mount so the segmented control
  // reflects the server's stored tier rather than the initial-store default.
  useEffect(() => {
    if (!accessToken) return;
    let cancelled = false;
    (async () => {
      try {
        const res = await apiFetch('/api/v1/users/me/presence-settings');
        if (!res.ok || cancelled || userHasMutatedRef.current) return;
        const data = (await res.json()) as {
          custom_text_tier?: number;
          custom_text?: string;
          custom_text_emoji?: string;
        };
        if (cancelled || userHasMutatedRef.current) return;
        useRichPresenceStore.getState().setSelfPresence({
          tier: typeof data.custom_text_tier === 'number' ? data.custom_text_tier : 0,
          customText: data.custom_text || undefined,
          customTextEmoji: data.custom_text_emoji || undefined,
        });
      } catch {
        // Non-critical — keep the current (default) tier on transient failure.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [accessToken]);

  const handleChangeTier = useCallback(
    async (next: number) => {
      if (next === tier || saving) return;
      userHasMutatedRef.current = true;
      setSaving(true);
      setError(null);
      const previous = tier;
      // Optimistic update so the segmented control responds immediately.
      useRichPresenceStore.getState().setSelfPresence({ tier: next });
      try {
        const res = await apiFetch('/api/v1/users/me/presence-settings', {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ custom_text_tier: next }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          throw new Error(data.error || 'Failed to update visibility');
        }
      } catch (err) {
        // Roll back the optimistic update on failure.
        useRichPresenceStore.getState().setSelfPresence({ tier: previous });
        setError(err instanceof Error ? err.message : 'Failed to update visibility');
      } finally {
        setSaving(false);
      }
    },
    [tier, saving]
  );

  const activeHint = TIERS.find((t) => t.value === tier)?.hint ?? '';

  return (
    <CollapsibleSection id="section-presence-settings" title="Custom Status">
      <p className="settings-section-description">
        Set a short status that others can see. Choose who is allowed to see it.
      </p>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Who Can See Your Custom Status</span>
          <span className="settings-row-hint">{activeHint}</span>
        </div>
        {/* Native <fieldset> (implicit role="group") with a visually-hidden
            <legend> for the group label, per S6819 (prefer native over a role
            attribute on a <div>). The .css resets the fieldset's default
            border/margin/padding so the segmented-control visual is unchanged. */}
        <fieldset className="presence-tier-segmented">
          <legend className="presence-tier-legend">Custom status visibility</legend>
          {TIERS.map((t) => (
            <button
              type="button"
              key={t.value}
              className={`presence-tier-option ${tier === t.value ? 'active' : ''}`}
              aria-pressed={tier === t.value}
              disabled={saving}
              onClick={() => handleChangeTier(t.value)}
            >
              {t.label}
            </button>
          ))}
        </fieldset>
      </div>

      {error && <div className="settings-error">{error}</div>}
    </CollapsibleSection>
  );
};

export default PresenceSettingsSection;
