type JsonObject = Record<string, unknown>;

interface CredentialJson {
  id: string;
  rawId: string;
  type: string;
  response: JsonObject;
  authenticatorAttachment?: string | null;
  clientExtensionResults?: JsonObject;
}

const MAX_AUTHENTICATOR_TRANSPORTS = 6;
const MAX_ALLOW_CREDENTIALS = 10;
const MAX_PUB_KEY_CRED_PARAMS = 10;
const MAX_EXCLUDE_CREDENTIALS = 10;
const MAX_CHALLENGE_BYTES = 32;
const MAX_CREDENTIAL_ID_BYTES = 1023;
const MAX_USER_ID_BYTES = 64;

function failOptions(): never {
  throw new Error("Invalid WebAuthn options");
}

function failCredential(): never {
  throw new Error("Invalid WebAuthn credential");
}

function record(value: unknown): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    failOptions();
  }
  return value as JsonObject;
}

function closedKeys(
  value: JsonObject,
  required: readonly string[],
  optional: readonly string[] = [],
): void {
  const allowed = new Set([...required, ...optional]);
  if (
    required.some((key) => !Object.hasOwn(value, key)) ||
    Object.keys(value).some((key) => !allowed.has(key))
  ) {
    failOptions();
  }
}

function boundedArray(value: unknown, maximum: number): unknown[] {
  if (!Array.isArray(value) || value.length > maximum) failOptions();
  return value;
}

function credentialRecord(value: unknown): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    failCredential();
  }
  return value as JsonObject;
}

function string(value: unknown): string {
  if (typeof value !== "string" || value.length === 0) failOptions();
  return value;
}

function finiteNonnegative(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    failOptions();
  }
  return value;
}

function boolean(value: unknown): boolean {
  if (typeof value !== "boolean") failOptions();
  return value;
}

function enumValue<const T extends string>(
  value: unknown,
  allowed: readonly T[],
): T {
  const match = allowed.find((candidate) => candidate === value);
  if (match === undefined) failOptions();
  return match;
}

function base64urlToBuffer(value: string, maximumBytes: number): ArrayBuffer {
  const maximumEncodedLength = Math.ceil((maximumBytes * 4) / 3);
  if (
    value.length > maximumEncodedLength ||
    !/^[A-Za-z0-9_-]*$/.test(value) ||
    value.length % 4 === 1
  ) {
    failOptions();
  }
  const base64 = value.replaceAll("-", "+").replaceAll("_", "/");
  const padded = base64.padEnd(
    base64.length + ((4 - (base64.length % 4)) % 4),
    "=",
  );
  let binary: string;
  try {
    binary = atob(padded);
  } catch {
    failOptions();
  }
  if (binary.length > maximumBytes) failOptions();
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.codePointAt(index) ?? 0;
  }
  return bytes.buffer;
}

function bufferToBase64url(value: ArrayBuffer | ArrayBufferView): string {
  let bytes: Uint8Array;
  if (value instanceof ArrayBuffer) {
    bytes = new Uint8Array(value);
  } else {
    if (!(value.buffer instanceof ArrayBuffer)) failCredential();
    bytes = new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  }
  let binary = "";
  for (const byte of bytes) binary += String.fromCodePoint(byte);
  return btoa(binary)
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replaceAll(/=+$/g, "");
}

function descriptor(value: unknown): PublicKeyCredentialDescriptor {
  const data = record(value);
  closedKeys(data, ["type", "id"], ["transports"]);
  const type = enumValue(data.type, ["public-key"] as const);
  let transports: AuthenticatorTransport[] | undefined;
  if (data.transports !== undefined) {
    transports = boundedArray(
      data.transports,
      MAX_AUTHENTICATOR_TRANSPORTS,
    ).map((transport) => {
      const validated = enumValue(transport, [
        "ble",
        "hybrid",
        "internal",
        "nfc",
        "smart-card",
        "usb",
      ] as const);
      // TypeScript 6.0's lib.dom omits WebAuthn Level 3's smart-card value.
      // The literal is closed-validated above and matches the Go v0.17.4 contract.
      return validated as AuthenticatorTransport;
    });
  }
  return {
    type,
    id: base64urlToBuffer(string(data.id), MAX_CREDENTIAL_ID_BYTES),
    ...(transports === undefined ? {} : { transports }),
  };
}

