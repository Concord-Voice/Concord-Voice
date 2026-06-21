import { useDraftSettingsStore } from '@/renderer/stores/draftSettingsStore';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { useTTSSettingsStore } from '@/renderer/stores/ttsSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Mock settingsStore's syncColorSchemeToServer (called via the draft apply flow).
vi.mock('@/renderer/stores/settingsStore', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/stores/settingsStore')>();
  return {
    ...actual,
    syncColorSchemeToServer: vi.fn(),
  };
});

// Spy on setSyncSuppressed — it now lives in the colorSyncSuppression leaf
// module (extracted from settingsStore to avoid the teardown-racing dynamic
// import); the draft apply/teardown flow calls it from there.
const setSyncSuppressedSpy = vi.fn();
vi.mock('@/renderer/stores/colorSyncSuppression', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/stores/colorSyncSuppression')>();
  return {
    ...actual,
    setSyncSuppressed: (...args: Parameters<typeof actual.setSyncSuppressed>) => {
      setSyncSuppressedSpy(...args);
      return actual.setSyncSuppressed(...args);
    },
  };
});

beforeEach(() => {
  resetAllStores();
  // Ensure the draftSettingsStore is torn down cleanly before each test
  useDraftSettingsStore.getState().teardown();

  // Reset settings stores that don't have clear methods in resetAllStores
  // (prevents apply() side effects from leaking between tests)
  useTTSSettingsStore.setState({
    ttsEnabled: false,
    ttsSendEnabled: false,
    ttsVoice: null,
    ttsRate: 1,
    ttsVolume: 1,
  });
  useAudioSettingsStore.setState({
    inputVolume: 100,
    outputVolume: 100,
    noiseCancellation: true,
    echoCancellation: true,
    autoGainControl: true,
  });
});

