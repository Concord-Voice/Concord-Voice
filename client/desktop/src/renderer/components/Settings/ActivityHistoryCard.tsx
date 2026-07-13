import React, { useCallback, useEffect, useRef, useState } from 'react';
import { clientConfigService } from '../../services/clientConfigService';
import {
  ACTIVITY_HISTORY_RETENTION_DAYS,
  deletePresenceHistory,
  getPresenceHistorySettings,
  patchPresenceHistorySettings,
  PresenceHistoryRequestError,
  type ActivityHistoryRetentionDays,
  type PresenceHistorySettings,
} from '../../services/presenceHistoryService';
import { useAuthStore } from '../../stores/authStore';
import {
  useClientConfigStore,
  type ActivityHistoryCapabilityState,
} from '../../stores/clientConfigStore';
import { useSettingsNavStore } from '../../stores/settingsNavStore';
import { useUserStore } from '../../stores/userStore';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import ActivityHistoryConsentModal, {
  type ActivityHistoryConsentSelection,
} from './ActivityHistoryConsentModal';
import ToggleSwitch from './ToggleSwitch';
import './ActivityHistory.css';

type ActivityHistoryRequestState =
  | { status: 'loading' }
  | { status: 'confirmed'; settings: PresenceHistorySettings }
  | { status: 'unavailable'; settings: PresenceHistorySettings }
  | {
      status: 'error';
      confirmed: PresenceHistorySettings | null;
      message: string;
      routeMissing: boolean;
    };

interface RequestEnvelope {
  ownerKey: string | null;
  state: ActivityHistoryRequestState;
}

interface MutationEnvelope {
  ownerKey: string | null;
  active: boolean;
}

interface AnnouncementEnvelope {
  ownerKey: string | null;
  message: string;
}

type ConsentMode = 'enable' | 'reconsent';
type Confirmation =
  | { kind: 'retention-decrease'; retentionDays: ActivityHistoryRetentionDays }
  | { kind: 'disable' }
  | { kind: 'delete' };

interface ModalEnvelope {
  ownerKey: string | null;
  consentMode: ConsentMode | null;
  confirmation: Confirmation | null;
}

interface RequestToken {
  ownerKey: string;
  generation: number;
  controller: AbortController;
}

interface ActivityHistoryCardController {
  capability: ActivityHistoryCapabilityState;
  canLoadSettings: boolean;
  ownerKey: string | null;
  headingRef: React.RefObject<HTMLHeadingElement | null>;
  requestState: ActivityHistoryRequestState;
  mutationActive: boolean;
  announcement: string;
  visibleModal: ModalEnvelope;
  consentCapabilityLost: boolean;
  visibleConsentMode: ConsentMode | null;
  refreshSettings: (requestOwnerKey: string, preserveConfirmed: boolean) => Promise<void>;
  enableHistory: (
    selection: ActivityHistoryConsentSelection,
    priorSettings: PresenceHistorySettings
  ) => Promise<void>;
  changeRetention: (
    nextRetention: ActivityHistoryRetentionDays,
    priorSettings: PresenceHistorySettings
  ) => Promise<void>;
  removeHistory: (
    kind: 'disable' | 'delete',
    priorSettings: PresenceHistorySettings
  ) => Promise<void>;
  closeModal: () => void;
  openConsent: (mode: ConsentMode) => void;
  openConfirmation: (confirmation: Confirmation) => void;
  retryAvailability: () => void;
}

const STALE_CAPABILITY_ANNOUNCEMENT = 'Availability not refreshed.';
const CAPABILITY_WIDENING_PAUSED_MESSAGE =
  'Activity History availability changed. Enabling and retention increases are paused until support is confirmed.';

function stateForSettings(settings: PresenceHistorySettings): ActivityHistoryRequestState {
  if (!settings.available) return { status: 'unavailable', settings };
  return { status: 'confirmed', settings };
}

function settingsFromState(state: ActivityHistoryRequestState): PresenceHistorySettings | null {
  if (state.status === 'confirmed' || state.status === 'unavailable') return state.settings;
  if (state.status === 'error') return state.confirmed;
  return null;
}

function retentionFromValue(value: string): ActivityHistoryRetentionDays | null {
  switch (value) {
    case '7':
      return 7;
    case '30':
      return 30;
    case '90':
      return 90;
    case '365':
      return 365;
    default:
      return null;
  }
}

function settingsStatus(settings: PresenceHistorySettings): string {
  if (settings.reconsentRequired) return 'Paused';
  if (!settings.available) return 'Unavailable';
  return settings.enabled ? 'On' : 'Off';
}

