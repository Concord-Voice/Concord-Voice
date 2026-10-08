/**
 * State and actions behind the "require MFA for dangerous actions" switch
 * (#3456 §3.5), one server at a time. `MfaEnforcementSetting` owns the markup
 * and every sentence; this owns what is known and when it is read.
 *
 * - **The setting** is read once on mount and again on this server's
 *   `server_permissions_changed`, through the same 0-5 s jittered, coalesced
 *   timer the permission refresh uses (§3.8). A read that began before a
 *   confirmed write is aborted rather than allowed to land on top of it, and a
 *   failed REFRESH keeps the value already shown: only the first read can fail
 *   the row. A later refresh that finds the setting ON clears an earlier ON
 *   error, which that value contradicts; the read a failed ON starts itself
 *   does not, because it qualifies that failure rather than answering it.
 * - **Enrolment** is the step-up requirements read (`GET /mfa/step-up`, P1),
 *   never `/mfa/status` (C-7). It runs only once the switch is shown, so a
 *   member who may not see the setting never spends the read's bucket, and it
 *   re-runs on `permissions_changed`. Only `ready` with no usable method says
 *   "unenrolled"; every failure says "unknown", and unknown leaves ON live.
 * - **ON** takes no code and is never optimistic: the value changes on the
 *   server's 200 and not before.
 * - **OFF** is not here. It needs the confirmation dialog, which the component
 *   owns; it calls `settleOff` once the server has said yes.
 *
 * Nothing here is logged, and nothing is stored: enrolment is account posture.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { getWebSocketService } from '../../services/messaging/websocketService';
import {
  fetchMfaEnforcement,
  putMfaEnforcement,
  type MfaEnforcementRead,
  type MfaEnforcementWrite,
} from '../../services/system/mfaEnforcementApi';
import { createJitteredRefetch } from '../../services/system/permissionRefresh';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
} from '../../services/system/requestContext';
import {
  fetchStepUpRequirements,
  type StepUpRequirementsResult,
} from '../../services/system/stepUpRequirements';
import { adaptDangerousActionRefusal } from '../../services/system/stepUpRouteAdapters';
import { useAuthStore } from '../../stores/auth/authStore';
import {
  useClientConfigStore,
  type MfaEnforcementCapabilityState,
} from '../../stores/ui/clientConfigStore';

export type EnforcementRead =
  /** The first read is out: nothing is shown and no space is reserved. */
  | { kind: 'pending' }
  /** 403 or 404: the member may not see the setting, so there is no switch. */
  | { kind: 'absent' }
  /** The first read failed some other way: a row with no value and a Retry. */
  | { kind: 'failed' }
  | { kind: 'ready'; enforcing: boolean };

export type Enrolment = 'unknown' | 'unenrolled' | 'enrolled';

/** Why turning ON is refused before it is sent, or null when it is not. */
export type OnBlock = 'unsupported' | 'unenrolled' | null;

/** Why turning ON failed, when the answer was not "enrol first". */
export type TurnOnError = { kind: 'transport' } | { kind: 'refused'; status: number };

/** What the polite status region says; the component words it. */
export type Announcement = 'on' | 'off' | 'enrolment';

export interface MfaEnforcementModel {
  read: EnforcementRead;
  enrolment: Enrolment;
  capability: MfaEnforcementCapabilityState['status'];
  onBlock: OnBlock;
  turningOn: boolean;
  error: TurnOnError | null;
  announcement: Announcement | null;
  retry: () => void;
  turnOn: () => void;
  /** The OFF confirmation succeeded for this server. */
  settleOff: () => void;
}

function nextRead(previous: EnforcementRead, result: MfaEnforcementRead): EnforcementRead {
  switch (result.kind) {
    case 'ok':
      return { kind: 'ready', enforcing: result.enforcing };
    case 'absent':
      return { kind: 'absent' };
    case 'unavailable':
      // A failed refresh says nothing new: only a read that has never
      // succeeded turns the row into the Retry row.
      return previous.kind === 'pending' || previous.kind === 'failed'
        ? { kind: 'failed' }
        : previous;
    case 'aborted':
      return previous;
  }
}

/** The enrolment a requirements read settled on, or null to discard it. */
function enrolmentOf(result: StepUpRequirementsResult): Enrolment | null {
  switch (result.kind) {
    case 'aborted':
      return null;
    case 'ready':
      return result.methods.length === 0 ? 'unenrolled' : 'enrolled';
    default:
      return 'unknown';
  }
}

function onBlockOf(
  read: EnforcementRead,
  capability: MfaEnforcementCapabilityState['status'],
  enrolment: Enrolment
): OnBlock {
  if (read.kind !== 'ready' || read.enforcing) return null;
  if (capability === 'confirmed-unsupported') return 'unsupported';
  return enrolment === 'unenrolled' ? 'unenrolled' : null;
}