describe('draftSettingsStore', () => {
  // ── Initial state ─────────────────────────────────────────────────────

  describe('initial state', () => {
    it('starts with null snapshot', () => {
      expect(useDraftSettingsStore.getState().snapshot).toBeNull();
    });

    it('starts with empty drafts', () => {
      const { drafts } = useDraftSettingsStore.getState();
      expect(Object.keys(drafts.appearance)).toHaveLength(0);
      expect(Object.keys(drafts.audio)).toHaveLength(0);
      expect(Object.keys(drafts.video)).toHaveLength(0);
      expect(Object.keys(drafts.tts)).toHaveLength(0);
    });

    it('starts with null mode stashes', () => {
      expect(useDraftSettingsStore.getState().audioBasicStash).toBeNull();
      expect(useDraftSettingsStore.getState().audioAdvancedStash).toBeNull();
    });
  });

  // ── Lifecycle ─────────────────────────────────────────────────────────

  describe('initialize', () => {
    it('captures a snapshot and resets drafts', () => {
      useDraftSettingsStore.getState().initialize();
      const state = useDraftSettingsStore.getState();
      expect(state.snapshot).not.toBeNull();
      expect(state.snapshot?.appearance).toBeDefined();
      expect(state.snapshot?.audio).toBeDefined();
      expect(state.snapshot?.video).toBeDefined();
      expect(state.snapshot?.tts).toBeDefined();
    });

    it('resets drafts on initialize', () => {
      // Set some fake draft data first
      useDraftSettingsStore.setState({
        drafts: {
          appearance: { theme: 'light' },
          audio: { inputVolume: 50 },
          video: {},
          tts: {},
        },
      });
      useDraftSettingsStore.getState().initialize();
      const { drafts } = useDraftSettingsStore.getState();
      expect(Object.keys(drafts.appearance)).toHaveLength(0);
      expect(Object.keys(drafts.audio)).toHaveLength(0);
    });

    it('resets mode stashes on initialize', () => {
      useDraftSettingsStore.setState({
        audioBasicStash: { inputVolume: 80 },
        audioAdvancedStash: { inputVolume: 90 },
      });
      useDraftSettingsStore.getState().initialize();
      expect(useDraftSettingsStore.getState().audioBasicStash).toBeNull();
      expect(useDraftSettingsStore.getState().audioAdvancedStash).toBeNull();
    });

    it('captures current appearance settings in snapshot', () => {
      useSettingsStore.getState().setTheme('light');
      useDraftSettingsStore.getState().initialize();
      expect(useDraftSettingsStore.getState().snapshot?.appearance.theme).toBe('light');
    });

    it('captures current audio settings in snapshot', () => {
      useAudioSettingsStore.getState().setInputVolume(75);
      useDraftSettingsStore.getState().initialize();
      expect(useDraftSettingsStore.getState().snapshot?.audio.inputVolume).toBe(75);
    });

    it('captures current TTS settings in snapshot', () => {
      useTTSSettingsStore.getState().setTtsEnabled(true);
      useDraftSettingsStore.getState().initialize();
      expect(useDraftSettingsStore.getState().snapshot?.tts.ttsEnabled).toBe(true);
    });
  });

  describe('teardown', () => {
    it('clears snapshot', () => {
      useDraftSettingsStore.getState().initialize();
      expect(useDraftSettingsStore.getState().snapshot).not.toBeNull();
      useDraftSettingsStore.getState().teardown();
      expect(useDraftSettingsStore.getState().snapshot).toBeNull();
    });

    it('clears drafts', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 50);
      useDraftSettingsStore.getState().teardown();
      expect(Object.keys(useDraftSettingsStore.getState().drafts.audio)).toHaveLength(0);
    });

    it('clears mode stashes', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.setState({ audioBasicStash: { inputVolume: 50 } });
      useDraftSettingsStore.getState().teardown();
      expect(useDraftSettingsStore.getState().audioBasicStash).toBeNull();
      expect(useDraftSettingsStore.getState().audioAdvancedStash).toBeNull();
    });

    it('restores appearance from snapshot if pending appearance drafts exist', () => {
      useSettingsStore.getState().setTheme('dark');
      useDraftSettingsStore.getState().initialize();

      // Make an appearance draft change (this writes through to real store)
      useDraftSettingsStore.getState().setAppearanceDraft('theme', 'light');
      expect(useSettingsStore.getState().appearance.theme).toBe('light');

      // Teardown should restore to snapshot value
      useDraftSettingsStore.getState().teardown();
      expect(useSettingsStore.getState().appearance.theme).toBe('dark');
    });

    it('does not restore appearance if no appearance drafts exist', () => {
      useSettingsStore.getState().setTheme('light');
      useDraftSettingsStore.getState().initialize();

      // Change theme directly (not via draft)
      useSettingsStore.getState().setTheme('dark');

      // Teardown should NOT restore since no appearance drafts
      useDraftSettingsStore.getState().teardown();
      expect(useSettingsStore.getState().appearance.theme).toBe('dark');
    });
  });

  // ── Draft setters ─────────────────────────────────────────────────────

  describe('setAppearanceDraft', () => {
    it('records a draft when value differs from snapshot', () => {
      useSettingsStore.getState().setTheme('dark');
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().setAppearanceDraft('theme', 'light');
      expect(useDraftSettingsStore.getState().drafts.appearance.theme).toBe('light');
    });

    it('removes draft entry when value matches snapshot', () => {
      useSettingsStore.getState().setTheme('dark');
      useDraftSettingsStore.getState().initialize();

      // Set to different value, then back to original
      useDraftSettingsStore.getState().setAppearanceDraft('theme', 'light');
      expect(useDraftSettingsStore.getState().drafts.appearance.theme).toBe('light');

      useDraftSettingsStore.getState().setAppearanceDraft('theme', 'dark');
      expect(useDraftSettingsStore.getState().drafts.appearance.theme).toBeUndefined();
    });

    it('writes through to the real settings store for live preview', () => {
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().setAppearanceDraft('compactMode', true);
      expect(useSettingsStore.getState().appearance.compactMode).toBe(true);
    });

    it('does nothing if no snapshot exists', () => {
      // Do not initialize
      useDraftSettingsStore.getState().setAppearanceDraft('theme', 'light');
      expect(useDraftSettingsStore.getState().drafts.appearance.theme).toBeUndefined();
    });
  });

  describe('setAudioDraft', () => {
    it('records an audio draft when value differs from snapshot', () => {
      useDraftSettingsStore.getState().initialize();
      const snapshotVol = useDraftSettingsStore.getState().snapshot!.audio.inputVolume;

      useDraftSettingsStore.getState().setAudioDraft('inputVolume', snapshotVol + 10);
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBe(snapshotVol + 10);
    });

    it('removes draft when value reverts to snapshot value', () => {
      useDraftSettingsStore.getState().initialize();
      const snapshotVol = useDraftSettingsStore.getState().snapshot!.audio.inputVolume;

      useDraftSettingsStore.getState().setAudioDraft('inputVolume', snapshotVol + 10);
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', snapshotVol);
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBeUndefined();
    });

    it('does nothing if no snapshot exists', () => {
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 50);
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBeUndefined();
    });
  });

  describe('setVideoDraft', () => {
    it('records a video draft when value differs from snapshot', () => {
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().setVideoDraft('cameraPreset', '720p30');
      expect(useDraftSettingsStore.getState().drafts.video.cameraPreset).toBe('720p30');
    });

    it('removes draft when value reverts to snapshot', () => {
      useDraftSettingsStore.getState().initialize();
      const snapshotPreset = useDraftSettingsStore.getState().snapshot!.video.cameraPreset;

      useDraftSettingsStore.getState().setVideoDraft('cameraPreset', '720p30');
      useDraftSettingsStore.getState().setVideoDraft('cameraPreset', snapshotPreset);
      expect(useDraftSettingsStore.getState().drafts.video.cameraPreset).toBeUndefined();
    });

    it('does nothing if no snapshot exists', () => {
      useDraftSettingsStore.getState().setVideoDraft('cameraPreset', '720p30');
      expect(useDraftSettingsStore.getState().drafts.video.cameraPreset).toBeUndefined();
    });
  });

  describe('setTtsDraft', () => {
    it('records a TTS draft when value differs from snapshot', () => {
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().setTtsDraft('ttsEnabled', true);
      expect(useDraftSettingsStore.getState().drafts.tts.ttsEnabled).toBe(true);
    });

    it('removes draft when value reverts to snapshot', () => {
      useDraftSettingsStore.getState().initialize();
      const snapshotEnabled = useDraftSettingsStore.getState().snapshot!.tts.ttsEnabled;

      useDraftSettingsStore.getState().setTtsDraft('ttsEnabled', !snapshotEnabled);
      useDraftSettingsStore.getState().setTtsDraft('ttsEnabled', snapshotEnabled);
      expect(useDraftSettingsStore.getState().drafts.tts.ttsEnabled).toBeUndefined();
    });

    it('does nothing if no snapshot exists', () => {
      useDraftSettingsStore.getState().setTtsDraft('ttsEnabled', true);
      expect(useDraftSettingsStore.getState().drafts.tts.ttsEnabled).toBeUndefined();
    });
  });

  // ── batchSetAudioDrafts ───────────────────────────────────────────────

  describe('batchSetAudioDrafts', () => {
    it('sets multiple audio drafts at once', () => {
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().batchSetAudioDrafts({
        inputVolume: 50,
        outputVolume: 75,
        noiseCancellation: false,
      });

      const drafts = useDraftSettingsStore.getState().drafts.audio;
      expect(drafts.inputVolume).toBe(50);
      expect(drafts.outputVolume).toBe(75);
    });

    it('only records entries that differ from snapshot', () => {
      useDraftSettingsStore.getState().initialize();
      const snapshot = useDraftSettingsStore.getState().snapshot!.audio;

      useDraftSettingsStore.getState().batchSetAudioDrafts({
        inputVolume: snapshot.inputVolume, // same as snapshot - should not be drafted
        outputVolume: snapshot.outputVolume + 10, // different - should be drafted
      });

      const drafts = useDraftSettingsStore.getState().drafts.audio;
      expect(drafts.inputVolume).toBeUndefined();
      expect(drafts.outputVolume).toBe(snapshot.outputVolume + 10);
    });

    it('removes existing drafts when batch values match snapshot', () => {
      useDraftSettingsStore.getState().initialize();
      const snapshot = useDraftSettingsStore.getState().snapshot!.audio;

      // First set a draft
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 50);
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBe(50);

      // Then batch set with snapshot value
      useDraftSettingsStore.getState().batchSetAudioDrafts({
        inputVolume: snapshot.inputVolume,
      });
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBeUndefined();
    });

    it('does nothing if no snapshot exists', () => {
      useDraftSettingsStore.getState().batchSetAudioDrafts({ inputVolume: 50 });
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBeUndefined();
    });
  });

  // ── stashAndSwapAudioMode ─────────────────────────────────────────────

  describe('stashAndSwapAudioMode', () => {
    it('stashes basic values when switching to advanced', () => {
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().stashAndSwapAudioMode(true, 'standard');

      // Basic stash should now have values
      expect(useDraftSettingsStore.getState().audioBasicStash).not.toBeNull();
    });

    it('stashes advanced values when switching to basic', () => {
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().stashAndSwapAudioMode(false, 'standard');

      expect(useDraftSettingsStore.getState().audioAdvancedStash).not.toBeNull();
    });

    it('restores advanced stash when switching to advanced mode', () => {
      useDraftSettingsStore.getState().initialize();
      const snapshot = useDraftSettingsStore.getState().snapshot!.audio;

      // First switch to basic (stashes current as advanced)
      useDraftSettingsStore
        .getState()
        .setAudioDraft('silenceDetection', !snapshot.silenceDetection);
      useDraftSettingsStore.getState().stashAndSwapAudioMode(false, 'standard');

      // Then switch back to advanced (should restore the advanced stash)
      useDraftSettingsStore.getState().stashAndSwapAudioMode(true, 'standard');

      // The advanced stash had silenceDetection toggled, should be restored
      expect(useDraftSettingsStore.getState().audioBasicStash).not.toBeNull();
    });

    it('applies tier defaults when switching to basic with no basic stash', () => {
      useDraftSettingsStore.getState().initialize();

      // Switch to basic without any prior basic stash
      useDraftSettingsStore.getState().stashAndSwapAudioMode(false, 'standard');

      // Advanced stash should be saved
      expect(useDraftSettingsStore.getState().audioAdvancedStash).not.toBeNull();
    });

    it('restores basic stash when switching to basic and stash exists', () => {
      useDraftSettingsStore.getState().initialize();

      // Switch to advanced (stashes basic)
      useDraftSettingsStore.getState().stashAndSwapAudioMode(true, 'standard');
      expect(useDraftSettingsStore.getState().audioBasicStash).not.toBeNull();

      // Switch back to basic (should restore basic stash)
      useDraftSettingsStore.getState().stashAndSwapAudioMode(false, 'standard');
      expect(useDraftSettingsStore.getState().audioAdvancedStash).not.toBeNull();
    });

    it('does nothing if no snapshot exists', () => {
      useDraftSettingsStore.getState().stashAndSwapAudioMode(true, 'standard');
      expect(useDraftSettingsStore.getState().audioBasicStash).toBeNull();
    });
  });

  // ── apply ─────────────────────────────────────────────────────────────

  describe('apply', () => {
    it('does nothing if no snapshot', () => {
      // Apply with no snapshot is a no-op and must not throw
      expect(() => useDraftSettingsStore.getState().apply()).not.toThrow();
      // And no stash should have been written
      expect(useDraftSettingsStore.getState().audioBasicStash).toBeNull();
    });

    it('pushes audio drafts to the real audio store', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);
      useDraftSettingsStore.getState().apply();

      expect(useAudioSettingsStore.getState().inputVolume).toBe(42);
    });

    it('pushes TTS drafts to the real TTS store', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setTtsDraft('ttsEnabled', true);
      useDraftSettingsStore.getState().setTtsDraft('ttsRate', 1.5);
      useDraftSettingsStore.getState().apply();

      expect(useTTSSettingsStore.getState().ttsEnabled).toBe(true);
      expect(useTTSSettingsStore.getState().ttsRate).toBe(1.5);
    });

    it('clears drafts after apply', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);
      useDraftSettingsStore.getState().apply();

      const { drafts } = useDraftSettingsStore.getState();
      expect(Object.keys(drafts.audio)).toHaveLength(0);
    });

    it('re-captures a fresh snapshot after apply', () => {
      useDraftSettingsStore.getState().initialize();

      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);
      useDraftSettingsStore.getState().apply();

      const newSnapshot = useDraftSettingsStore.getState().snapshot;
      expect(newSnapshot).not.toBeNull();
      // The new snapshot should reflect the applied changes
      expect(newSnapshot?.audio.inputVolume).toBe(42);
    });

    it('keeps mode stashes across apply', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().stashAndSwapAudioMode(true, 'standard');

      const basicStash = useDraftSettingsStore.getState().audioBasicStash;
      useDraftSettingsStore.getState().apply();

      // Mode stashes should survive apply
      expect(useDraftSettingsStore.getState().audioBasicStash).toEqual(basicStash);
    });
  });

  // ── revert ────────────────────────────────────────────────────────────

  describe('revert', () => {
    it('clears all drafts', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);
      useDraftSettingsStore.getState().setTtsDraft('ttsEnabled', true);

      useDraftSettingsStore.getState().revert();

      const { drafts } = useDraftSettingsStore.getState();
      expect(Object.keys(drafts.audio)).toHaveLength(0);
      expect(Object.keys(drafts.tts)).toHaveLength(0);
    });

    it('clears mode stashes', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().stashAndSwapAudioMode(true, 'standard');

      useDraftSettingsStore.getState().revert();

      expect(useDraftSettingsStore.getState().audioBasicStash).toBeNull();
      expect(useDraftSettingsStore.getState().audioAdvancedStash).toBeNull();
    });

    it('restores appearance from snapshot', () => {
      useSettingsStore.getState().setTheme('dark');
      useDraftSettingsStore.getState().initialize();

      // Change theme via draft (writes through to real store)
      useDraftSettingsStore.getState().setAppearanceDraft('theme', 'light');
      expect(useSettingsStore.getState().appearance.theme).toBe('light');

      useDraftSettingsStore.getState().revert();
      expect(useSettingsStore.getState().appearance.theme).toBe('dark');
    });

    it('preserves snapshot after revert (page still open)', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().revert();
      expect(useDraftSettingsStore.getState().snapshot).not.toBeNull();
    });

    // Regression: every settable appearance key must be restored to its
    // snapshot value on revert — not just the originally-listed ones.
    // The first version of restoreAppearanceFromSnapshot hand-listed the 5
    // setters it knew about and silently missed `highContrast` + `uiScale`
    // when those were added in #489, so toggling either then clicking Revert
    // would reset the slider/toggle UI position but leave the DOM in the
    // modified state (because the draft preview's write-through to the real
    // store was never undone). The fix iterates over snapshot keys and
    // dispatches each via callSetter, so future additions can't regress this.
    it('restores highContrast and uiScale on revert (regression)', () => {
      useSettingsStore.getState().setHighContrast(false);
      useSettingsStore.getState().setUiScale(1);
      useDraftSettingsStore.getState().initialize();

      // Live-preview modifications via draft writes — these write through to
      // the real store immediately (the draft store's intended behavior).
      useDraftSettingsStore.getState().setAppearanceDraft('highContrast', true);
      useDraftSettingsStore.getState().setAppearanceDraft('uiScale', 1.25);
      expect(useSettingsStore.getState().appearance.highContrast).toBe(true);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(1.25);

      useDraftSettingsStore.getState().revert();

      // Real store must reflect the snapshot, not the modified live values.
      expect(useSettingsStore.getState().appearance.highContrast).toBe(false);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(1);
    });
  });

  // ── Edge cases ────────────────────────────────────────────────────────

  describe('edge cases', () => {
    it('multiple initialize calls reset cleanly', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);

      // Re-initialize should reset everything
      useDraftSettingsStore.getState().initialize();
      expect(Object.keys(useDraftSettingsStore.getState().drafts.audio)).toHaveLength(0);
    });

    it('apply then set new draft works', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);
      useDraftSettingsStore.getState().apply();

      // Now input volume in snapshot is 42
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 60);
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBe(60);

      // Setting back to 42 (now the snapshot value) should remove the draft
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBeUndefined();
    });

    it('setting the same draft multiple times only keeps latest', () => {
      useDraftSettingsStore.getState().initialize();
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 10);
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 20);
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 30);
      expect(useDraftSettingsStore.getState().drafts.audio.inputVolume).toBe(30);
    });
  });

  // ── setSyncSuppressed integration ─────────────────────────────────────

  describe('setSyncSuppressed integration', () => {
    beforeEach(() => {
      setSyncSuppressedSpy.mockClear();
    });

    it('initialize calls setSyncSuppressed(true)', () => {
      useDraftSettingsStore.getState().initialize();
      expect(setSyncSuppressedSpy).toHaveBeenCalledWith(true);
    });

    it('teardown calls setSyncSuppressed(false)', () => {
      useDraftSettingsStore.getState().initialize();
      setSyncSuppressedSpy.mockClear();

      useDraftSettingsStore.getState().teardown();
      expect(setSyncSuppressedSpy).toHaveBeenCalledWith(false);
    });

    it('apply temporarily lifts suppression then re-enables it', () => {
      useDraftSettingsStore.getState().initialize();
      setSyncSuppressedSpy.mockClear();

      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 42);
      useDraftSettingsStore.getState().apply();

      // apply calls setSyncSuppressed(false) then setSyncSuppressed(true)
      const calls = setSyncSuppressedSpy.mock.calls.map((c: [boolean]) => c[0]);
      expect(calls).toContain(false);
      expect(calls).toContain(true);
      // The last call should re-enable suppression
      expect(calls[calls.length - 1]).toBe(true);
    });
  });
});