function settingsDescription(settings: PresenceHistorySettings): string {
  if (settings.reconsentRequired) {
    return 'Recording is paused because the Activity History terms changed.';
  }
  if (!settings.available) {
    return 'This instance operator has not configured the disclosure required for Activity History.';
  }
  if (settings.enabled) {
    return 'Future Custom Status changes are stored for your selected retention period.';
  }
  return 'Activity History is off. No activity is recorded or backfilled.';
}

function mutationErrorMessage(error: unknown): string {
  if (
    error instanceof PresenceHistoryRequestError &&
    error.status === 503 &&
    error.retryAfter !== null
  ) {
    return `Activity History is temporarily unavailable. Try again in ${error.retryAfter} seconds.`;
  }
  return 'Activity History settings could not be saved.';
}

function isConsentConflict(error: unknown): boolean {
  return error instanceof PresenceHistoryRequestError && error.status === 409;
}

function isMissingHistoryRoute(error: unknown): boolean {
  return (
    error instanceof PresenceHistoryRequestError && (error.status === 404 || error.status === 405)
  );
}

function isConfirmedOff(settings: PresenceHistorySettings): boolean {
  return (
    !settings.enabled &&
    !settings.reconsentRequired &&
    settings.consentVersion === null &&
    settings.consentCopyHash === null &&
    settings.consentedAt === null
  );
}

function handleSettledPromise(promise: Promise<unknown>): void {
  promise.catch(() => undefined);
}

function liveCapabilitySupportsWidening(): boolean {
  return useClientConfigStore.getState().activityHistoryCapability.status === 'supported';
}