function unwrapPublicKey(value: unknown): JsonObject {
  const data = record(value);
  closedKeys(data, ["publicKey"]);
  return record(data.publicKey);
}

function userVerification(value: unknown): UserVerificationRequirement {
  return enumValue(value, ["discouraged", "preferred", "required"] as const);
}

function extensions(
  value: unknown,
): AuthenticationExtensionsClientInputs | undefined {
  if (value === undefined) return undefined;
  const data = record(value);
  closedKeys(data, []);
  return {};
}

export function decodeRequestOptions(
  value: unknown,
): PublicKeyCredentialRequestOptions {
  const data = unwrapPublicKey(value);
  closedKeys(
    data,
    ["challenge"],
    ["timeout", "rpId", "allowCredentials", "userVerification", "extensions"],
  );
  const options: PublicKeyCredentialRequestOptions = {
    challenge: base64urlToBuffer(string(data.challenge), MAX_CHALLENGE_BYTES),
  };
  if (data.timeout !== undefined) {
    options.timeout = finiteNonnegative(data.timeout);
  }
  if (data.rpId !== undefined) options.rpId = string(data.rpId);
  if (data.allowCredentials !== undefined) {
    options.allowCredentials = boundedArray(
      data.allowCredentials,
      MAX_ALLOW_CREDENTIALS,
    ).map(descriptor);
  }
  if (data.userVerification !== undefined) {
    options.userVerification = userVerification(data.userVerification);
  }
  const parsedExtensions = extensions(data.extensions);
  if (parsedExtensions !== undefined) options.extensions = parsedExtensions;
  return options;
}

function relyingParty(value: unknown): PublicKeyCredentialRpEntity {
  const data = record(value);
  closedKeys(data, ["id", "name"]);
  return { id: string(data.id), name: string(data.name) };
}

function user(value: unknown): PublicKeyCredentialUserEntity {
  const data = record(value);
  closedKeys(data, ["id", "name", "displayName"]);
  return {
    id: base64urlToBuffer(string(data.id), MAX_USER_ID_BYTES),
    name: string(data.name),
    displayName: string(data.displayName),
  };
}

function parameter(value: unknown): PublicKeyCredentialParameters {
  const data = record(value);
  closedKeys(data, ["type", "alg"]);
  const type = enumValue(data.type, ["public-key"] as const);
  if (typeof data.alg !== "number" || !Number.isInteger(data.alg)) {
    failOptions();
  }
  return { type, alg: data.alg };
}

function authenticatorSelection(
  value: unknown,
): AuthenticatorSelectionCriteria {
  const data = record(value);
  closedKeys(
    data,
    [],
    [
      "authenticatorAttachment",
      "requireResidentKey",
      "residentKey",
      "userVerification",
    ],
  );
  const selection: AuthenticatorSelectionCriteria = {};
  if (data.authenticatorAttachment !== undefined) {
    selection.authenticatorAttachment = enumValue(
      data.authenticatorAttachment,
      ["cross-platform", "platform"] as const,
    );
  }
  if (data.requireResidentKey !== undefined) {
    selection.requireResidentKey = boolean(data.requireResidentKey);
  }
  if (data.residentKey !== undefined) {
    selection.residentKey = enumValue(data.residentKey, [
      "discouraged",
      "preferred",
      "required",
    ] as const);
  }
  if (data.userVerification !== undefined) {
    selection.userVerification = userVerification(data.userVerification);
  }
  return selection;
}

