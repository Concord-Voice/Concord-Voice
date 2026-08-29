import { useEffect, useRef, useState } from "react";

import { ApiContractError, ApiError, api } from "./api";
import { decodeCreationOptions, encodeAttestation } from "./webauthn";

interface EnrollScreenProps {
  onContinue: () => void;
}

interface EnrollmentInvite {
  username: string;
  token: string;
}

interface Ceremony {
  handle: string;
  options: PublicKeyCredentialCreationOptions;
}

function readInvite(): EnrollmentInvite {
  const params = new URLSearchParams(globalThis.location.search);
  const invite = {
    username: params.get("username") ?? "",
    token: params.get("token") ?? "",
  };
  if (globalThis.location.search) {
    globalThis.history.replaceState(null, "", "/admin/enroll");
  }
  return invite;
}

// Shown when the server answered but sent a shape this console rejects.
// Distinct from both the invitation and the security-key messages on purpose:
// blaming either would send the operator hunting the wrong fault (#3005).
const UNREADABLE_RESPONSE =
  "The server's response was not recognized. This console may be out of date.";

export function EnrollScreen({ onContinue }: Readonly<EnrollScreenProps>) {
  const [invite] = useState(readInvite);
  const [password, setPassword] = useState("");
  const [credentialName, setCredentialName] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  const [complete, setComplete] = useState(false);
  const [ceremony, setCeremony] = useState<Ceremony | null>(null);
  const errorRef = useRef<HTMLParagraphElement>(null);

  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  const createCredential = async (next: Ceremony) => {
    setBusy(true);
    setStatus("Touch your security key");
    setError("");
    try {
      const credential = await navigator.credentials.create({
        publicKey: next.options,
      });
      if (!credential) {
        setError("Security key did not return a credential.");
        return;
      }
      await api.enrollFinish(
        next.handle,
        encodeAttestation(credential as PublicKeyCredential),
        credentialName,
      );
      setPassword("");
      setStatus("");
      setComplete(true);
      setCeremony(null);
    } catch (cause) {
      if (cause instanceof DOMException && cause.name === "NotAllowedError") {
        setError("Security key enrollment was canceled.");
        return;
      }
      if (cause instanceof ApiContractError) {
        setCeremony(null);
        setError(UNREADABLE_RESPONSE);
        return;
      }
      if (cause instanceof ApiError) {
        setCeremony(null);
        setError("Enrollment failed. Check the invitation and try again.");
        return;
      }
      setError("Security-key challenge could not be used. Start again.");
    } finally {
      setBusy(false);
    }
  };

  const begin = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      const response = await api.enrollBegin(
        invite.username,
        password,
        invite.token,
      );
      const next = {
        handle: response.handle,
        options: decodeCreationOptions(response.publicKey),
      };
      setCeremony(next);
      await createCredential(next);
    } catch (cause) {
      setError(
        cause instanceof ApiContractError
          ? UNREADABLE_RESPONSE
          : "Enrollment failed. Check the invitation and try again.",
      );
      setBusy(false);
    }
  };

  return (
    <section aria-labelledby="enrollment-title" className="auth-panel">
      <h1 id="enrollment-title">Enroll administrator</h1>
      {complete ? (
        <>
          <h2>Security key enrolled</h2>
          <p>Use Continue to the Admin Portal when you are ready.</p>
          <button type="button" onClick={onContinue}>
            Continue to Admin Portal
          </button>
        </>
      ) : (
        <>
          {error ? (
            <p ref={errorRef} role="alert" className="form-error" tabIndex={-1}>
              {error}
            </p>
          ) : null}
          {status ? <output className="form-status">{status}</output> : null}
          <form onSubmit={begin}>
            <label htmlFor="enroll-password">Password</label>
            <input
              id="enroll-password"
              autoComplete="current-password"
              disabled={busy}
              type="password"
              value={password}
              onChange={(event) => setPassword(event.currentTarget.value)}
            />
            <label htmlFor="credential-name">Credential name</label>
            <input
              id="credential-name"
              autoComplete="off"
              disabled={busy}
              value={credentialName}
              onChange={(event) => setCredentialName(event.currentTarget.value)}
            />
            <button
              disabled={busy || !password || !credentialName}
              type="submit"
            >
              {busy ? "Enrolling" : "Enroll security key"}
            </button>
          </form>
          {ceremony && !busy ? (
            <button
              type="button"
              onClick={() => void createCredential(ceremony)}
            >
              Try again
            </button>
          ) : null}
        </>
      )}
    </section>
  );
}