function useActivityHistoryCardController(): ActivityHistoryCardController {
  const userId = useUserStore((state) => state.user?.id ?? null);
  const sessionId = useAuthStore((state) => state.sessionId);
  const capability = useClientConfigStore((state) => state.activityHistoryCapability);
  const canLoadSettings =
    capability.status === 'supported' ||
    capability.status === 'confirmed-unsupported' ||
    (capability.status === 'error' && capability.lastConfirmedSupported);
  const ownerKey = userId !== null && sessionId !== null ? `${userId}:${sessionId}` : null;
  const headingRef = useRef<HTMLHeadingElement>(null);
  const ownerKeyRef = useRef<string | null>(ownerKey);
  const requestGenerationRef = useRef(0);
  const requestControllerRef = useRef<AbortController | null>(null);
  const ownerLifecycleGenerationRef = useRef(0);
  const pendingHeadingFocusOwnerRef = useRef<string | null>(null);
  const invalidatedConsentEnvelopeRef = useRef<ModalEnvelope | null>(null);
  const settingsLoadStateRef = useRef<{
    ownerKey: string | null;
    capabilityStatus: ActivityHistoryCapabilityState['status'];
    canLoadSettings: boolean;
  } | null>(null);
  const pendingSettingsRefreshRef = useRef<{
    ownerKey: string;
    preserveConfirmed: boolean;
  } | null>(null);
  ownerKeyRef.current = ownerKey;

  const initialRequestEnvelope: RequestEnvelope = {
    ownerKey: null,
    state: { status: 'loading' },
  };
  const [requestEnvelope, setRequestEnvelope] = useState<RequestEnvelope>(initialRequestEnvelope);
  const requestEnvelopeRef = useRef<RequestEnvelope>(initialRequestEnvelope);
  const [mutationEnvelope, setMutationEnvelope] = useState<MutationEnvelope>({
    ownerKey: null,
    active: false,
  });
  const [announcementEnvelope, setAnnouncementEnvelope] = useState<AnnouncementEnvelope>({
    ownerKey: null,
    message: '',
  });
  const [modalEnvelope, setModalEnvelope] = useState<ModalEnvelope>({
    ownerKey: null,
    consentMode: null,
    confirmation: null,
  });

  const requestState: ActivityHistoryRequestState =
    requestEnvelope.ownerKey === ownerKey ? requestEnvelope.state : { status: 'loading' };
  const mutationActive = mutationEnvelope.ownerKey === ownerKey && mutationEnvelope.active;
  const announcement =
    announcementEnvelope.ownerKey === ownerKey ? announcementEnvelope.message : '';
  const visibleModal =
    modalEnvelope.ownerKey === ownerKey
      ? modalEnvelope
      : { ownerKey, consentMode: null, confirmation: null };
  const consentCapabilityLost =
    visibleModal.consentMode !== null && capability.status !== 'supported';
  const consentEnvelopeInvalidated =
    visibleModal.consentMode !== null && invalidatedConsentEnvelopeRef.current === modalEnvelope;
  const visibleConsentMode =
    consentCapabilityLost || consentEnvelopeInvalidated ? null : visibleModal.consentMode;

  const commitRequestState = useCallback(
    (requestOwnerKey: string, state: ActivityHistoryRequestState): boolean => {
      if (ownerKeyRef.current !== requestOwnerKey) return false;
      const nextEnvelope = { ownerKey: requestOwnerKey, state };
      requestEnvelopeRef.current = nextEnvelope;
      setRequestEnvelope(nextEnvelope);
      return true;
    },
    []
  );

  const setMutationForOwner = useCallback((requestOwnerKey: string, active: boolean): void => {
    if (ownerKeyRef.current !== requestOwnerKey) return;
    setMutationEnvelope({ ownerKey: requestOwnerKey, active });
  }, []);

  const setAnnouncementForOwner = useCallback((requestOwnerKey: string, message: string): void => {
    if (ownerKeyRef.current !== requestOwnerKey) return;
    setAnnouncementEnvelope({ ownerKey: requestOwnerKey, message });
  }, []);

  const setModalForOwner = useCallback(
    (
      requestOwnerKey: string,
      consentMode: ConsentMode | null,
      confirmation: Confirmation | null
    ): void => {
      if (ownerKeyRef.current !== requestOwnerKey) return;
      setModalEnvelope({ ownerKey: requestOwnerKey, consentMode, confirmation });
    },
    []
  );

  const beginRequest = useCallback((requestOwnerKey: string): RequestToken => {
    requestControllerRef.current?.abort();
    const controller = new AbortController();
    requestGenerationRef.current += 1;
    requestControllerRef.current = controller;
    return {
      ownerKey: requestOwnerKey,
      generation: requestGenerationRef.current,
      controller,
    };
  }, []);

  const isCurrentRequest = useCallback((request: RequestToken): boolean => {
    return (
      ownerKeyRef.current === request.ownerKey &&
      requestGenerationRef.current === request.generation &&
      requestControllerRef.current === request.controller &&
      !request.controller.signal.aborted
    );
  }, []);

  const refreshSettings = useCallback(
    async (requestOwnerKey: string, preserveConfirmed: boolean): Promise<void> => {
      const priorState =
        requestEnvelopeRef.current.ownerKey === requestOwnerKey
          ? requestEnvelopeRef.current.state
          : { status: 'loading' as const };
      const priorSettings = settingsFromState(priorState);
      const request = beginRequest(requestOwnerKey);
      if (!preserveConfirmed || priorSettings === null) {
        commitRequestState(requestOwnerKey, { status: 'loading' });
      }

      try {
        const settings = await getPresenceHistorySettings(request.controller.signal);
        if (!isCurrentRequest(request)) return;
        commitRequestState(requestOwnerKey, stateForSettings(settings));
      } catch (error) {
        if (!isCurrentRequest(request)) return;
        const routeMissing = isMissingHistoryRoute(error);
        commitRequestState(requestOwnerKey, {
          status: 'error',
          confirmed: routeMissing ? null : priorSettings,
          message: 'Activity History settings could not be refreshed.',
          routeMissing,
        });
      }
    },
    [beginRequest, commitRequestState, isCurrentRequest]
  );

  useEffect(() => {
    let active = true;
    ownerLifecycleGenerationRef.current += 1;
    requestControllerRef.current?.abort();
    requestControllerRef.current = null;
    requestGenerationRef.current += 1;
    pendingHeadingFocusOwnerRef.current = null;
    invalidatedConsentEnvelopeRef.current = null;

    const resetRequest: RequestEnvelope = {
      ownerKey,
      state: { status: 'loading' },
    };
    requestEnvelopeRef.current = resetRequest;
    // Render-time owner-envelope checks already hide the prior owner's state
    // synchronously. Commit the physical resets in a microtask so React does
    // not incur a synchronous effect-driven render, while the owner guard keeps
    // a superseded or unmounted continuation inert.
    queueMicrotask(() => {
      if (!active || ownerKeyRef.current !== ownerKey) return;
      setRequestEnvelope(resetRequest);
      setMutationEnvelope({ ownerKey, active: false });
      setAnnouncementEnvelope({ ownerKey, message: '' });
      setModalEnvelope({
        ownerKey,
        consentMode: null,
        confirmation: null,
      });
    });

    return () => {
      active = false;
      ownerLifecycleGenerationRef.current += 1;
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
      requestGenerationRef.current += 1;
      pendingHeadingFocusOwnerRef.current = null;
      settingsLoadStateRef.current = null;
      pendingSettingsRefreshRef.current = null;
    };
  }, [ownerKey]);

  useEffect(() => {
    const previous = settingsLoadStateRef.current;
    const ownerChanged = previous === null || previous.ownerKey !== ownerKey;
    const becameLoadable =
      !ownerChanged && previous !== null && !previous.canLoadSettings && canLoadSettings;
    const loadableStatusChanged =
      !ownerChanged &&
      previous !== null &&
      previous.canLoadSettings &&
      canLoadSettings &&
      previous.capabilityStatus !== capability.status &&
      (previous.capabilityStatus === 'confirmed-unsupported' ||
        capability.status === 'confirmed-unsupported');

    settingsLoadStateRef.current = {
      ownerKey,
      capabilityStatus: capability.status,
      canLoadSettings,
    };

    if (ownerKey === null) {
      pendingSettingsRefreshRef.current = null;
      return;
    }

    if (canLoadSettings && (ownerChanged || becameLoadable || loadableStatusChanged)) {
      const preserveConfirmed = !ownerChanged;
      const pending = pendingSettingsRefreshRef.current;
      pendingSettingsRefreshRef.current = {
        ownerKey,
        preserveConfirmed:
          pending?.ownerKey === ownerKey
            ? pending.preserveConfirmed && preserveConfirmed
            : preserveConfirmed,
      };
    }

    if (mutationActive || !canLoadSettings) return;
    const pending = pendingSettingsRefreshRef.current;
    if (pending === null || pending.ownerKey !== ownerKey) return;
    pendingSettingsRefreshRef.current = null;
    handleSettledPromise(refreshSettings(ownerKey, pending.preserveConfirmed));
  }, [canLoadSettings, capability.status, mutationActive, ownerKey, refreshSettings]);

  useEffect(() => {
    if (!consentCapabilityLost || ownerKey === null) return;
    const requestOwnerKey = ownerKey;
    const ownerLifecycleGeneration = ownerLifecycleGenerationRef.current;
    const invalidatedModalEnvelope = modalEnvelope;
    const invalidatedMutationEnvelope = mutationEnvelope;
    const invalidatedAnnouncementEnvelope = announcementEnvelope;
    invalidatedConsentEnvelopeRef.current = invalidatedModalEnvelope;
    requestControllerRef.current?.abort();
    requestControllerRef.current = null;
    requestGenerationRef.current += 1;
    pendingHeadingFocusOwnerRef.current = requestOwnerKey;

    queueMicrotask(() => {
      if (
        ownerKeyRef.current !== requestOwnerKey ||
        ownerLifecycleGenerationRef.current !== ownerLifecycleGeneration
      ) {
        return;
      }
      setMutationEnvelope((current) =>
        current === invalidatedMutationEnvelope
          ? { ownerKey: requestOwnerKey, active: false }
          : current
      );
      setModalEnvelope((current) =>
        current === invalidatedModalEnvelope
          ? { ownerKey: requestOwnerKey, consentMode: null, confirmation: null }
          : current
      );
      setAnnouncementEnvelope((current) =>
        current === invalidatedAnnouncementEnvelope
          ? { ownerKey: requestOwnerKey, message: CAPABILITY_WIDENING_PAUSED_MESSAGE }
          : current
      );
    });
  }, [announcementEnvelope, consentCapabilityLost, modalEnvelope, mutationEnvelope, ownerKey]);

  useEffect(() => {
    if (pendingHeadingFocusOwnerRef.current !== ownerKey) return;
    if (visibleModal.consentMode !== null || visibleModal.confirmation !== null) return;
    pendingHeadingFocusOwnerRef.current = null;
    headingRef.current?.focus();
  }, [ownerKey, visibleModal.confirmation, visibleModal.consentMode]);

  const commitMutationFailure = useCallback(
    (requestOwnerKey: string, priorSettings: PresenceHistorySettings, error: unknown): void => {
      const routeMissing = isMissingHistoryRoute(error);
      commitRequestState(requestOwnerKey, {
        status: 'error',
        confirmed: routeMissing ? null : priorSettings,
        message: mutationErrorMessage(error),
        routeMissing,
      });
      setMutationForOwner(requestOwnerKey, false);
    },
    [commitRequestState, setMutationForOwner]
  );

  const refetchAfterConflict = useCallback(
    async (
      request: RequestToken,
      priorSettings: PresenceHistorySettings
    ): Promise<PresenceHistorySettings | null> => {
      try {
        const refreshed = await getPresenceHistorySettings(request.controller.signal);
        if (!isCurrentRequest(request)) return null;
        commitRequestState(request.ownerKey, stateForSettings(refreshed));
        return refreshed;
      } catch (error) {
        if (isCurrentRequest(request)) {
          commitMutationFailure(request.ownerKey, priorSettings, error);
        }
        return null;
      }
    },
    [commitMutationFailure, commitRequestState, isCurrentRequest]
  );

  const requestHeadingFocusAfterReconsent = useCallback(
    (requestOwnerKey: string, priorSettings: PresenceHistorySettings): void => {
      if (priorSettings.reconsentRequired) {
        pendingHeadingFocusOwnerRef.current = requestOwnerKey;
      }
    },
    []
  );

  const enableHistory = useCallback(
    async (
      selection: ActivityHistoryConsentSelection,
      priorSettings: PresenceHistorySettings
    ): Promise<void> => {
      const requestOwnerKey = ownerKeyRef.current;
      if (requestOwnerKey === null) return;
      if (!liveCapabilitySupportsWidening()) {
        pendingHeadingFocusOwnerRef.current = requestOwnerKey;
        setMutationForOwner(requestOwnerKey, false);
        setModalForOwner(requestOwnerKey, null, null);
        setAnnouncementForOwner(requestOwnerKey, CAPABILITY_WIDENING_PAUSED_MESSAGE);
        return;
      }
      const request = beginRequest(requestOwnerKey);
      setMutationForOwner(requestOwnerKey, true);
      setAnnouncementForOwner(requestOwnerKey, '');

      try {
        const settings = await patchPresenceHistorySettings(
          {
            kind: 'enable',
            retentionDays: selection.retentionDays,
            consentVersion: selection.consentVersion,
            consentCopyHash: selection.consentCopyHash,
          },
          request.controller.signal
        );
        if (!isCurrentRequest(request)) return;
        commitRequestState(requestOwnerKey, stateForSettings(settings));
        setMutationForOwner(requestOwnerKey, false);
        requestHeadingFocusAfterReconsent(requestOwnerKey, priorSettings);
        setModalForOwner(requestOwnerKey, null, null);
        setAnnouncementForOwner(requestOwnerKey, 'Activity History is on.');
      } catch (error) {
        if (!isCurrentRequest(request)) return;
        if (isConsentConflict(error)) {
          const refreshed = await refetchAfterConflict(request, priorSettings);
          if (refreshed === null || !isCurrentRequest(request)) return;
          setMutationForOwner(requestOwnerKey, false);
          if (refreshed.requiredConsent === null) {
            setModalForOwner(requestOwnerKey, null, null);
          } else {
            setModalForOwner(requestOwnerKey, 'reconsent', null);
          }
          throw new Error('Activity History terms changed. Review the updated disclosure.');
        }

        commitMutationFailure(requestOwnerKey, priorSettings, error);
        throw new Error(mutationErrorMessage(error));
      }
    },
    [
      beginRequest,
      commitMutationFailure,
      commitRequestState,
      isCurrentRequest,
      refetchAfterConflict,
      requestHeadingFocusAfterReconsent,
      setAnnouncementForOwner,
      setModalForOwner,
      setMutationForOwner,
    ]
  );

  const changeRetention = useCallback(
    async (
      nextRetention: ActivityHistoryRetentionDays,
      priorSettings: PresenceHistorySettings
    ): Promise<void> => {
      const requestOwnerKey = ownerKeyRef.current;
      if (requestOwnerKey === null) return;
      if (nextRetention > priorSettings.retentionDays && !liveCapabilitySupportsWidening()) {
        setAnnouncementForOwner(requestOwnerKey, CAPABILITY_WIDENING_PAUSED_MESSAGE);
        return;
      }
      const request = beginRequest(requestOwnerKey);
      setMutationForOwner(requestOwnerKey, true);
      setAnnouncementForOwner(requestOwnerKey, '');

      try {
        const settings = await patchPresenceHistorySettings(
          { kind: 'retention', retentionDays: nextRetention },
          request.controller.signal
        );
        if (!isCurrentRequest(request)) return;
        commitRequestState(requestOwnerKey, stateForSettings(settings));
        setMutationForOwner(requestOwnerKey, false);
        setModalForOwner(requestOwnerKey, null, null);
        setAnnouncementForOwner(
          requestOwnerKey,
          `Activity History retention changed to ${nextRetention} days.`
        );
      } catch (error) {
        if (!isCurrentRequest(request)) return;
        if (isConsentConflict(error)) {
          const refreshed = await refetchAfterConflict(request, priorSettings);
          if (refreshed !== null && isCurrentRequest(request)) {
            setMutationForOwner(requestOwnerKey, false);
            setModalForOwner(requestOwnerKey, null, null);
            setAnnouncementForOwner(
              requestOwnerKey,
              'Activity History settings changed on the server. Review the current values.'
            );
          }
          return;
        }
        commitMutationFailure(requestOwnerKey, priorSettings, error);
        setModalForOwner(requestOwnerKey, null, null);
      }
    },
    [
      beginRequest,
      commitMutationFailure,
      commitRequestState,
      isCurrentRequest,
      refetchAfterConflict,
      setAnnouncementForOwner,
      setModalForOwner,
      setMutationForOwner,
    ]
  );

  const removeHistory = useCallback(
    async (kind: 'disable' | 'delete', priorSettings: PresenceHistorySettings): Promise<void> => {
      const requestOwnerKey = ownerKeyRef.current;
      if (requestOwnerKey === null) return;
      const request = beginRequest(requestOwnerKey);
      setMutationForOwner(requestOwnerKey, true);
      setAnnouncementForOwner(requestOwnerKey, '');

      try {
        if (kind === 'delete') {
          await deletePresenceHistory(request.controller.signal);
        } else {
          await patchPresenceHistorySettings({ kind: 'disable' }, request.controller.signal);
        }
        if (!isCurrentRequest(request)) return;

        const confirmed = await getPresenceHistorySettings(request.controller.signal);
        if (!isCurrentRequest(request)) return;
        commitRequestState(requestOwnerKey, stateForSettings(confirmed));
        setMutationForOwner(requestOwnerKey, false);
        setModalForOwner(requestOwnerKey, null, null);
        if (!isConfirmedOff(confirmed)) {
          setAnnouncementForOwner(
            requestOwnerKey,
            'Activity History changed in another session. Review the current settings and try again.'
          );
          return;
        }
        const message =
          kind === 'delete'
            ? 'Activity History was deleted and recording was turned off.'
            : 'Activity History was turned off and history was deleted.';
        setAnnouncementForOwner(requestOwnerKey, message);
        pendingHeadingFocusOwnerRef.current = requestOwnerKey;
      } catch (error) {
        if (!isCurrentRequest(request)) return;
        commitMutationFailure(requestOwnerKey, priorSettings, error);
        setModalForOwner(requestOwnerKey, null, null);
      }
    },
    [
      beginRequest,
      commitMutationFailure,
      commitRequestState,
      isCurrentRequest,
      setAnnouncementForOwner,
      setModalForOwner,
      setMutationForOwner,
    ]
  );

  const closeModal = (): void => {
    if (ownerKey !== null) setModalForOwner(ownerKey, null, null);
  };

  const openConsent = (mode: ConsentMode): void => {
    if (ownerKey === null) return;
    setModalForOwner(ownerKey, mode, null);
  };

  const openConfirmation = (confirmation: Confirmation): void => {
    if (ownerKey === null) return;
    setModalForOwner(ownerKey, null, confirmation);
  };

  const retryAvailability = (): void => {
    handleSettledPromise(clientConfigService.refreshServerCapabilities());
  };

  return {
    capability,
    canLoadSettings,
    ownerKey,
    headingRef,
    requestState,
    mutationActive,
    announcement,
    visibleModal,
    consentCapabilityLost,
    visibleConsentMode,
    refreshSettings,
    enableHistory,
    changeRetention,
    removeHistory,
    closeModal,
    openConsent,
    openConfirmation,
    retryAvailability,
  };
}

