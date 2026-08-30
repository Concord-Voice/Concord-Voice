import { z } from 'zod';
import { usePresenceOverrideStore } from '../../stores/ui/presenceOverrideStore';
import {
  parsePresenceOverrides,
  type PresenceOverridesDocument,
} from '../../utils/presenceOverrides';
import { apiFetch } from './apiClient';
import { e2eeService } from '../e2ee/e2eeService';

const ENDPOINT = '/api/v1/users/me/presence-overrides/custom_text';
const LOAD_ERROR = 'Failed to load presence exceptions';
const SAVE_ERROR = 'Failed to save presence exceptions';
const E2EE_ERROR = 'Presence exception encryption is unavailable';

const PreferenceSchema = z.strictObject({
  encrypted_data: z.string(),
  version: z.number().int().positive(),
  updated_at: z.string().datetime({ offset: true }),
});

const GetResponseSchema = z.strictObject({
  preference: PreferenceSchema.nullable(),
});

const SaveResponseSchema = z.strictObject({
  version: z.number().int().positive(),
});

class PresenceOverrideSyncService {
  private generation = 0;
  private fetchSequence = 0;
  private fetchController: AbortController | null = null;
  private saveController: AbortController | null = null;
  private pendingSave = false;
  private deferredVersion: number | null = null;

  async fetchAndApply(): Promise<boolean> {
    const generation = this.generation;
    if (this.pendingSave) return true;
    await this.fetchAndApplyInternal();
    return generation === this.generation;
  }

  private async fetchAndApplyInternal(): Promise<void> {
    const generation = this.generation;
    const sequence = ++this.fetchSequence;
    this.fetchController?.abort();
    const controller = new AbortController();
    this.fetchController = controller;

    const store = usePresenceOverrideStore.getState();
    if (!e2eeService.isInitialized) {
      store.setLoading(false);
      store.setError(E2EE_ERROR);
      this.fetchController = null;
      return;
    }

    store.setLoading(true);
    store.setError(null);

    try {
      const response = await apiFetch(
        ENDPOINT,
        { signal: controller.signal },
        { authoritative: false }
      );
      if (!this.isCurrentFetch(generation, sequence, controller)) return;
      if (!response.ok) throw new Error(LOAD_ERROR);

      const raw: unknown = await response.json();
      if (!this.isCurrentFetch(generation, sequence, controller)) return;
      const envelope = GetResponseSchema.parse(raw);
      if (envelope.preference === null) {
        usePresenceOverrideStore.getState().apply([], 0);
        return;
      }

      const decrypted = await e2eeService.decryptPreferences<unknown>(
        envelope.preference.encrypted_data
      );
      if (!this.isCurrentFetch(generation, sequence, controller)) return;
      const document = parsePresenceOverrides(decrypted);
      usePresenceOverrideStore
        .getState()
        .apply(document.excludedUserIds, envelope.preference.version);
    } catch {
      if (this.isCurrentFetch(generation, sequence, controller)) {
        usePresenceOverrideStore.getState().setError(LOAD_ERROR);
      }
    } finally {
      if (this.isCurrentFetchSequence(generation, sequence, controller)) {
        this.fetchController = null;
        usePresenceOverrideStore.getState().setLoading(false);
      }
    }
  }

  /**
   * Save the encrypted document and report whether this account generation is
   * still current when every deferred continuation has settled.
   */
  async save(excludedUserIds: readonly string[]): Promise<boolean> {
    if (this.pendingSave) return false;
    const generation = this.generation;
    await this.saveCurrent(excludedUserIds);
    return generation === this.generation;
  }

  private async saveCurrent(excludedUserIds: readonly string[]): Promise<void> {
    let document: PresenceOverridesDocument;
    try {
      document = parsePresenceOverrides({ v: 1, excludedUserIds: [...excludedUserIds] });
    } catch {
      usePresenceOverrideStore.getState().setError(SAVE_ERROR);
      return;
    }

    const store = usePresenceOverrideStore.getState();
    if (!e2eeService.isInitialized) {
      store.setSaving(false);
      store.setError(E2EE_ERROR);
      return;
    }

    const generation = this.generation;
    const controller = new AbortController();
    this.invalidateFetch();
    this.saveController = controller;
    this.pendingSave = true;
    this.deferredVersion = null;
    const expectedVersion = store.appliedVersion;
    store.setSaving(true);
    store.setConflict(false);
    store.setError(null);

    try {
      const encryptedData = await e2eeService.encryptPreferences(document);
      if (!this.isCurrentSave(generation, controller)) return;

      const response = await apiFetch(ENDPOINT, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          encrypted_data: encryptedData,
          expected_version: expectedVersion,
          excluded_user_ids: document.excludedUserIds,
        }),
        signal: controller.signal,
      });
      if (!this.isCurrentSave(generation, controller)) return;

      if (response.status === 409) {
        await this.fetchAndApplyInternal();
        if (this.isCurrentSave(generation, controller)) {
          usePresenceOverrideStore.getState().setConflict(true);
        }
        return;
      }
      if (!response.ok) throw new Error(SAVE_ERROR);

      const raw: unknown = await response.json();
      if (!this.isCurrentSave(generation, controller)) return;
      const result = SaveResponseSchema.parse(raw);
      usePresenceOverrideStore.getState().apply(document.excludedUserIds, result.version);
    } catch {
      if (this.isCurrentSave(generation, controller)) {
        usePresenceOverrideStore.getState().setError(SAVE_ERROR);
      }
    } finally {
      if (generation !== this.generation || this.saveController !== controller) return;

      this.saveController = null;
      this.pendingSave = false;
      usePresenceOverrideStore.getState().setSaving(false);
      const deferredVersion = this.deferredVersion;
      this.deferredVersion = null;
      if (
        deferredVersion !== null &&
        deferredVersion !== usePresenceOverrideStore.getState().appliedVersion
      ) {
        await this.fetchAndApply();
      }
    }
  }

  async handleRemoteUpdate(version: number): Promise<void> {
    if (!Number.isInteger(version) || version <= 0) return;
    if (this.pendingSave) {
      this.deferredVersion = Math.max(this.deferredVersion ?? 0, version);
      return;
    }
    if (version === usePresenceOverrideStore.getState().appliedVersion) return;
    await this.fetchAndApply();
  }

  reset(): void {
    this.generation += 1;
    this.fetchSequence += 1;
    this.fetchController?.abort();
    this.saveController?.abort();
    this.fetchController = null;
    this.saveController = null;
    this.pendingSave = false;
    this.deferredVersion = null;
    usePresenceOverrideStore.getState().reset();
  }

  private isCurrentFetch(
    generation: number,
    sequence: number,
    controller: AbortController
  ): boolean {
    return (
      this.isCurrentFetchSequence(generation, sequence, controller) && !controller.signal.aborted
    );
  }

  private isCurrentFetchSequence(
    generation: number,
    sequence: number,
    controller: AbortController
  ): boolean {
    return (
      generation === this.generation &&
      sequence === this.fetchSequence &&
      this.fetchController === controller
    );
  }

  private isCurrentSave(generation: number, controller: AbortController): boolean {
    return (
      generation === this.generation &&
      this.saveController === controller &&
      !controller.signal.aborted
    );
  }

  private invalidateFetch(): void {
    this.fetchSequence += 1;
    this.fetchController?.abort();
    this.fetchController = null;
    usePresenceOverrideStore.getState().setLoading(false);
  }
}

export const presenceOverrideSyncService = new PresenceOverrideSyncService();