export function useMfaEnforcement(serverId: string): MfaEnforcementModel {
  const capability = useClientConfigStore((state) => state.mfaEnforcementCapability.status);
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const [read, setRead] = useState<EnforcementRead>({ kind: 'pending' });
  const [enrolment, setEnrolment] = useState<Enrolment>('unknown');
  const [enrolmentEpoch, setEnrolmentEpoch] = useState(0);
  const [turningOn, setTurningOn] = useState(false);
  const [error, setError] = useState<TurnOnError | null>(null);
  const [announcement, setAnnouncement] = useState<Announcement | null>(null);
  const readAbortRef = useRef<AbortController | null>(null);
  const turningOnRef = useRef(false);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  /** Drops any read still out: it began before a write the server has confirmed. */
  const supersedeReads = useCallback(() => {
    readAbortRef.current?.abort();
    readAbortRef.current = null;
  }, []);

  const fetchSetting = useCallback(
    (keepError: boolean) => {
      readAbortRef.current?.abort();
      const controller = new AbortController();
      readAbortRef.current = controller;
      // An answer for an account no longer signed in is that account's, not this row's.
      const context = captureApiRequestContext();
      void fetchMfaEnforcement(serverId, controller.signal).then((result) => {
        if (controller.signal.aborted || !apiRequestContextIsCurrent(context)) return;
        setRead((previous) => nextRead(previous, result));
        // An ON error is about an attempt to turn it ON; a setting now seen ON
        // contradicts it. A setting seen OFF leaves it, which is still true.
        if (!keepError && result.kind === 'ok' && result.enforcing) setError(null);
      });
    },
    [serverId]
  );

  const refresh = useCallback(() => fetchSetting(false), [fetchSetting]);

  useEffect(() => {
    refresh();
    return supersedeReads;
  }, [refresh, supersedeReads]);

  const switchShown = read.kind === 'ready';
  useEffect(() => {
    if (!switchShown) return;
    const controller = new AbortController();
    void fetchStepUpRequirements(controller.signal).then((result) => {
      const settled = enrolmentOf(result);
      if (settled !== null) setEnrolment(settled);
    });
    return () => controller.abort();
  }, [switchShown, enrolmentEpoch]);

  useEffect(() => {
    const ws = getWebSocketService();
    const refetch = createJitteredRefetch(refresh);
    const offServer = ws.on('server_permissions_changed', (event) => {
      if (event.data.server_id === serverId) refetch.request();
    });
    const offOwn = ws.on('permissions_changed', () => setEnrolmentEpoch((epoch) => epoch + 1));
    return () => {
      offServer();
      offOwn();
      refetch.cancel();
    };
    // `authGeneration`: an account change cancels a re-read the previous account's event armed.
  }, [serverId, refresh, authGeneration]);

  const applyTurnOn = useCallback(
    (result: MfaEnforcementWrite) => {
      switch (result.kind) {
        case 'ok':
          supersedeReads();
          setRead({ kind: 'ready', enforcing: true });
          setAnnouncement('on');
          return;
        case 'aborted':
          return;
        case 'transport':
          // The change may have applied and only the answer been lost.
          setError({ kind: 'transport' });
          fetchSetting(true);
          return;
        case 'refused':
          // `turnOn` aborted any read in flight before sending, so a refused ON
          // restarts it: the row would otherwise keep a value older than the
          // `server_permissions_changed` that triggered that read.
          fetchSetting(true);
          if (
            adaptDangerousActionRefusal(result.status, result.body)?.kind === 'enrollmentRequired'
          ) {
            setEnrolment('unenrolled');
            setAnnouncement('enrolment');
            return;
          }
          setError({ kind: 'refused', status: result.status });
      }
    },
    [fetchSetting, supersedeReads]
  );

  const turnOn = useCallback(() => {
    if (turningOnRef.current) return;
    turningOnRef.current = true;
    setTurningOn(true);
    setError(null);
    setAnnouncement(null);
    supersedeReads();
    const context = captureApiRequestContext();
    void putMfaEnforcement(serverId, { enabled: true }, context).then((result) => {
      turningOnRef.current = false;
      if (!mountedRef.current) return;
      setTurningOn(false);
      // An answer for an account or server no longer current belongs to the old one.
      if (apiRequestContextIsCurrent(context)) applyTurnOn(result);
    });
  }, [serverId, supersedeReads, applyTurnOn]);

  const settleOff = useCallback(() => {
    supersedeReads();
    setRead({ kind: 'ready', enforcing: false });
    setError(null);
    setAnnouncement('off');
  }, [supersedeReads]);

  return {
    read,
    enrolment,
    capability,
    onBlock: onBlockOf(read, capability, enrolment),
    turningOn,
    error,
    announcement,
    retry: refresh,
    turnOn,
    settleOff,
  };
}
