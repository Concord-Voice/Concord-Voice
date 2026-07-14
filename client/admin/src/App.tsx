import { useEffect, useState } from "react";

import { ApiError, api } from "./api";
import concordSymbol from "./assets/logos/symbol-transparent-vector.svg";
import { AuthScreen } from "./AuthScreen";
import type { AdminHealthResponse } from "./contracts";
import { EnrollScreen } from "./EnrollScreen";
import { Portal } from "./Portal";
import "./styles.css";

function Brand() {
  return (
    <header className="app-header">
      <div className="brand-lockup">
        <img alt="" className="brand-symbol" src={concordSymbol} />
        <div>
          <strong className="brand-name">Concord Voice</strong>
          <span className="product-name">Admin Portal</span>
        </div>
      </div>
      <span className="host-name">{globalThis.location.host}</span>
    </header>
  );
}

interface AppProps {
  navigate?: (path: string) => void;
  reload?: () => void;
}

function navigateDocument(path: string): void {
  globalThis.location.assign(path);
}

function reloadDocument(): void {
  globalThis.location.reload();
}

type View =
  | { kind: "loading" }
  | { kind: "login" }
  | { kind: "reloading" }
  | { kind: "unverified" }
  | { health: AdminHealthResponse; kind: "portal" };

interface EntryContentProps {
  isEnrollment: boolean;
  navigate: (path: string) => void;
  onAuthenticated: () => void;
  onRetry: () => void;
  view: View;
}

function EntryContent({
  isEnrollment,
  navigate,
  onAuthenticated,
  onRetry,
  view,
}: Readonly<EntryContentProps>) {
  if (isEnrollment) {
    return <EnrollScreen onContinue={() => navigate("/admin/")} />;
  }
  if (view.kind === "login") {
    return <AuthScreen onAuthenticated={onAuthenticated} />;
  }
  if (view.kind === "reloading") {
    return <output>Reconnecting to Access</output>;
  }
  if (view.kind === "unverified") {
    return (
      <section
        aria-labelledby="session-verification-title"
        className="auth-panel"
      >
        <h1 id="session-verification-title">Unable to verify session</h1>
        <output className="form-status">
          The Admin Portal could not confirm whether you are signed in.
        </output>
        <button type="button" onClick={onRetry}>
          Retry
        </button>
      </section>
    );
  }
  return <output>Loading Admin Portal</output>;
}

export function App({
  navigate = navigateDocument,
  reload = reloadDocument,
}: Readonly<AppProps> = {}) {
  const isEnrollment = globalThis.location.pathname === "/admin/enroll";
  const [view, setView] = useState<View>({ kind: "loading" });
  const [probeGeneration, setProbeGeneration] = useState(0);

  useEffect(() => {
    if (isEnrollment) return;
    let active = true;
    const controller = new AbortController();
    void api
      .getHealth({ signal: controller.signal })
      .then((health) => {
        if (active) setView({ health, kind: "portal" });
      })
      .catch((cause: unknown) => {
        if (!active) return;
        if (cause instanceof ApiError && cause.status === 401) {
          setView({ kind: "login" });
          return;
        }
        if (cause instanceof ApiError && cause.status === 403) {
          setView({ kind: "reloading" });
          reload();
          return;
        }
        setView({ kind: "unverified" });
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [isEnrollment, probeGeneration, reload]);

  const retrySessionProbe = () => {
    setView({ kind: "loading" });
    setProbeGeneration((generation) => generation + 1);
  };

  return (
    <div className="app-shell">
      <Brand />
      {!isEnrollment && view.kind === "portal" ? (
        <Portal
          initialHealth={view.health}
          onForbidden={reload}
          onSessionEnded={() => setView({ kind: "login" })}
        />
      ) : (
        <main className="app-main">
          <EntryContent
            isEnrollment={isEnrollment}
            navigate={navigate}
            onAuthenticated={retrySessionProbe}
            onRetry={retrySessionProbe}
            view={view}
          />
        </main>
      )}
    </div>
  );
}