interface ControllerProps {
  controller: ActivityHistoryCardController;
}

interface SettingsControlProps extends ControllerProps {
  settings: PresenceHistorySettings;
  mutationsBlocked: boolean;
  canWiden: boolean;
}

function statusClassFor(status: string | null): string {
  switch (status) {
    case 'On':
      return 'activity-history-card--on';
    case 'Off':
      return 'activity-history-card--off';
    case 'Paused':
      return 'activity-history-card--paused';
    case 'Unavailable':
      return 'activity-history-card--unavailable';
    default:
      return 'activity-history-card--loading';
  }
}

function liveAnnouncementFor(controller: ActivityHistoryCardController): string {
  const { announcement, capability, consentCapabilityLost } = controller;
  if (consentCapabilityLost) return CAPABILITY_WIDENING_PAUSED_MESSAGE;
  if (
    capability.status === 'error' &&
    capability.lastConfirmedSupported &&
    announcement !== CAPABILITY_WIDENING_PAUSED_MESSAGE
  ) {
    return STALE_CAPABILITY_ANNOUNCEMENT;
  }
  if (capability.status === 'supported' && announcement === CAPABILITY_WIDENING_PAUSED_MESSAGE) {
    return '';
  }
  return announcement;
}

function retentionConfirmationFor(
  modal: ModalEnvelope
): Extract<Confirmation, { kind: 'retention-decrease' }> | null {
  if (modal.confirmation?.kind !== 'retention-decrease') return null;
  return modal.confirmation;
}

