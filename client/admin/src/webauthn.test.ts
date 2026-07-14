import { describe, expect, it, vi } from "vitest";

import {
  decodeCreationOptions,
  decodeRequestOptions,
  encodeAssertion,
  encodeAttestation,
} from "./webauthn";

function bytes(value: BufferSource): number[] {
  return Array.from(
    new Uint8Array(value instanceof ArrayBuffer ? value : value.buffer),
  );
}

function buffer(...values: number[]): ArrayBuffer {
  return new Uint8Array(values).buffer;
}

function encodedBytes(length: number): string {
  return btoa("\0".repeat(length)).replaceAll("=", "");
}

function creationOptions(overrides: Record<string, unknown>): unknown {
  return {
    publicKey: {
      challenge: "AQID",
      rp: { id: "localhost", name: "Concord Voice" },
      user: { id: "BAUG", name: "operator", displayName: "Operator" },
      pubKeyCredParams: [{ type: "public-key", alg: -7 }],
      ...overrides,
    },
  };
}

describe("bounded Base64URL options", () => {
  const oversizedChallenge = encodedBytes(33);
  const oversizedCredentialID = encodedBytes(1024);
  const oversizedUserID = encodedBytes(65);

  it.each([
    [
      "request challenges above 32 decoded bytes",
      oversizedChallenge,
      () =>
        decodeRequestOptions({
          publicKey: { challenge: oversizedChallenge },
        }),
    ],
    [
      "creation challenges above 32 decoded bytes",
      oversizedChallenge,
      () =>
        decodeCreationOptions(
          creationOptions({ challenge: oversizedChallenge }),
        ),
    ],
    [
      "allowed credential IDs above 1023 decoded bytes",
      oversizedCredentialID,
      () =>
        decodeRequestOptions({
          publicKey: {
            challenge: "AQID",
            allowCredentials: [
              { type: "public-key", id: oversizedCredentialID },
            ],
          },
        }),
    ],
    [
      "excluded credential IDs above 1023 decoded bytes",
      oversizedCredentialID,
      () =>
        decodeCreationOptions(
          creationOptions({
            excludeCredentials: [
              { type: "public-key", id: oversizedCredentialID },
            ],
          }),
        ),
    ],
    [
      "user IDs above 64 decoded bytes",
      oversizedUserID,
      () =>
        decodeCreationOptions(
          creationOptions({
            user: {
              id: oversizedUserID,
              name: "operator",
              displayName: "Operator",
            },
          }),
        ),
    ],
  ])("rejects oversized %s before decoding", (_label, oversized, decode) => {
    const decoder = vi.spyOn(globalThis, "atob");

    expect(decode).toThrow("Invalid WebAuthn options");
    expect(decoder).not.toHaveBeenCalledWith(oversized);
  });
});

