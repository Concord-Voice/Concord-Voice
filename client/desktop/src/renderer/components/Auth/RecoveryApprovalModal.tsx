import { useCallback, useEffect, useId, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import DeviceRecoveryFingerprint from './DeviceRecoveryFingerprint';
import {
  ResponderDeviceRecoveryAttempt,
  rejectDeviceRecoveryRequest,
  type DeviceRecoveryView,
  type ReviewableDeviceRecoveryRequest,
} from '../../services/system/deviceRecoveryService';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
} from '../../services/system/runtimeServerBase';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { useUserStore } from '../../stores/auth/userStore';

interface RecoveryApprovalModalProps {
  readonly request: ReviewableDeviceRecoveryRequest;
  readonly onClose: () => void;
  readonly onResolved?: (requestId: string) => void;
}
const initialView: DeviceRecoveryView = {
  status: 'creating',
  fingerprint: '',
  confirmed: false,
  error: '',
  retryAt: 0,
};

// This bounded action deliberately outlives real unmount. Its caller owns the
// returned cancellation when React replays setup or replaces the request.
function deferUnmountRejection(reject: () => void): () => void {
  const timer = setTimeout(reject, 0);
  return () => clearTimeout(timer);
}

function submitUnmountRejection(
  request: ReviewableDeviceRecoveryRequest,
  assertContext: () => void
): void {
  void rejectDeviceRecoveryRequest(request, assertContext).catch(() => {
    // Best-effort cleanup leaves the row unresolved without an acknowledgement.
  });
}