const ActivityHistoryAvailability: React.FC<ControllerProps> = ({ controller }) => {
  const { capability, canLoadSettings, ownerKey, requestState, refreshSettings } = controller;
  const capabilityUnknown = capability.status === 'error' && !capability.lastConfirmedSupported;
  const capabilityStale = capability.status === 'error' && capability.lastConfirmedSupported;

  return (
    <>
      {capability.status === 'loading' && (
        <p className="activity-history-card__message">Checking Activity History availability…</p>
      )}

      {capabilityUnknown && (
        <div className="activity-history-card__notice" role="alert" aria-atomic="true">
          <p>Activity History availability could not be confirmed.</p>
          <button type="button" className="secondary-button" onClick={controller.retryAvailability}>
            Retry availability check
          </button>
        </div>
      )}

      {capabilityStale && (
        <div className="activity-history-card__notice">
          <span>Availability not refreshed</span>
          <button type="button" className="secondary-button" onClick={controller.retryAvailability}>
            Retry availability check
          </button>
        </div>
      )}

      {capability.status === 'confirmed-unsupported' && (
        <aside
          className="activity-history-card__notice"
          aria-label="Activity History activation paused"
        >
          New opt-ins and retention increases are paused. Existing history remains available to
          view, shorten, disable, or delete.
        </aside>
      )}

      {!capabilityUnknown &&
        capability.status !== 'loading' &&
        requestState.status === 'loading' && (
          <p className="activity-history-card__message">Loading Activity History settings…</p>
        )}

      {requestState.status === 'error' && (
        <div
          className="activity-history-card__notice activity-history-card__notice--error"
          role="alert"
          aria-atomic="true"
        >
          <span>{requestState.message}</span>
          {requestState.confirmed !== null && <strong>Not refreshed</strong>}
          {ownerKey !== null && canLoadSettings && (
            <button
              type="button"
              className="secondary-button"
              onClick={() => handleSettledPromise(refreshSettings(ownerKey, true))}
            >
              Retry settings
            </button>
          )}
        </div>
      )}
    </>
  );
};