describe("decodeRequestOptions", () => {
  it("decodes Base64URL challenge and credential ids with URL alphabet and omitted padding", () => {
    const options = decodeRequestOptions({
      publicKey: {
        challenge: "AQID-_8",
        rpId: "localhost",
        allowCredentials: [
          { type: "public-key", id: "BAUG", transports: ["usb"] },
        ],
        userVerification: "required",
      },
    });

    expect(bytes(options.challenge)).toEqual([1, 2, 3, 251, 255]);
    expect(options.allowCredentials).toHaveLength(1);
    expect(bytes(options.allowCredentials?.[0]?.id ?? buffer())).toEqual([
      4, 5, 6,
    ]);
    expect(options.allowCredentials?.[0]?.transports).toEqual(["usb"]);
    expect(options.userVerification).toBe("required");
  });

  it("accepts the backend-supported WebAuthn Level 3 smart-card transport", () => {
    const options = decodeRequestOptions({
      publicKey: {
        challenge: "AQID",
        allowCredentials: [
          { type: "public-key", id: "BAUG", transports: ["smart-card"] },
        ],
      },
    });

    expect(options.allowCredentials?.[0]?.transports).toEqual(["smart-card"]);
  });

  it("rejects oversized credential transports", () => {
    expect(() =>
      decodeRequestOptions({
        publicKey: {
          challenge: "AQID",
          allowCredentials: [
            {
              type: "public-key",
              id: "BAUG",
              transports: Array.from({ length: 7 }, () => "usb"),
            },
          ],
        },
      }),
    ).toThrow("Invalid WebAuthn options");
  });

  it("rejects oversized allowed credential lists", () => {
    expect(() =>
      decodeRequestOptions({
        publicKey: {
          challenge: "AQID",
          allowCredentials: Array.from({ length: 11 }, () => ({
            type: "public-key",
            id: "BAUG",
          })),
        },
      }),
    ).toThrow("Invalid WebAuthn options");
  });

  it("rejects malformed request options", () => {
    expect(() =>
      decodeRequestOptions({ publicKey: { challenge: "*" } }),
    ).toThrow("Invalid WebAuthn options");
    expect(() => decodeRequestOptions(null)).toThrow(
      "Invalid WebAuthn options",
    );
  });

  it.each([
    ["wrapper fields", { publicKey: { challenge: "AQID" }, unexpected: true }],
    ["option fields", { publicKey: { challenge: "AQID", unexpected: true } }],
    ["timeout", { publicKey: { challenge: "AQID", timeout: "60000" } }],
    [
      "user verification",
      { publicKey: { challenge: "AQID", userVerification: "always" } },
    ],
    [
      "credential type",
      {
        publicKey: {
          challenge: "AQID",
          allowCredentials: [{ type: "password", id: "BAUG" }],
        },
      },
    ],
    [
      "transport",
      {
        publicKey: {
          challenge: "AQID",
          allowCredentials: [
            { type: "public-key", id: "BAUG", transports: ["telepathy"] },
          ],
        },
      },
    ],
    [
      "extensions",
      {
        publicKey: {
          challenge: "AQID",
          extensions: { unreviewedExtension: true },
        },
      },
    ],
  ])("rejects unvalidated request %s", (_label, value) => {
    expect(() => decodeRequestOptions(value)).toThrow(
      "Invalid WebAuthn options",
    );
  });
});

describe("decodeCreationOptions", () => {
  it("decodes challenge, user id, and excluded credential ids", () => {
    const options = decodeCreationOptions({
      publicKey: {
        challenge: "AQID",
        rp: { id: "localhost", name: "Concord Voice" },
        user: { id: "BAUG", name: "operator", displayName: "Operator" },
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        excludeCredentials: [{ type: "public-key", id: "BwgJ" }],
        authenticatorSelection: { userVerification: "required" },
        attestation: "direct",
      },
    });

    expect(bytes(options.challenge)).toEqual([1, 2, 3]);
    expect(bytes(options.user.id)).toEqual([4, 5, 6]);
    expect(bytes(options.excludeCredentials?.[0]?.id ?? buffer())).toEqual([
      7, 8, 9,
    ]);
    expect(options.attestation).toBe("direct");
  });

  it("rejects oversized public-key credential parameter lists", () => {
    expect(() =>
      decodeCreationOptions({
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: { id: "BAUG", name: "operator", displayName: "Operator" },
          pubKeyCredParams: Array.from({ length: 11 }, () => ({
            type: "public-key",
            alg: -7,
          })),
        },
      }),
    ).toThrow("Invalid WebAuthn options");
  });

  it("rejects oversized excluded credential lists", () => {
    expect(() =>
      decodeCreationOptions({
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: { id: "BAUG", name: "operator", displayName: "Operator" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
          excludeCredentials: Array.from({ length: 11 }, () => ({
            type: "public-key",
            id: "BwgJ",
          })),
        },
      }),
    ).toThrow("Invalid WebAuthn options");
  });

  it("rejects malformed creation options", () => {
    expect(() =>
      decodeCreationOptions({
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: { id: "*", name: "operator", displayName: "Operator" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        },
      }),
    ).toThrow("Invalid WebAuthn options");
  });

  it.each([
    [
      "relying party fields",
      {
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice", icon: "unsafe" },
          user: { id: "BAUG", name: "operator", displayName: "Operator" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        },
      },
    ],
    [
      "user fields",
      {
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: {
            id: "BAUG",
            name: "operator",
            displayName: "Operator",
            role: "admin",
          },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        },
      },
    ],
    [
      "credential parameters",
      {
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: { id: "BAUG", name: "operator", displayName: "Operator" },
          pubKeyCredParams: [{ type: "public-key", alg: "-7" }],
        },
      },
    ],
    [
      "authenticator selection",
      {
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: { id: "BAUG", name: "operator", displayName: "Operator" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
          authenticatorSelection: { authenticatorAttachment: "remote" },
        },
      },
    ],
    [
      "attestation",
      {
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: { id: "BAUG", name: "operator", displayName: "Operator" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
          attestation: "opaque",
        },
      },
    ],
    [
      "extensions",
      {
        publicKey: {
          challenge: "AQID",
          rp: { id: "localhost", name: "Concord Voice" },
          user: { id: "BAUG", name: "operator", displayName: "Operator" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
          extensions: { unreviewedExtension: true },
        },
      },
    ],
  ])("rejects unvalidated creation %s", (_label, value) => {
    expect(() => decodeCreationOptions(value)).toThrow(
      "Invalid WebAuthn options",
    );
  });
});

