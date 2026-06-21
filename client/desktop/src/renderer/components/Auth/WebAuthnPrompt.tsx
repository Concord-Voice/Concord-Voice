import React, { useEffect, useRef, useState } from 'react';

/** Extract an error message from a WebAuthn ceremony failure. */
function extractCeremonyErrorMessage(err: unknown): string {
  if (err instanceof DOMException && err.name === 'NotAllowedError') {
    return 'Request cancelled or timed out. Try again.';
  }
  if (err instanceof DOMException && err.name === 'AbortError') {
    return ''; // Silently ignore aborts from cleanup
  }
  if (err instanceof Error) {
    return err.message;
  }
  return 'WebAuthn verification failed';
}

interface WebAuthnPromptProps {
  /** For registration: CredentialCreationOptions from the server */
  creationOptions?: PublicKeyCredentialCreationOptions;
  /** For login: CredentialRequestOptions from the server */
  requestOptions?: PublicKeyCredentialRequestOptions;
  onSuccess: (credential: Credential) => void;
  onError: (error: string) => void;
  onCancel?: () => void;
}

const WebAuthnPrompt: React.FC<WebAuthnPromptProps> = ({
  creationOptions,
  requestOptions,
  onSuccess,
  onError,
  onCancel,
}) => {
  const [status, setStatus] = useState<'waiting' | 'error'>('waiting');
  const [errorMsg, setErrorMsg] = useState('');

  // Use refs for callbacks to avoid re-triggering the WebAuthn ceremony
  // when parent re-renders with new function references
  const onSuccessRef = useRef(onSuccess);
  const onErrorRef = useRef(onError);
  onSuccessRef.current = onSuccess;
  onErrorRef.current = onError;

  useEffect(() => {
    if (!navigator.credentials) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: sets error status when WebAuthn is unavailable on mount; not a render loop
      setStatus('error');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: sets error message when WebAuthn is unavailable on mount; not a render loop
      setErrorMsg('WebAuthn is not supported in this browser.');
      onErrorRef.current('WebAuthn not supported');
      return;
    }

    const abortController = new AbortController();

    const performCeremony = async () => {
      try {
        let credential: Credential | null = null;

        if (creationOptions) {
          credential = await navigator.credentials.create({
            publicKey: creationOptions,
            signal: abortController.signal,
          });
        } else if (requestOptions) {
          credential = await navigator.credentials.get({
            publicKey: requestOptions,
            signal: abortController.signal,
          });
        }

        if (abortController.signal.aborted) return;

        if (!credential) {
          throw new Error('No credential returned');
        }

        onSuccessRef.current(credential);
      } catch (err) {
        if (abortController.signal.aborted) return;
        const message = extractCeremonyErrorMessage(err);
        if (message) {
          setStatus('error');
          setErrorMsg(message);
          onErrorRef.current(message);
        }
      }
    };

    performCeremony();

    return () => {
      abortController.abort();
    };
  }, [creationOptions, requestOptions]);

  return (
    <div className="webauthn-prompt">
      {status === 'waiting' && (
        <>
          <div className="webauthn-icon">
            <svg
              width="48"
              height="48"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
            >
              <path d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z" />
            </svg>
          </div>
          <p className="webauthn-status">Waiting for security key...</p>
          <p className="webauthn-hint">
            Touch your security key, use your fingerprint reader, or follow your browser&apos;s
            prompt.
          </p>
        </>
      )}
      {status === 'error' && (
        <>
          <p className="webauthn-error">{errorMsg}</p>
          {onCancel && (
            <button type="button" className="btn btn-secondary" onClick={onCancel}>
              Try another method
            </button>
          )}
        </>
      )}
    </div>
  );
};

export default WebAuthnPrompt;