const ActivityHistoryToggleRow: React.FC<SettingsControlProps> = ({
  controller,
  settings,
  mutationsBlocked,
  canWiden,
}) => {
  const showToggle = settings.available && !settings.reconsentRequired;
  const handleToggle = (nextEnabled: boolean): void => {
    if (nextEnabled) {
      controller.openConsent('enable');
      return;
    }
    controller.openConfirmation({ kind: 'disable' });
  };

  return (
    <div className="activity-history-card__state-row">
      <p>{settingsDescription(settings)}</p>
      {showToggle && (
        <ToggleSwitch
          checked={settings.enabled}
          onChange={handleToggle}
          disabled={
            mutationsBlocked ||
            (!settings.enabled && (!canWiden || settings.requiredConsent === null))
          }
          label="Activity History"
          inputRole="switch"
        />
      )}
    </div>
  );
};

const ActivityHistoryRetention: React.FC<SettingsControlProps> = ({
  controller,
  settings,
  mutationsBlocked,
  canWiden,
}) => {
  if (!settings.enabled && !settings.reconsentRequired) {
    return (
      <div className="activity-history-card__retention">
        <label htmlFor="activity-history-retention-inactive">Retention period</label>
        <select id="activity-history-retention-inactive" value={settings.retentionDays} disabled>
          {ACTIVITY_HISTORY_RETENTION_DAYS.map((days) => (
            <option key={days} value={days}>
              {days} days
            </option>
          ))}
        </select>
      </div>
    );
  }

  const handleRetentionChange = (value: string): void => {
    const nextRetention = retentionFromValue(value);
    if (nextRetention === null || nextRetention === settings.retentionDays) return;
    if (nextRetention < settings.retentionDays) {
      controller.openConfirmation({
        kind: 'retention-decrease',
        retentionDays: nextRetention,
      });
      return;
    }
    if (canWiden && !settings.reconsentRequired) {
      handleSettledPromise(controller.changeRetention(nextRetention, settings));
    }
  };

  return (
    <div className="activity-history-card__retention">
      <label htmlFor="activity-history-retention">Retention period</label>
      <select
        id="activity-history-retention"
        value={settings.retentionDays}
        disabled={mutationsBlocked}
        onChange={(event) => handleRetentionChange(event.currentTarget.value)}
      >
        {ACTIVITY_HISTORY_RETENTION_DAYS.map((days) => (
          <option
            key={days}
            value={days}
            disabled={days > settings.retentionDays && (!canWiden || settings.reconsentRequired)}
          >
            {days} days
          </option>
        ))}
      </select>
    </div>
  );
};