describe("credential encoding", () => {
  it("encodes assertions with only standard JSON fields", () => {
    const assertion = encodeAssertion({
      id: "credential",
      rawId: buffer(1, 2, 3),
      type: "public-key",
      authenticatorAttachment: "cross-platform",
      response: {
        authenticatorData: buffer(4),
        clientDataJSON: buffer(5),
        signature: buffer(6),
        userHandle: buffer(7),
        extra: "do not include",
      },
      getClientExtensionResults: () => ({ appid: true }),
      extra: "do not include",
    } as unknown as PublicKeyCredential);

    expect(assertion).toEqual({
      id: "credential",
      rawId: "AQID",
      type: "public-key",
      authenticatorAttachment: "cross-platform",
      clientExtensionResults: { appid: true },
      response: {
        authenticatorData: "BA",
        clientDataJSON: "BQ",
        signature: "Bg",
        userHandle: "Bw",
      },
    });
  });

  it("uses toJSON when available but still filters nonstandard fields", () => {
    const assertion = encodeAssertion({
      toJSON: () => ({
        id: "credential",
        rawId: "AQID",
        type: "public-key",
        authenticatorAttachment: "platform",
        clientExtensionResults: {},
        response: {
          authenticatorData: "BA",
          clientDataJSON: "BQ",
          signature: "Bg",
          userHandle: null,
          nonstandard: "drop",
        },
        nonstandard: "drop",
      }),
    } as unknown as PublicKeyCredential);

    expect(assertion).toEqual({
      id: "credential",
      rawId: "AQID",
      type: "public-key",
      authenticatorAttachment: "platform",
      clientExtensionResults: {},
      response: {
        authenticatorData: "BA",
        clientDataJSON: "BQ",
        signature: "Bg",
        userHandle: null,
      },
    });
  });

  it("normalizes an omitted non-resident user handle to null", () => {
    const assertion = encodeAssertion({
      toJSON: () => ({
        id: "credential",
        rawId: "AQID",
        type: "public-key",
        clientExtensionResults: {},
        response: {
          authenticatorData: "BA",
          clientDataJSON: "BQ",
          signature: "Bg",
        },
      }),
    } as unknown as PublicKeyCredential);

    expect(assertion).toMatchObject({
      response: { userHandle: null },
    });
  });

  it("encodes attestations with only standard JSON fields", () => {
    const attestation = encodeAttestation({
      id: "credential",
      rawId: buffer(1, 2, 3),
      type: "public-key",
      response: {
        attestationObject: buffer(4),
        clientDataJSON: buffer(5),
        extra: "do not include",
      },
      getClientExtensionResults: () => ({}),
      extra: "do not include",
    } as unknown as PublicKeyCredential);

    expect(attestation).toEqual({
      id: "credential",
      rawId: "AQID",
      type: "public-key",
      clientExtensionResults: {},
      response: {
        attestationObject: "BA",
        clientDataJSON: "BQ",
      },
    });
  });

  it("rejects null credentials", () => {
    expect(() => encodeAssertion(null)).toThrow("Invalid WebAuthn credential");
    expect(() => encodeAttestation(null)).toThrow(
      "Invalid WebAuthn credential",
    );
  });
});