export default function RecoveryApprovalModal({
  request,
  onClose,
  onResolved,
}: RecoveryApprovalModalProps) {
  const [view, setView] = useState(initialView);
  const [busy, setBusy] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [rejectionRetryAt, setRejectionRetryAt] = useState(0);
  const rejectionRetryRef = useRef(0);
  const [rejectionError, setRejectionError] = useState('');
  const attemptRef = useRef<ResponderDeviceRecoveryAttempt | null>(null);
  const descriptionRef = useRef<HTMLParagraphElement>(null);
  const descriptionId = useId();
  const mountedRef = useRef(false);
  const busyRef = useRef(false);
  const rejectionStartedRef = useRef(false);
  const settledRef = useRef(false);
  const modalGuardRef = useRef<(() => void) | null>(null);
  const detachedGuardRef = useRef<(() => void) | null>(null);
  const unmountRejectionRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    // StrictMode replays cleanup/setup synchronously; a replacement request also
    // retires the old cleanup's remote action before it can be dispatched.
    if (unmountRejectionRef.current !== null) {
      unmountRejectionRef.current();
      unmountRejectionRef.current = null;
    }
    mountedRef.current = true;
    const retryClock = setInterval(() => setNow(Date.now()), 1000);
    const auth = captureAuthLifecycle();
    const server = captureRuntimeServerSelection();
    const epoch = e2eeService.captureTeardownEpoch();
    const userId = useUserStore.getState().user?.id;
    const assertContext = () => {
      if (
        !isSameAuthLifecycle(auth) ||
        !runtimeServerSelectionIsCurrent(server) ||
        e2eeService.wasTornDownSince(epoch) ||
        useUserStore.getState().user?.id !== userId ||
        Date.now() >= request.expires_at
      )
        throw new Error('Recovery changed or expired. Restart recovery.');
    };
    modalGuardRef.current = () => {
      if (!mountedRef.current) throw new Error('Recovery comparison was closed.');
      assertContext();
    };
    detachedGuardRef.current = assertContext;
    const attempt = new ResponderDeviceRecoveryAttempt(request, (next) => {
      if (next.status === 'submitted') settledRef.current = true;
      if (mountedRef.current && attemptRef.current === attempt) setView(next);
    });
    attemptRef.current = attempt;
    void attempt.start();
    return () => {
      mountedRef.current = false;
      clearInterval(retryClock);
      modalGuardRef.current = null;
      detachedGuardRef.current = null;
      attempt.dispose();
      if (attemptRef.current === attempt) attemptRef.current = null;
      if (
        !settledRef.current &&
        !rejectionStartedRef.current &&
        Date.now() >= rejectionRetryRef.current
      ) {
        const cancel = deferUnmountRejection(() => {
          if (unmountRejectionRef.current !== cancel || mountedRef.current) return;
          unmountRejectionRef.current = null;
          if (
            settledRef.current ||
            rejectionStartedRef.current ||
            Date.now() < rejectionRetryRef.current
          )
            return;
          rejectionStartedRef.current = true;
          submitUnmountRejection(request, assertContext);
        });
        unmountRejectionRef.current = cancel;
      }
    };
  }, [request]);

  const close = useCallback(() => {
    const guard = detachedGuardRef.current;
    // Reject begins while this modal owns the context. A late acknowledgement cannot mutate a new session.
    if (
      !settledRef.current &&
      !rejectionStartedRef.current &&
      guard &&
      Date.now() >= rejectionRetryRef.current
    ) {
      rejectionStartedRef.current = true;
      void rejectDeviceRecoveryRequest(request, guard)
        .then(() => {
          guard();
          onResolved?.(request.request_id);
        })
        .catch(() => {});
    }
    attemptRef.current?.dispose();
    onClose();
  }, [onClose, onResolved, request]);

  const confirm = async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    const attempt = attemptRef.current;
    try {
      if (attempt && (await attempt.confirmMatch()) && mountedRef.current)
        onResolved?.(request.request_id);
    } finally {
      busyRef.current = false;
      if (mountedRef.current) setBusy(false);
    }
  };
  const reject = async () => {
    if (busyRef.current || !modalGuardRef.current || Date.now() < rejectionRetryRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setRejectionError('');
    rejectionStartedRef.current = true;
    try {
      await rejectDeviceRecoveryRequest(request, modalGuardRef.current);
      if (mountedRef.current) {
        settledRef.current = true;
        attemptRef.current?.dispose();
        onResolved?.(request.request_id);
        setView({ ...initialView, status: 'rejected' });
      }
    } catch (error) {
      rejectionStartedRef.current = false;
      if (
        error &&
        typeof error === 'object' &&
        'retryAfterMs' in error &&
        typeof error.retryAfterMs === 'number'
      ) {
        rejectionRetryRef.current = Date.now() + error.retryAfterMs;
        if (mountedRef.current) setRejectionRetryAt(rejectionRetryRef.current);
      }
      if (mountedRef.current)
        setRejectionError(
          error instanceof Error ? error.message : 'Rejection was not acknowledged. Retry.'
        );
    } finally {
      busyRef.current = false;
      if (mountedRef.current) setBusy(false);
    }
  };
  const retry = async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    try {
      await attemptRef.current?.retryOffer();
    } finally {
      busyRef.current = false;
      if (mountedRef.current) setBusy(false);
    }
  };

  return (
    <Modal
      isOpen
      onClose={close}
      title="Account Recovery Request"
      initialFocusRef={descriptionRef}
      describedById={descriptionId}
    >
      <div className="device-recovery-ceremony">
        <p
          ref={descriptionRef}
          id={descriptionId}
          tabIndex={-1}
          className="device-recovery-description"
        >
          Compare all eight fingerprint groups with your recovering device directly. Confirm only
          when every character matches. Never send the fingerprint to support or approve an
          unexpected request. Both devices must run an updated version of Concord.
        </p>
        {view.fingerprint && <DeviceRecoveryFingerprint fingerprint={view.fingerprint} />}
        <p role="status" aria-live="polite">
          {view.status === 'creating' && 'Preparing the comparison. Account keys remain locked.'}
          {view.status === 'offered' &&
            'Ready to compare. Your account key remains locked until you confirm.'}
          {view.status === 'submitted' &&
            'Approval submitted. Confirm the fingerprint on the recovering device to continue.'}
          {view.status === 'rejected' && 'Rejection acknowledged.'}
        </p>
        {(view.error || rejectionError) && <p role="alert">{rejectionError || view.error}</p>}
        <div className="device-recovery-actions">
          {view.status !== 'submitted' && view.status !== 'rejected' && (
            <>
              <button
                className="btn btn-primary"
                onClick={confirm}
                disabled={
                  busy || view.status !== 'offered' || !view.fingerprint || now < view.retryAt
                }
              >
                {busy ? 'Submitting…' : 'These fingerprints match'}
              </button>
              <button
                className="btn btn-secondary"
                onClick={reject}
                disabled={busy || now < rejectionRetryAt}
              >
                Reject
              </button>
              {view.status === 'creating' && view.error && view.retryAt > 0 && (
                <button
                  className="btn btn-secondary"
                  onClick={retry}
                  disabled={busy || now < view.retryAt}
                >
                  Retry comparison
                </button>
              )}
            </>
          )}
          <button className="btn btn-secondary" onClick={close}>
            Close
          </button>
        </div>
      </div>
    </Modal>
  );
}