const ActivityHistoryActions: React.FC<SettingsControlProps> = ({
  controller,
  settings,
  mutationsBlocked,
  canWiden,
}) => (
  <div className="activity-history-card__actions">
    {settings.reconsentRequired && settings.available && settings.requiredConsent !== null && (
      <button
        type="button"
        className="primary-button"
        disabled={mutationsBlocked || !canWiden}
        onClick={() => controller.openConsent('reconsent')}
      >
        Review updated terms
      </button>
    )}
    {(settings.enabled || settings.reconsentRequired) && (
      <button
        type="button"
        className="secondary-button"
        onClick={() =>
          useSettingsNavStore.getState().requestFocus('account', 'presence-history-heading')
        }
      >
        View history
      </button>
    )}
    {(settings.enabled || settings.reconsentRequired) && (
      <button
        type="button"
        className="activity-history-card__danger-button"
        disabled={mutationsBlocked}
        onClick={() => controller.openConfirmation({ kind: 'delete' })}
      >
        Delete history and turn off
      </button>
    )}
  </div>
);

const ActivityHistorySettingsControls: React.FC<SettingsControlProps> = (props) => (
  <>
    <ActivityHistoryToggleRow {...props} />
    <ActivityHistoryRetention {...props} />
    <ActivityHistoryActions {...props} />
  </>
);

