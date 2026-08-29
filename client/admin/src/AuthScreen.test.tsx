import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { AuthScreen } from "./AuthScreen";
import { server } from "./test/server";

const base = "http://localhost";

describe("AuthScreen", () => {
  it("completes password plus WebAuthn and clears password after success", async () => {
    const onAuthenticated = vi.fn();
    const credential = assertionCredential();
    let resolveCredential: (value: PublicKeyCredential) => void = () => {};
    vi.spyOn(navigator.credentials, "get").mockReturnValue(
      new Promise<PublicKeyCredential>((resolve) => {
        resolveCredential = resolve;
      }),
    );
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, () =>
        HttpResponse.json({
          handle: "login-handle",
          publicKey: { publicKey: requestOptions() },
        }),
      ),
      http.post(`${base}/admin/api/v1/auth/webauthn`, async ({ request }) => {
        expect(await request.json()).toMatchObject({
          handle: "login-handle",
          assertion: { id: "credential" },
        });
        return HttpResponse.json({ status: "authenticated" });
      }),
    );

    render(<AuthScreen onAuthenticated={onAuthenticated} />);
    await userEvent.type(screen.getByLabelText("Username"), "operator");
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));

    expect(await screen.findByRole("status")).toHaveTextContent(
      "Touch your security key",
    );
    resolveCredential(credential);
    await waitFor(() => expect(onAuthenticated).toHaveBeenCalledTimes(1));
    // The password is cleared in the same continuation that calls
    // onAuthenticated, so that re-render is still pending when the spy gate
    // above resolves. Wait on the DOM, not on the spy.
    await waitFor(() =>
      expect(screen.getByLabelText("Password")).toHaveValue(""),
    );
  });

  it("shows generic password failure text and blocks duplicate submits", async () => {
    let attempts = 0;
    let releaseRequest: () => void = () => {};
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, async () => {
        attempts += 1;
        await new Promise<void>((resolve) => {
          releaseRequest = resolve;
        });
        return HttpResponse.json(
          { error: "password token assertion session leaked body" },
          { status: 401 },
        );
      }),
    );

    render(<AuthScreen onAuthenticated={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Username"), "operator");
    await userEvent.type(screen.getByLabelText("Password"), "wrong");
    const button = screen.getByRole("button", { name: "Continue" });
    await userEvent.click(button);
    await userEvent.click(button);
    // `attempts` increments immediately before the handler constructs its
    // promise, so attempts === 1 proves the executor ran and releaseRequest is
    // no longer the no-op initializer. Calling it earlier would never resolve.
    await waitFor(() => expect(attempts).toBe(1));
    releaseRequest();

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(
      "Sign-in failed. Check your credentials and try again.",
    );
    // findByRole gates on the alert EXISTING; focus is applied by a separate
    // post-render effect, so it needs its own wait. Re-queried inside the
    // callback per [internal]rules/tests.md § Async assertions.
    //
    // The negative assertion below stays bare deliberately: waitFor resolves
    // at the first poll where its condition holds, so a leak rendered a tick
    // later would slip past it.
    await waitFor(() => expect(screen.getByRole("alert")).toHaveFocus());
    expect(alert).not.toHaveTextContent(/password|token|assertion|session/i);
    expect(attempts).toBe(1);
  });

  // #3005: a response the server delivered fine but whose SHAPE api.ts rejects
  // is not a credential problem and not a security-key problem. Before this,
  // exactKeys threw ApiError(0) and the operator was told the security-key
  // challenge failed — sending them to the hardware for a schema mismatch.
  // The WebAuthn leg has its own catch. Before #3005 a malformed /auth/webauthn
  // response landed on "Sign-in failed. Check your credentials" — blaming the
  // operator for a server/client schema mismatch after their key already
  // succeeded.
  it("reports an unrecognized webauthn response without blaming the credentials", async () => {
    const credential = assertionCredential();
    vi.spyOn(navigator.credentials, "get").mockResolvedValue(credential);
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, () =>
        HttpResponse.json({
          handle: "login-handle",
          publicKey: { publicKey: requestOptions() },
        }),
      ),
      http.post(`${base}/admin/api/v1/auth/webauthn`, () =>
        HttpResponse.json({ status: "not-the-status-we-asked-for" }),
      ),
    );

    render(<AuthScreen onAuthenticated={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Username"), "operator");
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(
      "The server's response was not recognized. This console may be out of date.",
    );
    expect(alert).not.toHaveTextContent(/credentials|security.key/i);
  });

  it("reports an unrecognized response shape as neither a credential nor a key fault", async () => {
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, () =>
        HttpResponse.json({ handle: "operator", unexpected: "field" }),
      ),
    );
    render(<AuthScreen onAuthenticated={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Username"), "operator");
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(
      "The server's response was not recognized. This console may be out of date.",
    );
    expect(alert).not.toHaveTextContent(/security.key|credentials/i);
    expect(alert).not.toHaveTextContent(/correct horse|operator/i);
  });

  it("handles malformed options, browser cancellation retry, null credential, and redemption failure", async () => {
    const get = vi.spyOn(navigator.credentials, "get");
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, () =>
        HttpResponse.json({
          handle: "bad-handle",
          publicKey: { publicKey: { challenge: "*" } },
        }),
      ),
    );
    render(<AuthScreen onAuthenticated={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Username"), "operator");
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Security-key challenge could not be used. Start again.",
    );

    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, () =>
        HttpResponse.json({
          handle: "retry-handle",
          publicKey: { publicKey: requestOptions() },
        }),
      ),
      http.post(`${base}/admin/api/v1/auth/webauthn`, () =>
        HttpResponse.json(
          { error: "single-use handle expired" },
          { status: 401 },
        ),
      ),
    );
    get.mockRejectedValueOnce(
      new DOMException("The operation was aborted.", "NotAllowedError"),
    );
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Security key verification was canceled.",
      ),
    );
    get.mockResolvedValueOnce(null);
    await userEvent.click(
      screen.getByRole("button", { name: "Retry security key" }),
    );
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Security key did not return a credential.",
      ),
    );
    get.mockResolvedValueOnce(assertionCredential());
    await userEvent.click(
      screen.getByRole("button", { name: "Retry security key" }),
    );
    expect(screen.getByLabelText("Password")).toBeVisible();
    // The alert is already mounted carrying the PREVIOUS message here, so gate
    // on the text rather than on the element existing.
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Sign-in failed. Check your credentials and try again.",
      ),
    );
  });

  it("does not persist or log password, token, WebAuthn, or session material", async () => {
    const consoleError = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});
    const consoleWarn = vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.spyOn(navigator.credentials, "get").mockResolvedValue(
      assertionCredential(),
    );
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, () =>
        HttpResponse.json({
          handle: "login-handle",
          publicKey: { publicKey: requestOptions() },
        }),
      ),
      http.post(`${base}/admin/api/v1/auth/webauthn`, () =>
        HttpResponse.json(
          { error: "password token assertion session leaked body" },
          { status: 401 },
        ),
      ),
    );

    render(<AuthScreen onAuthenticated={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Username"), "operator");
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));

    expect(await screen.findByRole("alert")).not.toHaveTextContent(
      /correct horse|token|assertion|session/i,
    );
    expect(`${localStorage.length}${sessionStorage.length}`).toBe("00");
    expect(consoleError).not.toHaveBeenCalled();
    expect(consoleWarn).not.toHaveBeenCalled();
  });
});

function requestOptions() {
  return {
    challenge: "AQID",
    rpId: "localhost",
    allowCredentials: [{ type: "public-key", id: "BAUG" }],
    userVerification: "required",
  };
}

function assertionCredential(): PublicKeyCredential {
  return {
    id: "credential",
    rawId: new Uint8Array([1, 2, 3]).buffer,
    type: "public-key",
    response: {
      authenticatorData: new Uint8Array([4]).buffer,
      clientDataJSON: new Uint8Array([5]).buffer,
      signature: new Uint8Array([6]).buffer,
      userHandle: null,
    },
    getClientExtensionResults: () => ({}),
  } as unknown as PublicKeyCredential;
}
