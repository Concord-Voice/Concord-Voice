import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { EnrollScreen } from "./EnrollScreen";
import { server } from "./test/server";

const base = "http://localhost";

describe("EnrollScreen", () => {
  it("scrubs token-bearing URL before rendering and completes enrollment explicitly", async () => {
    history.replaceState(
      null,
      "",
      "/admin/enroll?username=operator&token=enroll-secret",
    );
    vi.spyOn(navigator.credentials, "create").mockResolvedValue(
      attestationCredential(),
    );
    server.use(
      http.post(`${base}/admin/api/v1/enroll/begin`, async ({ request }) => {
        expect(await request.json()).toMatchObject({
          username: "operator",
          password: "correct horse", // pragma: allowlist secret
          token: "enroll-secret",
        });
        return HttpResponse.json({
          handle: "enroll-handle",
          publicKey: { publicKey: creationOptions() },
        });
      }),
      http.post(`${base}/admin/api/v1/enroll/finish`, async ({ request }) => {
        expect(await request.json()).toMatchObject({
          handle: "enroll-handle",
          credential_name: "YubiKey primary",
          attestation: { id: "credential" },
        });
        return HttpResponse.json({ status: "enrolled" });
      }),
    );
    const onContinue = vi.fn();

    render(<EnrollScreen onContinue={onContinue} />);

    expect(location.pathname).toBe("/admin/enroll");
    expect(location.search).toBe("");
    expect(document.body).not.toHaveTextContent("enroll-secret");
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.type(
      screen.getByLabelText("Credential name"),
      "YubiKey primary",
    );
    await userEvent.click(
      screen.getByRole("button", { name: "Enroll security key" }),
    );

    expect(await screen.findByText("Security key enrolled")).toBeVisible();
    expect(screen.getByText(/continue to the Admin Portal/i)).toBeVisible();
    await userEvent.click(
      screen.getByRole("button", { name: "Continue to Admin Portal" }),
    );
    expect(onContinue).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["begin", "/admin/api/v1/enroll/begin"],
    ["finish", "/admin/api/v1/enroll/finish"],
  ])(
    "shows generic %s failures without leaking token or password",
    async (_name, failedPath) => {
      history.replaceState(
        null,
        "",
        "/admin/enroll?username=operator&token=enroll-secret",
      );
      vi.spyOn(navigator.credentials, "create").mockResolvedValue(
        attestationCredential(),
      );
      server.use(
        http.post(`${base}/admin/api/v1/enroll/begin`, () =>
          failedPath.endsWith("/begin")
            ? HttpResponse.json(
                { error: "password token attestation leaked body" },
                { status: 401 },
              )
            : HttpResponse.json({
                handle: "enroll-handle",
                publicKey: { publicKey: creationOptions() },
              }),
        ),
        http.post(`${base}/admin/api/v1/enroll/finish`, () =>
          HttpResponse.json(
            { error: "password token attestation leaked body" },
            { status: 401 },
          ),
        ),
      );

      render(<EnrollScreen onContinue={vi.fn()} />);
      await userEvent.type(screen.getByLabelText("Password"), "correct horse");
      await userEvent.type(
        screen.getByLabelText("Credential name"),
        "YubiKey primary",
      );
      await userEvent.click(
        screen.getByRole("button", { name: "Enroll security key" }),
      );

      const alert = await screen.findByRole("alert");
      expect(alert).toHaveTextContent(
        "Enrollment failed. Check the invitation and try again.",
      );
      // findByRole gates on the alert EXISTING; focus is applied by a separate
      // post-render effect, so it needs its own wait. The negative assertion
      // below stays bare — waitFor resolves at the first poll where its
      // condition holds, so a late leak would slip past.
      // See [internal]rules/tests.md § Async assertions.
      await waitFor(() => expect(screen.getByRole("alert")).toHaveFocus());
      expect(alert).not.toHaveTextContent(
        /correct horse|enroll-secret|token|attestation/i,
      );
      expect(`${localStorage.length}${sessionStorage.length}`).toBe("00");
    },
  );

  it("handles credential creation failure and null credentials", async () => {
    history.replaceState(
      null,
      "",
      "/admin/enroll?username=operator&token=enroll-secret",
    );
    const create = vi
      .spyOn(navigator.credentials, "create")
      .mockRejectedValueOnce(
        new DOMException("The operation was aborted.", "NotAllowedError"),
      );
    server.use(
      http.post(`${base}/admin/api/v1/enroll/begin`, () =>
        HttpResponse.json({
          handle: "enroll-handle",
          publicKey: { publicKey: creationOptions() },
        }),
      ),
    );

    render(<EnrollScreen onContinue={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.type(
      screen.getByLabelText("Credential name"),
      "YubiKey primary",
    );
    await userEvent.click(
      screen.getByRole("button", { name: "Enroll security key" }),
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Security key enrollment was canceled.",
    );

    create.mockResolvedValueOnce(null);
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Security key did not return a credential.",
      ),
    );
  });
});

function creationOptions() {
  return {
    challenge: "AQID",
    rp: { id: "localhost", name: "Concord Voice" },
    user: { id: "BAUG", name: "operator", displayName: "operator" },
    pubKeyCredParams: [{ type: "public-key", alg: -7 }],
    attestation: "direct",
  };
}

function attestationCredential(): PublicKeyCredential {
  return {
    id: "credential",
    rawId: new Uint8Array([1, 2, 3]).buffer,
    type: "public-key",
    response: {
      attestationObject: new Uint8Array([4]).buffer,
      clientDataJSON: new Uint8Array([5]).buffer,
    },
    getClientExtensionResults: () => ({}),
  } as unknown as PublicKeyCredential;
}