const ActivityHistoryModals: React.FC<
  ControllerProps & { settings: PresenceHistorySettings | null }
> = ({ controller, settings }) => {
  const retentionConfirmation = retentionConfirmationFor(controller.visibleModal);

  return (
    <>
      {settings?.requiredConsent !== null &&
        settings?.requiredConsent !== undefined &&
        controller.visibleConsentMode !== null && (
          <ActivityHistoryConsentModal
            isOpen={true}
            mode={controller.visibleConsentMode}
            disclosure={settings.requiredConsent}
            retentionDays={settings.retentionDays}
            onClose={controller.closeModal}
            onSubmit={(selection) => controller.enableHistory(selection, settings)}
          />
        )}

      {settings !== null && retentionConfirmation !== null && (
        <ConfirmActionModal
          isOpen={true}
          onClose={controller.closeModal}
          title="Shorten Activity History retention?"
          message={
            <p>
              Activity History older than {retentionConfirmation.retentionDays} days will be deleted
              immediately. Expired records cannot be restored.
            </p>
          }
          confirmLabel="Delete older history"
          loadingLabel="Deleting older history…"
          onConfirm={() =>
            controller.changeRetention(retentionConfirmation.retentionDays, settings)
          }
        />
      )}

      {settings !== null && controller.visibleModal.confirmation?.kind === 'disable' && (
        <ConfirmActionModal
          isOpen={true}
          onClose={controller.closeModal}
          title="Turn off Activity History?"
          message={
            <p>All active history will be deleted immediately and recording will turn off.</p>
          }
          confirmLabel="Turn off and delete history"
          loadingLabel="Turning off…"
          onConfirm={() => controller.removeHistory('disable', settings)}
        />
      )}

      {settings !== null && controller.visibleModal.confirmation?.kind === 'delete' && (
        <ConfirmActionModal
          isOpen={true}
          onClose={controller.closeModal}
          title="Delete Activity History?"
          message={<p>Delete every Activity History record and turn off future recording.</p>}
          confirmLabel="Delete history and turn off"
          loadingLabel="Deleting history…"
          onConfirm={() => controller.removeHistory('delete', settings)}
        />
      )}
    </>
  );
};

const ActivityHistoryCardView: React.FC<ControllerProps> = ({ controller }) => {
  const settings = settingsFromState(controller.requestState);
  const staleSettings = controller.requestState.status === 'error' && settings !== null;
  const mutationsBlocked = controller.mutationActive || staleSettings;
  const canWiden = controller.capability.status === 'supported';
  const status = settings === null ? null : settingsStatus(settings);

  return (
    <article className={`activity-history-card ${statusClassFor(status)}`}>
      <div className="activity-history-card__header">
        <div>
          <h3
            id="activity-history-card-heading"
            ref={controller.headingRef}
            tabIndex={-1}
            className="activity-history-card__heading"
          >
            Activity History
          </h3>
          <p className="activity-history-card__summary">
            Keep a private, self-only timeline of supported Rich Presence activity.
          </p>
        </div>
        {status !== null && <span className="activity-history-card__status">{status}</span>}
      </div>

      <ActivityHistoryAvailability controller={controller} />

      {settings !== null && (
        <ActivityHistorySettingsControls
          controller={controller}
          settings={settings}
          mutationsBlocked={mutationsBlocked}
          canWiden={canWiden}
        />
      )}

      <output className="activity-history-card__live" aria-live="polite">
        {liveAnnouncementFor(controller)}
      </output>

      <ActivityHistoryModals controller={controller} settings={settings} />
    </article>
  );
};

const ActivityHistoryCard: React.FC = () => {
  const controller = useActivityHistoryCardController();
  if (controller.capability.status === 'confirmed-unsupported') {
    if (controller.requestState.status === 'loading') return null;
    if (
      controller.requestState.status === 'error' &&
      controller.requestState.confirmed === null &&
      controller.requestState.routeMissing
    ) {
      return null;
    }
  }
  return <ActivityHistoryCardView controller={controller} />;
};

export default ActivityHistoryCard;
