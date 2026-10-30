import { createHash, createHmac, timingSafeEqual } from 'node:crypto';
// This boundary authenticates wire values, never SQL or native authority.
export type Endpoint = 'connection_register' | 'connection_release' | 'provisional_close' | 'room_register' | 'room_release' | 'health_bootstrap' | 'boot_retire';
export interface Header {
  name: string;
  value: string;
}
export interface HTTPRequest {
  method: string;
  path: string;
  headers: Header[];
  body: Uint8Array;
}
export interface HTTPResponse {
  status: number;
  headers: Header[];
  body: Uint8Array;
}
export interface AttemptMeta {
  timestamp: string;
  nonce: string;
}
export interface ExpectedResponse {
  userId?: string;
  binding?: Binding;
}
export type ProtocolReason = 'payload_too_large' | 'malformed_body' | 'invalid_variant' | 'invalid_value' | 'noncanonical_endpoint' | 'invalid_headers' | 'invalid_proof' | 'proof_stale' | 'unexpected_status' | 'binding_mismatch' | 'invalid_decoder_state';
export class ProtocolError extends Error {
  constructor(readonly reason: ProtocolReason) { super(`voice auth protocol: ${reason}`); this.name = 'ProtocolError'; }
}
function fail(reason: ProtocolReason): never { throw new ProtocolError(reason); }
export interface Target {
  connectionGeneration: string;
  nodeBootId: string;
  socketId: string;
  credentialEpoch: string;
}
export interface Binding {
  userId: string;
  familyId: string;
  target: Target;
}
export interface Room {
  sessionGeneration: string;
  roomId: string;
  roomKind: 'channel' | 'dm';
}
export interface AuthCommand {
  userId: string;
  familyId: string;
  target: Target;
  schema: 'concord.auth-revocation';
  version: 1;
  operationId: string;
  scope: 'session' | 'user';
  reason: 'logout' | 'explicit_revoke' | 'displacement' | 'theft' | 'disablement' | 'erasure';
  timestamp: string;
  nonce: string;
  proof: string;
}
export interface EpochCommand {
  userId: string;
  familyId: string;
  target: Target;
  schema: 'concord.credential-connection-revocation';
  version: 1;
  parentGeneration: string;
  supersededCredentialEpoch: string;
  timestamp: string;
  nonce: string;
  proof: string;
}
export interface Health4 {
  schema: 'concord.voice-enforcement-health';
  version: 1;
  protocol: 4;
  nodeBootId: string;
  challenge: string;
  activation: 'prepared' | 'active';
  admissionAllowed: boolean;
  timestamp: string;
  proof: string;
}
export interface ConnectionRegister {
  version: 1;
  connectionGeneration: string;
  nodeBootId: string;
  socketId: string;
  credentialEpoch: string;
}
export interface RegistrationResult {
  userId: string;
  familyId: string;
  target: Target;
  version: 1;
  voiceEnforcementProtocol: 4;
}
export interface ConnectionRelease {
  userId: string;
  familyId: string;
  target: Target;
  version: 1;
}
export interface ProvisionalClose {
  schema: 'concord.auth-connection-provisional-close';
  version: 1;
  target: Target;
  registrationTokenDigest: string;
  registrationBodyDigest: string;
}
export type ProvisionalCloseResult = {
  schema: 'concord.auth-connection-provisional-close-result';
  version: 1;
  target: Target;
  registrationTokenDigest: string;
  registrationBodyDigest: string;
} & ({
  disposition: 'registered';
  binding: Binding;
} | {
  disposition: 'unregistered' | 'retired_boot';
  binding?: never;
});
export interface RoomRegister {
  schema: 'concord.auth-room-binding';
  version: 1;
  mode: 'attach' | 'renew';
  connectionGeneration: string;
  nodeBootId: string;
  socketId: string;
  credentialEpoch: string;
  sessionGeneration: string;
  roomId: string;
  roomKind: 'channel' | 'dm';
}
export interface RoomRegistrationResult {
  schema: 'concord.auth-room-binding';
  version: 1;
  voiceEnforcementProtocol: 4;
  binding: Binding;
  room: Room;
}
export interface RoomRelease {
  schema: 'concord.auth-room-binding';
  version: 1;
  binding: Binding;
  room: Room;
}
export interface HealthBootstrap {
  version: 1;
  nodeBootId: string;
  nonce: string;
}
export interface HealthBootstrapResult {
  version: 1;
  voiceEnforcementProtocol: 4;
  nodeBootId: string;
  activation: 'prepared' | 'active';
  transportOK: true;
  admissionGranted: boolean;
}
export interface BootRetire {
  schema: 'concord.auth-boot-retirement';
  version: 1;
  nodeBootId: string;
}
export interface AuthAck {
  userId: string;
  familyId: string;
  target: Target;
  schema: 'concord.auth-revocation';
  version: 1;
  operationId: string;
  scope: 'session' | 'user';
  reason: 'logout' | 'explicit_revoke' | 'displacement' | 'theft' | 'disablement' | 'erasure';
  timestamp: string;
  nonce: string;
  ok: true;
  proof: string;
}
export interface EpochAck {
  userId: string;
  familyId: string;
  target: Target;
  schema: 'concord.credential-connection-revocation';
  version: 1;
  parentGeneration: string;
  supersededCredentialEpoch: string;
  timestamp: string;
  nonce: string;
  ok: true;
  proof: string;
}
export interface HealthAck4 {
  schema: 'concord.voice-enforcement-health';
  version: 1;
  protocol: 4;
  nodeBootId: string;
  challenge: string;
  activation: 'prepared' | 'active';
  admissionAllowed: boolean;
  timestamp: string;
  transportOK: true;
  admissionGranted: boolean;
  proof: string;
}
export type ResponsePayload = RegistrationResult | ProvisionalCloseResult | RoomRegistrationResult | HealthBootstrapResult | null;
export interface Command {
  readonly [wrapperBrand]: "Command";
  kind(): string;
  auth(): AuthCommand | undefined;
  epoch(): EpochCommand | undefined;
  health(): Health4 | undefined;
}
export interface ACK {
  readonly [wrapperBrand]: "ACK";
  kind(): string;
  auth(): AuthAck | undefined;
  epoch(): EpochAck | undefined;
  health(): HealthAck4 | undefined;
}
export interface Request {
  readonly [wrapperBrand]: "Request";
  endpoint(): Endpoint;
  connectionRegister(): ConnectionRegister | undefined;
  connectionRelease(): ConnectionRelease | undefined;
  provisionalClose(): ProvisionalClose | undefined;
  roomRegister(): RoomRegister | undefined;
  roomRelease(): RoomRelease | undefined;
  healthBootstrap(): HealthBootstrap | undefined;
  bootRetire(): BootRetire | undefined;
}
export interface Response {
  readonly [wrapperBrand]: "Response";
  endpoint(): Endpoint;
  empty(): boolean;
  registration(): RegistrationResult | undefined;
  provisionalClose(): ProvisionalCloseResult | undefined;
  roomRegistration(): RoomRegistrationResult | undefined;
  healthBootstrap(): HealthBootstrapResult | undefined;
}
const wrapperBrand = Symbol();
export interface RequestContext {
  readonly [wrapperBrand]: "RequestContext";
}
const AUTH = 'concord.auth-revocation';
const EPOCH = 'concord.credential-connection-revocation';
const HEALTH = 'concord.voice-enforcement-health';
const PROVISIONAL = 'concord.auth-connection-provisional-close';
const PROVISIONAL_RESULT = 'concord.auth-connection-provisional-close-result';
const ROOM = 'concord.auth-room-binding';
const RETIRE = 'concord.auth-boot-retirement';
const ZERO_PROOF = '0'.repeat(64);
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}(?![\s\S])/;
const HEX64 = /^[0-9a-f]{64}(?![\s\S])/;
const EPOCH_HEX = /^(?:[0-9a-f]{32})?(?![\s\S])/;
function timestampOK(s: string): boolean { return /^[1-9]\d*(?![\s\S])/.test(s) && BigInt(s) <= 9223372036854775807n; }
/** JSON permits unpaired surrogate escapes; the wire contract does not. */
export function unicodeOK(s: string): boolean {
  // String iteration yields a lone surrogate as its own one-unit code point.
  for (const ch of s) {
    const c = ch.codePointAt(0) ?? 0;
    if (c >= 0xd800 && c <= 0xdfff) {
      return false;
    }
  }
  return true;
}
function bounded(raw: Uint8Array): void { if (raw.byteLength > 4096) {
  fail('payload_too_large');
} }
// Inspect duplicate keys before JSON.parse can erase them. The complete body is
// bounded before either parse, and fatal decoding preserves invalid-byte evidence.
function parseRaw(raw: Uint8Array): unknown {
  bounded(raw);
  let text: string;
  try {
    text = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(raw);
    JSON.parse(text);
  }
  catch {
    return fail('malformed_body');
  }
  return new RawScanner(text).value();
}
// Walks text already proven to be valid JSON, so only key identity and scalar
// spelling are checked here; structural errors cannot reach it.
class RawScanner {
  private pos = 0;
  constructor(private readonly text: string) {}
  private whitespace(): void {
    while (/[\x20\t\r\n]/.test(this.text[this.pos] ?? '\0')) this.pos++;
  }
  private string(): string {
    const start = this.pos++;
    while (this.pos < this.text.length) {
      if (this.text[this.pos] === '\\') {
        this.pos += 2;
        continue;
      }
      if (this.text[this.pos++] === '"') {
        break;
      }
    }
    const s: unknown = JSON.parse(this.text.slice(start, this.pos));
    if (typeof s !== 'string' || !unicodeOK(s)) {
      fail('malformed_body');
    }
    return s;
  }
  /** Consumes an opening bracket; true when its container is empty and closed. */
  private open(close: string): boolean {
    this.pos++;
    this.whitespace();
    if (this.text[this.pos] !== close) return false;
    this.pos++;
    return true;
  }
  private object(): Record<string, unknown> {
    const result: Record<string, unknown> = Object.create(null) as Record<string, unknown>;
    if (this.open('}')) return result;
    do {
      this.whitespace();
      const key = this.string();
      if (Object.hasOwn(result, key)) {
        fail('malformed_body');
      }
      this.whitespace();
      this.pos++;
      result[key] = this.value();
      this.whitespace();
    } while (this.text[this.pos++] !== '}');
    return result;
  }
  private array(): unknown[] {
    const result: unknown[] = [];
    if (this.open(']')) return result;
    do {
      result.push(this.value());
      this.whitespace();
    } while (this.text[this.pos++] !== ']');
    return result;
  }
  private scalar(): unknown {
    const start = this.pos;
    while (this.pos < this.text.length && !/[\x20\t\r\n,}\]]/.test(this.text[this.pos])) this.pos++;
    const token = this.text.slice(start, this.pos);
    const result: unknown = JSON.parse(token);
    // Versions/protocols are exact integers, never a rounded Number coercion.
    return typeof result === 'number' && !/^(?:0|[1-9]\d*)$/.test(token) ? Symbol('noninteger') : result;
  }
  value(): unknown {
    this.whitespace();
    switch (this.text[this.pos]) {
      case '"': return this.string();
      case '{': return this.object();
      case '[': return this.array();
      default: return this.scalar();
    }
  }
}
type Shape = readonly (readonly [
  string,
  string
])[];
const shapes: Record<string, Shape> = {
  Target: [["connectionGeneration", "string"], ["nodeBootId", "string"], ["socketId", "string"], ["credentialEpoch", "string"]],
  Binding: [["userId", "string"], ["familyId", "string"], ["target", "Target"]],
  Room: [["sessionGeneration", "string"], ["roomId", "string"], ["roomKind", "string"]],
  AuthCommand: [["userId", "string"], ["familyId", "string"], ["target", "Target"], ["schema", "string"], ["version", "int"], ["operationId", "string"], ["scope", "string"], ["reason", "string"], ["timestamp", "string"], ["nonce", "string"], ["proof", "string"]],
  EpochCommand: [["userId", "string"], ["familyId", "string"], ["target", "Target"], ["schema", "string"], ["version", "int"], ["parentGeneration", "string"], ["supersededCredentialEpoch", "string"], ["timestamp", "string"], ["nonce", "string"], ["proof", "string"]],
  Health4: [["schema", "string"], ["version", "int"], ["protocol", "int"], ["nodeBootId", "string"], ["challenge", "string"], ["activation", "string"], ["admissionAllowed", "bool"], ["timestamp", "string"], ["proof", "string"]],
  ConnectionRegister: [["version", "int"], ["connectionGeneration", "string"], ["nodeBootId", "string"], ["socketId", "string"], ["credentialEpoch", "string"]],
  RegistrationResult: [["userId", "string"], ["familyId", "string"], ["target", "Target"], ["version", "int"], ["voiceEnforcementProtocol", "int"]],
  ConnectionRelease: [["userId", "string"], ["familyId", "string"], ["target", "Target"], ["version", "int"]],
  ProvisionalClose: [["schema", "string"], ["version", "int"], ["target", "Target"], ["registrationTokenDigest", "string"], ["registrationBodyDigest", "string"]],
  ProvisionalCloseResult: [["schema", "string"], ["version", "int"], ["target", "Target"], ["registrationTokenDigest", "string"], ["registrationBodyDigest", "string"], ["disposition", "string"], ["binding", "*Binding"]],
  RoomRegister: [["schema", "string"], ["version", "int"], ["mode", "string"], ["connectionGeneration", "string"], ["nodeBootId", "string"], ["socketId", "string"], ["credentialEpoch", "string"], ["sessionGeneration", "string"], ["roomId", "string"], ["roomKind", "string"]],
  RoomRegistrationResult: [["schema", "string"], ["version", "int"], ["voiceEnforcementProtocol", "int"], ["binding", "Binding"], ["room", "Room"]],
  RoomRelease: [["schema", "string"], ["version", "int"], ["binding", "Binding"], ["room", "Room"]],
  HealthBootstrap: [["version", "int"], ["nodeBootId", "string"], ["nonce", "string"]],
  HealthBootstrapResult: [["version", "int"], ["voiceEnforcementProtocol", "int"], ["nodeBootId", "string"], ["activation", "string"], ["transportOK", "bool"], ["admissionGranted", "bool"]],
  BootRetire: [["schema", "string"], ["version", "int"], ["nodeBootId", "string"]],
  AuthAck: [["userId", "string"], ["familyId", "string"], ["target", "Target"], ["schema", "string"], ["version", "int"], ["operationId", "string"], ["scope", "string"], ["reason", "string"], ["timestamp", "string"], ["nonce", "string"], ["ok", "bool"], ["proof", "string"]],
  EpochAck: [["userId", "string"], ["familyId", "string"], ["target", "Target"], ["schema", "string"], ["version", "int"], ["parentGeneration", "string"], ["supersededCredentialEpoch", "string"], ["timestamp", "string"], ["nonce", "string"], ["ok", "bool"], ["proof", "string"]],
  HealthAck4: [["schema", "string"], ["version", "int"], ["protocol", "int"], ["nodeBootId", "string"], ["challenge", "string"], ["activation", "string"], ["admissionAllowed", "bool"], ["timestamp", "string"], ["transportOK", "bool"], ["admissionGranted", "bool"], ["proof", "string"]],
};
function object(value: unknown): value is Record<string, unknown> { return typeof value === 'object' && value !== null && !Array.isArray(value); }
function checkShape(value: unknown, name: string, schema: string): void {
  if (!object(value)) {
    fail('invalid_variant');
  }
  const shape = shapes[name];
  if (!shape) {
    fail('invalid_decoder_state');
  }
  const fields = variantFields(value, name, shape);
  if (Object.keys(value).length !== fields.length) {
    fail('invalid_variant');
  }
  for (const [key, type] of fields) {
    checkShapeField(value, key, type, schema);
  }
}
// A provisional close without a registration carries no binding.
function variantFields(value: Record<string, unknown>, name: string, shape: Shape): Shape {
  if (name !== 'ProvisionalCloseResult' || value.disposition === 'registered') {
    return shape;
  }
  if (value.disposition === 'unregistered' || value.disposition === 'retired_boot') {
    return shape.filter(([key]) => key !== 'binding');
  }
  return fail('invalid_variant');
}
function checkShapeField(value: Record<string, unknown>, key: string, type: string, schema: string): void {
  if (!Object.hasOwn(value, key) || value[key] === null) {
    fail('invalid_variant');
  }
  if (key === 'schema' && value[key] !== schema) {
    fail('invalid_variant');
  }
  if (type === 'int' && value[key] !== (key === 'version' ? 1 : 4)) {
    fail('invalid_variant');
  }
  if (type !== 'int' && type !== 'bool' && type !== 'string') {
    checkShape(value[key], type.replace(/^\*/, ''), '');
  }
}
function scalarOK(name: string, s: string): boolean {
  if (['userId', 'familyId', 'connectionGeneration', 'nodeBootId', 'sessionGeneration', 'roomId', 'operationId', 'parentGeneration'].includes(name)) {
    return UUID.test(s);
  }
  if (['credentialEpoch', 'supersededCredentialEpoch'].includes(name)) {
    return EPOCH_HEX.test(s);
  }
  if (['nonce', 'proof', 'challenge', 'registrationTokenDigest', 'registrationBodyDigest'].includes(name)) {
    return HEX64.test(s);
  }
  switch (name) {
    case 'socketId': return Buffer.byteLength(s, 'utf8') >= 1 && Buffer.byteLength(s, 'utf8') <= 128 && !/\p{Cc}/u.test(s) && unicodeOK(s);
    case 'timestamp': return timestampOK(s);
    case 'scope': return s === 'session' || s === 'user';
    case 'reason': return ['logout', 'explicit_revoke', 'displacement', 'theft', 'disablement', 'erasure'].includes(s);
    case 'activation': return s === 'prepared' || s === 'active';
    case 'mode': return s === 'attach' || s === 'renew';
    case 'roomKind': return s === 'channel' || s === 'dm';
    default: return true;
  }
}
function checkScalars(value: Record<string, unknown>, name: string): void {
  for (const [key, type] of shapes[name]) {
    checkScalarField(value[key], key, type);
  }
  if (value.activation === 'prepared' && (value.admissionAllowed === true || value.admissionGranted === true)) {
    fail('invalid_value');
  }
  if (value.admissionAllowed === false && value.admissionGranted === true) {
    fail('invalid_value');
  }
}
function checkScalarField(v: unknown, key: string, type: string): void {
  if (v === undefined && type.startsWith('*')) {
    return;
  }
  if (type === 'string' && (typeof v !== 'string' || !scalarOK(key, v))) {
    fail('invalid_value');
  }
  if (type === 'bool' && (typeof v !== 'boolean' || ((key === 'ok' || key === 'transportOK') && !v))) {
    fail('invalid_value');
  }
  if (type !== 'int' && type !== 'bool' && type !== 'string') {
    if (!object(v)) {
      fail('invalid_variant');
    }
    checkScalars(v, type.replace(/^\*/, ''));
  }
}
function decode<T>(raw: Uint8Array, name: string, schema = ''): T {
  const value = parseRaw(raw);
  checkShape(value, name, schema);
  if (!object(value)) {
    fail('invalid_variant');
  }
  checkScalars(value, name);
  return value as T;
}
function encode(value: unknown): Uint8Array { return Buffer.from(JSON.stringify(value), 'utf8'); }
// Builders always use the published field order, independently of the order in
// which a peer supplied equivalent JSON. HTTP request bytes are never rebuilt.
function orderedValue(value: unknown, name: string): unknown {
  if (!object(value)) {
    return fail('invalid_decoder_state');
  }
  const result: Record<string, unknown> = {};
  for (const [key, type] of shapes[name]) {
    if (value[key] === undefined && type.startsWith('*')) {
      continue;
    }
    result[key] = type === 'int' || type === 'string' || type === 'bool'
      ? value[key] : orderedValue(value[key], type.replace(/^\*/, ''));
  }
  return result;
}
function encodeTyped(value: unknown, name: string): Uint8Array { return encode(orderedValue(value, name)); }
function commandType(v: CommandValue): string {
  if ('operationId' in v) return 'AuthCommand';
  return 'parentGeneration' in v ? 'EpochCommand' : 'Health4';
}
function copy<T>(value: T): T { return structuredClone(value); }
type CommandValue = AuthCommand | EpochCommand | Health4;
type AckValue = AuthAck | EpochAck | HealthAck4;
type RequestValue = ConnectionRegister | ConnectionRelease | ProvisionalClose | RoomRegister | RoomRelease | HealthBootstrap | BootRetire;
type SuccessValue = Exclude<ResponsePayload, null>;
interface CommandState {
  value: CommandValue;
  raw: Uint8Array;
}
interface RequestState {
  endpoint: Endpoint;
  value: RequestValue;
  wire: HTTPRequest;
  timestamp: string;
  nonce: string;
  proof: string;
  tokenDigest: string;
  bodyDigest: string;
}
interface ContextState {
  request: RequestState;
  expected: ExpectedResponse;
}
const commands = new WeakMap<Command, CommandState>();
const requests = new WeakMap<Request, RequestState>();
const contexts = new WeakMap<RequestContext, ContextState>();
function commandState(c: Command): CommandState {
  return commands.get(c) ?? fail('invalid_decoder_state');
}
function requestState(r: Request): RequestState {
  return requests.get(r) ?? fail('invalid_decoder_state');
}
function wrapCommand(value: CommandValue, raw: Uint8Array): Command {
  const c = Object.freeze({ kind: () => value.schema, auth: () => ('operationId' in value ? copy(value) : undefined), epoch: () => ('parentGeneration' in value ? copy(value) : undefined), health: () => ('challenge' in value ? copy(value) : undefined) }) as Command;
  commands.set(c, { value, raw: Uint8Array.from(raw) });
  return c;
}
function wrapAck(value: AckValue): ACK { return Object.freeze({ kind: () => value.schema, auth: () => ('operationId' in value ? copy(value) : undefined), epoch: () => ('parentGeneration' in value ? copy(value) : undefined), health: () => ('challenge' in value ? copy(value) : undefined) }) as ACK; }
function commandBinding(value: CommandValue): void { if ('supersededCredentialEpoch' in value && value.supersededCredentialEpoch !== value.target.credentialEpoch) {
  fail('binding_mismatch');
} }
export function decodeCommand(raw: Uint8Array): Command {
  const obj = parseRaw(raw);
  if (!object(obj)) {
    fail('invalid_variant');
  }
  let value: CommandValue;
  switch (obj.schema) {
    case AUTH:
      value = decode<AuthCommand>(raw, 'AuthCommand', AUTH);
      break;
    case EPOCH:
      value = decode<EpochCommand>(raw, 'EpochCommand', EPOCH);
      break;
    case HEALTH:
      value = decode<Health4>(raw, 'Health4', HEALTH);
      break;
    default: return fail('invalid_variant');
  }
  commandBinding(value);
  return wrapCommand(value, raw);
}
export function newAuthCommand(binding: Binding, operationId: string, scope: string, reason: string, attempt: AttemptMeta): Command { return decodeCommand(encode({ userId: binding.userId, familyId: binding.familyId, target: copy(binding.target), schema: AUTH, version: 1, operationId, scope, reason, timestamp: attempt.timestamp, nonce: attempt.nonce, proof: ZERO_PROOF })); }
export function newEpochCommand(binding: Binding, parentGeneration: string, supersededCredentialEpoch: string, attempt: AttemptMeta): Command { return decodeCommand(encode({ userId: binding.userId, familyId: binding.familyId, target: copy(binding.target), schema: EPOCH, version: 1, parentGeneration, supersededCredentialEpoch, timestamp: attempt.timestamp, nonce: attempt.nonce, proof: ZERO_PROOF })); }
export function newHealthCommand(nodeBootId: string, challenge: string, activation: string, admissionAllowed: boolean, timestamp: string): Command { return decodeCommand(encode({ schema: HEALTH, version: 1, protocol: 4, nodeBootId, challenge, activation, admissionAllowed, timestamp, proof: ZERO_PROOF })); }
function digest(raw: Uint8Array | string): string { return createHash('sha256').update(raw).digest('hex'); }
function sign(domain: string, timestamp: string, fields: string[], secret: Uint8Array): string {
  if (secret.byteLength === 0 || [timestamp, ...fields].some(f => f.includes('\n'))) {
    fail('invalid_proof');
  }
  const key = createHmac('sha256', secret).update(domain).digest();
  return createHmac('sha256', key).update(['v1', timestamp, ...fields].join('\n')).digest('hex');
}
function verifyProof(domain: string, timestamp: string, proof: string, fields: string[], secret: Uint8Array, now: bigint, exact: boolean): void {
  const expected = sign(domain, timestamp, fields, secret);
  const a = Buffer.from(expected, 'utf8'), b = Buffer.from(proof, 'utf8');
  if (a.length !== b.length || !timingSafeEqual(a, b)) {
    fail('invalid_proof');
  }
  if (typeof now !== 'bigint') {
    fail('invalid_decoder_state');
  }
  const age = now - BigInt(timestamp);
  if (age < (exact ? -1n : -30n) || age > (exact ? 6n : 30n)) {
    fail('proof_stale');
  }
}
function targetFields(t: Target): string[] { return [t.connectionGeneration, t.nodeBootId, t.socketId, t.credentialEpoch]; }
function bindingFields(b: Binding): string[] { return [b.userId, b.familyId, ...targetFields(b.target)]; }
function roomFields(r: Room): string[] { return [r.sessionGeneration, r.roomId, r.roomKind]; }
function commandProof(v: CommandValue): {
  domain: string;
  fields: string[];
  exact: boolean;
} {
  if ('operationId' in v) {
    return { domain: 'concord/auth-connection/eject/request/v1', fields: [v.schema, '1', v.operationId, v.scope, v.reason, v.userId, v.familyId, ...targetFields(v.target), v.nonce], exact: true };
  }
  if ('parentGeneration' in v) {
    return { domain: 'concord/auth-connection/credential-eject/request/v1', fields: [v.schema, '1', v.parentGeneration, v.userId, v.familyId, v.supersededCredentialEpoch, ...targetFields(v.target), v.nonce], exact: true };
  }
  return { domain: 'concord/auth-connection/health/request/v1', fields: [v.schema, '1', '4', v.nodeBootId, v.challenge, v.activation, String(v.admissionAllowed)], exact: false };
}
export function verifyCommand(c: Command, secret: Uint8Array, now: bigint): void { const v = commandState(c).value; const p = commandProof(v); verifyProof(p.domain, v.timestamp, v.proof, p.fields, secret, now, p.exact); commandBinding(v); }
export function signCommand(c: Command, secret: Uint8Array): Uint8Array { const v = commandState(c).value; const p = commandProof(v); commandBinding(v); return encodeTyped({ ...v, proof: sign(p.domain, v.timestamp, p.fields, secret) }, commandType(v)); }
function signAck(c: Command, kind: string, granted: boolean, secret: Uint8Array): Uint8Array {
  const v = commandState(c).value;
  if (v.schema !== kind) {
    fail('invalid_decoder_state');
  }
  commandBinding(v);
  const p = commandProof(v);
  const fields = [...p.fields, 'true'];
  if ('challenge' in v) {
    if (granted && (!v.admissionAllowed || v.activation !== 'active')) {
      fail('invalid_value');
    }
    fields.push(String(granted));
    return encodeTyped({ ...v, transportOK: true, admissionGranted: granted, proof: sign(p.domain.replace('/request/', '/ack/'), v.timestamp, fields, secret) }, 'HealthAck4');
  }
  return encodeTyped({ ...v, ok: true, proof: sign(p.domain.replace('/request/', '/ack/'), v.timestamp, fields, secret) }, 'operationId' in v ? 'AuthAck' : 'EpochAck');
}
export function signAuthAck(c: Command, secret: Uint8Array): Uint8Array { return signAck(c, AUTH, false, secret); }
export function signEpochAck(c: Command, secret: Uint8Array): Uint8Array { return signAck(c, EPOCH, false, secret); }
export function signHealthAck(c: Command, admissionGranted: boolean, secret: Uint8Array): Uint8Array { return signAck(c, HEALTH, admissionGranted, secret); }
export function verifyAck(raw: Uint8Array, original: Command, secret: Uint8Array, now: bigint): ACK {
  bounded(raw);
  const origin = commandState(original).value;
  let v: AckValue;
  switch (origin.schema) {
    case AUTH:
      v = decode<AuthAck>(raw, 'AuthAck', AUTH);
      break;
    case EPOCH:
      v = decode<EpochAck>(raw, 'EpochAck', EPOCH);
      break;
    case HEALTH:
      v = decode<HealthAck4>(raw, 'HealthAck4', HEALTH);
      break;
    default: return fail('invalid_decoder_state');
  }
  const p = commandProof(v), fields = [...p.fields, 'true'];
  if ('admissionGranted' in v) {
    fields.push(String(v.admissionGranted));
  }
  verifyProof(p.domain.replace('/request/', '/ack/'), v.timestamp, v.proof, fields, secret, now, p.exact);
  commandBinding(v);
  if (v.timestamp !== origin.timestamp || JSON.stringify(p.fields) !== JSON.stringify(commandProof(origin).fields)) {
    fail('binding_mismatch');
  }
  return wrapAck(v);
}
interface EndpointConfig {
  path: string;
  domain: string;
  schema: string;
  header: string;
  status: number;
  bearer: boolean;
  requestType: string;
  responseType: string;
}
function config(endpoint: Endpoint): EndpointConfig {
  const c = { path: '', domain: 'concord/auth-connection/', schema: '', header: 'X-Concord-Auth-Connection', status: 200, bearer: false, requestType: '', responseType: '' };
  switch (endpoint) {
    case 'connection_register':
      Object.assign(c, { path: '/api/v1/internal/voice/auth-connections', domain: c.domain + 'register', bearer: true, requestType: 'ConnectionRegister', responseType: 'RegistrationResult' });
      break;
    case 'connection_release':
      Object.assign(c, { path: '/api/v1/internal/voice/auth-connections/release', domain: c.domain + 'release', status: 204, requestType: 'ConnectionRelease' });
      break;
    case 'provisional_close':
      Object.assign(c, { path: '/api/v1/internal/voice/auth-connections/provisional-close', domain: c.domain + 'provisional-close', schema: PROVISIONAL, requestType: 'ProvisionalClose', responseType: 'ProvisionalCloseResult' });
      break;
    case 'room_register':
      Object.assign(c, { path: '/api/v1/voice/enforcement-sessions', domain: 'concord/auth-room-binding/register', schema: ROOM, header: 'X-Concord-Auth-Room', bearer: true, requestType: 'RoomRegister', responseType: 'RoomRegistrationResult' });
      break;
    case 'room_release':
      Object.assign(c, { path: '/api/v1/internal/voice/enforcement-sessions/release', domain: 'concord/auth-room-binding/release', schema: ROOM, header: 'X-Concord-Auth-Room', status: 204, requestType: 'RoomRelease' });
      break;
    case 'health_bootstrap':
      Object.assign(c, { path: '/api/v1/internal/voice/enforcement/health', domain: c.domain + 'health/bootstrap', requestType: 'HealthBootstrap', responseType: 'HealthBootstrapResult' });
      break;
    case 'boot_retire':
      Object.assign(c, { path: '/api/v1/internal/voice/auth-connections/retire-boot', domain: c.domain + 'retire-boot', schema: RETIRE, status: 204, requestType: 'BootRetire' });
      break;
    default: return fail('invalid_decoder_state');
  }
  return c;
}
function headerEqual(name: string, expected: string): boolean {
  for (const ch of name) { if ((ch.codePointAt(0) ?? 0) > 127) { return false; } }
  return name.toLowerCase() === expected.toLowerCase();
}
function singleHeader(headers: Header[], name: string): string {
  const values = headers.filter(h => headerEqual(h.name, name));
  if (values.length !== 1) {
    fail('invalid_headers');
  }
  return values[0].value;
}
function proofHeaders(headers: Header[], prefix: string): {
  timestamp: string;
  nonce: string;
  proof: string;
} {
  const timestamp = singleHeader(headers, prefix + '-Timestamp');
  if (!timestampOK(timestamp)) {
    fail('invalid_headers');
  }
  const nonce = singleHeader(headers, prefix + '-Nonce');
  if (!HEX64.test(nonce)) {
    fail('invalid_headers');
  }
  const proof = singleHeader(headers, prefix + '-Proof');
  if (!HEX64.test(proof)) {
    fail('invalid_headers');
  }
  return { timestamp, nonce, proof };
}
function makeHeaders(prefix: string, timestamp: string, nonce: string, proof: string): Header[] { return [{ name: prefix + '-Timestamp', value: timestamp }, { name: prefix + '-Nonce', value: nonce }, { name: prefix + '-Proof', value: proof }]; }
function wrapRequest(state: RequestState): Request {
  const v = state.value, e = state.endpoint;
  const r = Object.freeze({ endpoint: () => e,
    connectionRegister: () => e === 'connection_register' ? copy(v) as ConnectionRegister : undefined,
    connectionRelease: () => e === 'connection_release' ? copy(v) as ConnectionRelease : undefined,
    provisionalClose: () => e === 'provisional_close' ? copy(v) as ProvisionalClose : undefined,
    roomRegister: () => e === 'room_register' ? copy(v) as RoomRegister : undefined,
    roomRelease: () => e === 'room_release' ? copy(v) as RoomRelease : undefined,
    healthBootstrap: () => e === 'health_bootstrap' ? copy(v) as HealthBootstrap : undefined,
    bootRetire: () => e === 'boot_retire' ? copy(v) as BootRetire : undefined }) as Request;
  requests.set(r, state);
  return r;
}
export function decodeHttpRequest(endpoint: Endpoint, wire: HTTPRequest): Request {
  bounded(wire.body);
  const c = config(endpoint);
  const value = decode<RequestValue>(wire.body, c.requestType, c.schema);
  if (wire.method !== 'POST' || wire.path !== c.path) {
    fail('noncanonical_endpoint');
  }
  const meta = proofHeaders(wire.headers, c.header);
  let token = '';
  if (c.bearer) {
    const auth = singleHeader(wire.headers, 'Authorization');
    if (!auth.startsWith('Bearer ') || auth.length <= 7 || !unicodeOK(auth) || /[\r\n]/.test(auth)) {
      fail('invalid_headers');
    }
    token = auth.slice(7);
  }
  else if (wire.headers.some(h => headerEqual(h.name, 'Authorization'))) {
    fail('invalid_headers');
  }
  if ('nonce' in value && value.nonce !== meta.nonce) {
    fail('invalid_headers');
  }
  return wrapRequest({ endpoint, value, wire: copy(wire), ...meta, tokenDigest: digest(token), bodyDigest: digest(wire.body) });
}
function requestTarget(r: RequestState): Target | undefined {
  const v = r.value;
  if ('target' in v) {
    return v.target;
  }
  if ('binding' in v) {
    return v.binding.target;
  }
  if ('connectionGeneration' in v) {
    return { connectionGeneration: v.connectionGeneration, nodeBootId: v.nodeBootId, socketId: v.socketId, credentialEpoch: v.credentialEpoch };
  }
  return undefined;
}
function requestBoot(r: RequestState): string {
  const t = requestTarget(r);
  if (t) {
    return t.nodeBootId;
  }
  if ('nodeBootId' in r.value) {
    return r.value.nodeBootId;
  }
  return fail('invalid_decoder_state');
}
function requestBinding(r: RequestState): Binding | undefined {
  const v = r.value;
  if ('userId' in v) {
    return { userId: v.userId, familyId: v.familyId, target: v.target };
  }
  return 'binding' in v ? v.binding : undefined;
}
function requestRoom(r: RequestState): Room | undefined {
  const v = r.value;
  if ('room' in v) {
    return v.room;
  }
  if ('sessionGeneration' in v) {
    return { sessionGeneration: v.sessionGeneration, roomId: v.roomId, roomKind: v.roomKind };
  }
  return undefined;
}
function requestFields(r: RequestState, c: EndpointConfig): string[] { return [...(c.schema ? [c.schema] : []), '1', 'POST', c.path, ...(c.bearer ? [r.tokenDigest] : []), r.bodyDigest, requestBoot(r), r.nonce]; }
export function verifyHttpRequest(request: Request, secret: Uint8Array, now: bigint): void { const r = requestState(request), c = config(r.endpoint); verifyProof(c.domain + '/request/v1', r.timestamp, r.proof, requestFields(r, c), secret, now, false); }
export function signHttpRequest(endpoint: Endpoint, body: Uint8Array, bearerToken: string, attempt: AttemptMeta, secret: Uint8Array): {
  wire: HTTPRequest;
  request: Request;
} {
  const c = config(endpoint);
  if (c.bearer !== (bearerToken !== '')) {
    fail('invalid_decoder_state');
  }
  const wire: HTTPRequest = { method: 'POST', path: c.path, headers: makeHeaders(c.header, attempt.timestamp, attempt.nonce, ZERO_PROOF), body: Uint8Array.from(body) };
  if (c.bearer) {
    wire.headers.push({ name: 'Authorization', value: 'Bearer ' + bearerToken });
  }
  const request = decodeHttpRequest(endpoint, wire), state = requestState(request);
  const proof = sign(c.domain + '/request/v1', attempt.timestamp, requestFields(state, c), secret);
  wire.headers[2].value = proof;
  // The mutable wire returned to the transport never aliases retained proof state.
  state.proof = proof;
  state.wire = copy(wire);
  return { wire, request };
}
function sameTarget(a: Target | undefined, b: Target | undefined): boolean { return a !== undefined && b !== undefined && targetFields(a).every((f, i) => f === targetFields(b)[i]); }
function sameBinding(a: Binding, b: Binding): boolean { return a.userId === b.userId && a.familyId === b.familyId && sameTarget(a.target, b.target); }
/** Expectations an endpoint requires: a user for registration, a matching
 * binding for room and release calls, and none for health and boot retirement. */