export function decodeCreationOptions(
  value: unknown,
): PublicKeyCredentialCreationOptions {
  const data = unwrapPublicKey(value);
  closedKeys(
    data,
    ["challenge", "rp", "user", "pubKeyCredParams"],
    [
      "timeout",
      "excludeCredentials",
      "authenticatorSelection",
      "attestation",
      "extensions",
    ],
  );
  const options: PublicKeyCredentialCreationOptions = {
    challenge: base64urlToBuffer(string(data.challenge), MAX_CHALLENGE_BYTES),
    rp: relyingParty(data.rp),
    user: user(data.user),
    pubKeyCredParams: boundedArray(
      data.pubKeyCredParams,
      MAX_PUB_KEY_CRED_PARAMS,
    ).map(parameter),
  };
  if (data.timeout !== undefined) {
    options.timeout = finiteNonnegative(data.timeout);
  }
  if (data.excludeCredentials !== undefined) {
    options.excludeCredentials = boundedArray(
      data.excludeCredentials,
      MAX_EXCLUDE_CREDENTIALS,
    ).map(descriptor);
  }
  if (data.authenticatorSelection !== undefined) {
    options.authenticatorSelection = authenticatorSelection(
      data.authenticatorSelection,
    );
  }
  if (data.attestation !== undefined) {
    options.attestation = enumValue(data.attestation, [
      "direct",
      "enterprise",
      "indirect",
      "none",
    ] as const);
  }
  const parsedExtensions = extensions(data.extensions);
  if (parsedExtensions !== undefined) options.extensions = parsedExtensions;
  return options;
}

function standardCredentialJson(credential: unknown): CredentialJson {
  const data = credentialRecord(credential);
  const json =
    typeof data.toJSON === "function"
      ? credentialRecord(data.toJSON.call(credential))
      : null;
  if (json) {
    return {
      id: stringFromCredential(json.id),
      rawId: stringFromCredential(json.rawId),
      type: stringFromCredential(json.type),
      authenticatorAttachment: attachment(json.authenticatorAttachment),
      clientExtensionResults: jsonObject(json.clientExtensionResults),
      response: credentialRecord(json.response),
    };
  }

  const publicKey = credential as PublicKeyCredential;
  return {
    id: publicKey.id,
    rawId: bufferToBase64url(publicKey.rawId),
    type: publicKey.type,
    authenticatorAttachment: attachment(publicKey.authenticatorAttachment),
    clientExtensionResults:
      publicKey.getClientExtensionResults() as unknown as JsonObject,
    response: credentialRecord(publicKey.response),
  };
}

function stringFromCredential(value: unknown): string {
  if (typeof value !== "string") failCredential();
  return value;
}

function jsonObject(value: unknown): JsonObject | undefined {
  if (value === undefined) return undefined;
  return credentialRecord(value);
}

function attachment(value: unknown): string | null | undefined {
  if (value === undefined || value === null) return value;
  return stringFromCredential(value);
}

function bufferField(value: unknown): string {
  if (value instanceof ArrayBuffer || ArrayBuffer.isView(value)) {
    return bufferToBase64url(value);
  }
  if (typeof value === "string") return value;
  failCredential();
}

export function encodeAssertion(
  credential: PublicKeyCredential | null,
): JsonObject {
  const data = standardCredentialJson(credential);
  const response = data.response;
  return {
    id: data.id,
    rawId: data.rawId,
    type: data.type,
    ...(data.authenticatorAttachment === undefined
      ? {}
      : { authenticatorAttachment: data.authenticatorAttachment }),
    ...(data.clientExtensionResults === undefined
      ? {}
      : { clientExtensionResults: data.clientExtensionResults }),
    response: {
      authenticatorData: bufferField(response.authenticatorData),
      clientDataJSON: bufferField(response.clientDataJSON),
      signature: bufferField(response.signature),
      userHandle:
        response.userHandle === null || response.userHandle === undefined
          ? null
          : bufferField(response.userHandle),
    },
  };
}

export function encodeAttestation(
  credential: PublicKeyCredential | null,
): JsonObject {
  const data = standardCredentialJson(credential);
  const response = data.response;
  return {
    id: data.id,
    rawId: data.rawId,
    type: data.type,
    ...(data.authenticatorAttachment === undefined
      ? {}
      : { authenticatorAttachment: data.authenticatorAttachment }),
    ...(data.clientExtensionResults === undefined
      ? {}
      : { clientExtensionResults: data.clientExtensionResults }),
    response: {
      attestationObject: bufferField(response.attestationObject),
      clientDataJSON: bufferField(response.clientDataJSON),
    },
  };
}
