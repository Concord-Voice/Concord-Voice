import { useEffect, useCallback } from 'react';
import { useStoreWithEqualityFn } from 'zustand/traditional';
import { shallow } from 'zustand/shallow';
import {
  useDraftSettingsStore,
  type DraftableAudioSettings,
  type DraftableVideoSettings,
  type DraftableTTSSettings,
  type DraftableAppearanceSettings,
} from '../stores/draftSettingsStore';
import { useAudioSettingsStore } from '../stores/audioSettingsStore';
import { useVideoSettingsStore } from '../stores/videoSettingsStore';
import { useTTSSettingsStore } from '../stores/ttsSettingsStore';
import { useSettingsStore, type AppearanceSettings } from '../stores/settingsStore';

// ---------------------------------------------------------------------------
// Read hooks — return draft value if present, else snapshot, else real store
// ---------------------------------------------------------------------------

export function useDraftAudioSetting<K extends keyof DraftableAudioSettings>(
  key: K
): DraftableAudioSettings[K] {
  return useDraftSettingsStore(
    useCallback(
      (s) => {
        const draft = s.drafts.audio[key];
        if (draft !== undefined) return draft;
        if (s.snapshot) return s.snapshot.audio[key];
        return useAudioSettingsStore.getState()[key];
      },
      [key]
    )
  );
}

export function useDraftVideoSetting<K extends keyof DraftableVideoSettings>(
  key: K
): DraftableVideoSettings[K] {
  return useDraftSettingsStore(
    useCallback(
      (s) => {
        const draft = s.drafts.video[key];
        if (draft !== undefined) return draft;
        if (s.snapshot) return s.snapshot.video[key];
        return useVideoSettingsStore.getState()[key];
      },
      [key]
    )
  );
}

export function useDraftTtsSetting<K extends keyof DraftableTTSSettings>(
  key: K
): DraftableTTSSettings[K] {
  return useDraftSettingsStore(
    useCallback(
      (s) => {
        const draft = s.drafts.tts[key];
        if (draft !== undefined) return draft;
        if (s.snapshot) return s.snapshot.tts[key];
        return useTTSSettingsStore.getState()[key];
      },
      [key]
    )
  );
}

export function useDraftAppearance(): AppearanceSettings {
  return useStoreWithEqualityFn(
    useDraftSettingsStore,
    (s) => {
      if (!s.snapshot) return useSettingsStore.getState().appearance;
      if (Object.keys(s.drafts.appearance).length === 0) return s.snapshot.appearance;
      return { ...s.snapshot.appearance, ...s.drafts.appearance } as AppearanceSettings;
    },
    shallow
  );
}

// ---------------------------------------------------------------------------
// Write functions — plain functions (not hooks) for onChange handlers
// ---------------------------------------------------------------------------

export function setDraftAudioSetting<K extends keyof DraftableAudioSettings>(
  key: K,
  value: DraftableAudioSettings[K]
) {
  useDraftSettingsStore.getState().setAudioDraft(key, value);
}

export function setDraftVideoSetting<K extends keyof DraftableVideoSettings>(
  key: K,
  value: DraftableVideoSettings[K]
) {
  useDraftSettingsStore.getState().setVideoDraft(key, value);
}

export function setDraftTtsSetting<K extends keyof DraftableTTSSettings>(
  key: K,
  value: DraftableTTSSettings[K]
) {
  useDraftSettingsStore.getState().setTtsDraft(key, value);
}

export function setDraftAppearanceSetting<K extends keyof DraftableAppearanceSettings>(
  key: K,
  value: DraftableAppearanceSettings[K]
) {
  useDraftSettingsStore.getState().setAppearanceDraft(key, value);
}

export function batchSetAudioDrafts(updates: Partial<DraftableAudioSettings>) {
  useDraftSettingsStore.getState().batchSetAudioDrafts(updates);
}

// ---------------------------------------------------------------------------
// Lifecycle hook — initialize on mount, teardown on unmount
// ---------------------------------------------------------------------------

export function useDraftSettingsLifecycle() {
  useEffect(() => {
    useDraftSettingsStore.getState().initialize();
    return () => useDraftSettingsStore.getState().teardown();
  }, []);
}

// ---------------------------------------------------------------------------
// Actions hook — apply, revert, change detection
// ---------------------------------------------------------------------------

export function useDraftActions() {
  const apply = useDraftSettingsStore((s) => s.apply);
  const revert = useDraftSettingsStore((s) => s.revert);
  const hasPendingChanges = useDraftSettingsStore(
    (s) =>
      Object.keys(s.drafts.appearance).length > 0 ||
      Object.keys(s.drafts.audio).length > 0 ||
      Object.keys(s.drafts.video).length > 0 ||
      Object.keys(s.drafts.tts).length > 0
  );
  const hwAccelChanged = useDraftSettingsStore((s) => 'hardwareAcceleration' in s.drafts.video);
  return { apply, revert, hasPendingChanges, hwAccelChanged };
}

// ---------------------------------------------------------------------------
// Mode stash hook
// ---------------------------------------------------------------------------

export function useStashAndSwapAudioMode() {
  return useDraftSettingsStore((s) => s.stashAndSwapAudioMode);
}