function expectedFitsEndpoint(r: RequestState, user: string, binding: Binding | undefined): boolean {
  switch (r.endpoint) {
    case 'connection_register':
      return user !== '' && binding === undefined;
    case 'room_register':
    case 'connection_release':
    case 'room_release': {
      if (!binding) return false;
      const known = requestBinding(r);
      return !known || sameBinding(known, binding);
    }
    case 'provisional_close':
      return true;
    case 'health_bootstrap':
    case 'boot_retire':
      return user === '' && binding === undefined;
  }
  return false;
}
export function newRequestContext(request: Request, expected: ExpectedResponse): RequestContext {
  const r = requestState(request);
  if (!object(expected) || Object.keys(expected).some(key => key !== 'userId' && key !== 'binding')) {
    fail('invalid_decoder_state');
  }
  const e = copy<ExpectedResponse>(expected);
  const user = e.userId ?? '';
  if (user !== '' && !UUID.test(user)) {
    fail('invalid_decoder_state');
  }
  if (e.binding !== undefined) {
    try {
      decode<Binding>(encode(e.binding), 'Binding');
    }
    catch {
      return fail('invalid_decoder_state');
    }
    if (!sameTarget(e.binding.target, requestTarget(r)) || (user !== '' && user !== e.binding.userId)) {
      fail('invalid_decoder_state');
    }
  }
  if (!expectedFitsEndpoint(r, user, e.binding)) {
    fail('invalid_decoder_state');
  }
  const ctx = Object.freeze({}) as RequestContext;
  contexts.set(ctx, { request: copy(r), expected: e });
  return ctx;
}
function responseBody(endpoint: Endpoint, raw: Uint8Array, c: EndpointConfig): SuccessValue | null {
  bounded(raw);
  if (c.status === 204) {
    if (raw.byteLength !== 0) {
      fail('invalid_variant');
    }
    return null;
  }
  return decode<SuccessValue>(raw, c.responseType, endpoint === 'provisional_close' ? PROVISIONAL_RESULT : c.schema);
}
function responseBinding(v: SuccessValue | null): Binding | undefined {
  if (v === null) {
    return undefined;
  }
  if ('userId' in v) {
    return { userId: v.userId, familyId: v.familyId, target: v.target };
  }
  return 'binding' in v ? v.binding : undefined;
}
function responseMatch(r: RequestState, v: SuccessValue | null, e: ExpectedResponse): void {
  const b = responseBinding(v);
  if (b && (!sameTarget(b.target, requestTarget(r)) || (e.userId && b.userId !== e.userId) || (e.binding && !sameBinding(b, e.binding)))) {
    fail('binding_mismatch');
  }
  if (v && 'disposition' in v) {
    const q = r.value;
    if (!('registrationTokenDigest' in q) || !sameTarget(v.target, q.target) || v.registrationTokenDigest !== q.registrationTokenDigest || v.registrationBodyDigest !== q.registrationBodyDigest) {
      fail('binding_mismatch');
    }
  }
  if (v && 'room' in v) {
    const room = requestRoom(r);
    if (!room || roomFields(v.room).some((f, i) => f !== roomFields(room)[i])) {
      fail('binding_mismatch');
    }
  }
  if (v && 'nodeBootId' in v && v.nodeBootId !== requestBoot(r)) {
    fail('binding_mismatch');
  }
}
function responseFields(r: RequestState, v: SuccessValue | null, raw: Uint8Array, c: EndpointConfig): string[] {
  const schema = r.endpoint === 'provisional_close' ? PROVISIONAL_RESULT : c.schema;
  const fields = [...(schema ? [schema] : []), '1', c.path, r.timestamp, r.nonce, r.bodyDigest];
  if (v && 'userId' in v) {
    return [...fields, '4', ...bindingFields({ userId: v.userId, familyId: v.familyId, target: v.target })];
  }
  if (v && 'room' in v) {
    return [...fields, '200', '4', ...bindingFields(v.binding), ...roomFields(v.room)];
  }
  if (v) {
    return [...fields, '200', digest(raw)];
  }
  if (r.endpoint === 'boot_retire') {
    return [...fields, '204', requestBoot(r)];
  }
  const b = requestBinding(r);
  if (!b) {
    fail('invalid_decoder_state');
  }
  fields.push('204', ...bindingFields(b));
  if (r.endpoint === 'room_release') {
    const room = requestRoom(r);
    if (!room) {
      fail('invalid_decoder_state');
    }
    fields.push(...roomFields(room));
  }
  return fields;
}
function wrapResponse(endpoint: Endpoint, v: SuccessValue | null): Response {
  return Object.freeze({ endpoint: () => endpoint, empty: () => v === null,
    registration: () => endpoint === 'connection_register' ? copy(v) as RegistrationResult : undefined,
    provisionalClose: () => endpoint === 'provisional_close' ? copy(v) as ProvisionalCloseResult : undefined,
    roomRegistration: () => endpoint === 'room_register' ? copy(v) as RoomRegistrationResult : undefined,
    healthBootstrap: () => endpoint === 'health_bootstrap' ? copy(v) as HealthBootstrapResult : undefined }) as Response;
}
export function verifyHttpResponse(endpoint: Endpoint, wire: HTTPResponse, context: RequestContext, secret: Uint8Array, now: bigint): Response {
  bounded(wire.body);
  const ctx = contexts.get(context);
  if (ctx?.request.endpoint !== endpoint) {
    fail('invalid_decoder_state');
  }
  const r = ctx.request, c = config(endpoint);
  if (wire.status !== c.status) {
    fail('unexpected_status');
  }
  const v = responseBody(endpoint, wire.body, c), meta = proofHeaders(wire.headers, c.header + '-Response');
  if (meta.nonce !== r.nonce) {
    fail('invalid_headers');
  }
  verifyProof(c.domain + '/response/v1', meta.timestamp, meta.proof, responseFields(r, v, wire.body, c), secret, now, false);
  responseMatch(r, v, ctx.expected);
  return wrapResponse(endpoint, v);
}
export function signHttpResponse(request: Request, payload: ResponsePayload, timestamp: string, secret: Uint8Array): HTTPResponse {
  const r = requestState(request), c = config(r.endpoint);
  if (!timestampOK(timestamp)) {
    fail('invalid_value');
  }
  if ((payload === null) !== (c.status === 204)) {
    fail('invalid_decoder_state');
  }
  const initial = payload === null ? new Uint8Array() : encode(payload);
  const v = responseBody(r.endpoint, initial, c);
  const raw = v === null ? new Uint8Array() : encodeTyped(v, c.responseType);
  responseMatch(r, v, { binding: requestBinding(r) });
  const proof = sign(c.domain + '/response/v1', timestamp, responseFields(r, v, raw, c), secret);
  return { status: c.status, headers: makeHeaders(c.header + '-Response', timestamp, r.nonce, proof), body: raw };
}
