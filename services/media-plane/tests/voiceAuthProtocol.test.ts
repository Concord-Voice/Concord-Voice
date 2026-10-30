import { createHash, createHmac } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import * as p from '../src/lib/voiceAuthProtocol';

type Vector = {
 id: string; transport: string; direction: string; rawHex: string; rawSHA256: string;
 method: string; path: string; requestId: string; status: number; headers: [string,string][];
 verifierUnixSeconds: string;
 signing?: {domain: string; timestamp: string; orderedFields: string[]; signingBytesHex: string; derivedKeyHex: string; expectedMacHex: string};
 expected: {decode: string; accept: boolean; decoded: unknown};
};
const corpus = JSON.parse(readFileSync(new URL('../../control-plane/internal/api/testdata/auth_connection_protocol.json', import.meta.url), 'utf8')) as {testOnlySecretHex: string; records: Vector[]};
const records = new Map(corpus.records.map(v => [v.id, v]));
const secret = Buffer.from(corpus.testOnlySecretHex, 'hex');
function record(id: string): Vector { const v = records.get(id); if (!v) throw new Error(`Missing vector ${id}`); return v; }
function endpoint(v: Vector): p.Endpoint {
 if (v.id.startsWith('path-')) return 'connection_register';
 switch(v.path.split('?')[0].replace(/\/$/, '')) {
 case '/api/v1/internal/voice/auth-connections': return 'connection_register';
 case '/api/v1/internal/voice/auth-connections/release': return 'connection_release';
 case '/api/v1/internal/voice/auth-connections/provisional-close': return 'provisional_close';
 case '/api/v1/voice/enforcement-sessions': return 'room_register';
 case '/api/v1/internal/voice/enforcement-sessions/release': return 'room_release';
 case '/api/v1/internal/voice/enforcement/health': return 'health_bootstrap';
 case '/api/v1/internal/voice/auth-connections/retire-boot': return 'boot_retire';
 default: throw new Error(`Unknown fixture path ${v.path}`);
 }
}
const goldenBinding = () => ({userId: '00000003-1111-4111-8111-111111111111', familyId: '00000004-1111-4111-8111-111111111111', target: {connectionGeneration: '00000001-1111-4111-8111-111111111111', nodeBootId: '00000002-1111-4111-8111-111111111111', socketId: 'test-socket-é', credentialEpoch: '0123456789abcdef0123456789abcdef'}});
const body = (v: Vector) => Buffer.from(v.rawHex, 'hex');
const headers = (v: Vector) => v.headers.map(([name,value]) => ({name,value}));
const requestWire = (v: Vector) => ({method: v.method, path: v.path, headers: headers(v), body: body(v)});
function contextFor(v: Vector) {
 const ep = endpoint(v); const request = p.decodeHttpRequest(ep, requestWire(v));
 const expected: p.ExpectedResponse = ep === 'connection_register' ? {userId: goldenBinding().userId}
 : ['connection_release','room_register','room_release'].includes(ep) ? {binding: goldenBinding()} : {};
 return {request, context: p.newRequestContext(request, expected)};
}
// These vectors need persisted commitments or actual native ownership to refuse.
// A valid codec result alone cannot settle the retained obligation.
const stateOnly = new Set(['valid-proof-wrong-owned-boot','valid-proof-wrong-owned-generation','valid-proof-wrong-owned-family','provisional-wrong-original-token-digest','provisional-wrong-original-body-digest','provisional-wrong-target']);
function verify(v: Vector) {
 const now = BigInt(v.verifierUnixSeconds);
 if(v.transport === 'nats') {
  if(v.direction === 'request') return p.verifyCommand(p.decodeCommand(body(v)), secret, now);
  return p.verifyAck(body(v), p.decodeCommand(body(record(v.requestId))), secret, now);
 }
 if(v.direction === 'request') return p.verifyHttpRequest(p.decodeHttpRequest(endpoint(v), requestWire(v)), secret, now);
 return p.verifyHttpResponse(endpoint(v), {status: v.status, headers: headers(v), body: body(v)}, contextFor(record(v.requestId)).context, secret, now);
}
// Decoding, payload expectations and proof rejection are separate assertions:
// an AssertionError can never count as a successful protocol refusal.
function checkCommandVector(v: Vector, decode: typeof p.decodeCommand = p.decodeCommand): void {
 if(v.expected.decode === 'reject') {
  expect(() => decode(body(v))).toThrow(p.ProtocolError);
  return;
 }
 const command=decode(body(v));
 expect(command.auth() ?? command.epoch() ?? command.health()).toEqual(v.expected.decoded);
 const check=() => p.verifyCommand(command,secret,BigInt(v.verifierUnixSeconds));
 if(v.expected.accept || stateOnly.has(v.id)) expect(check).not.toThrow();
 else expect(check).toThrow(p.ProtocolError);
}
describe('shared raw protocol vectors', () => {
 it('pins the unchanged corpus and all twenty proof domains', () => {
  expect(corpus.records).toHaveLength(143);
  expect(new Set(corpus.records.flatMap(v => v.signing ? [v.signing.domain] : [] )).size).toBe(20);
 });
 for(const v of corpus.records) it(v.id, () => {
  expect(createHash('sha256').update(body(v)).digest('hex')).toBe(v.rawSHA256);
  if(v.signing) {
   const s=v.signing;
   expect(Buffer.from(['v1',s.timestamp,...s.orderedFields].join('\n'))).toEqual(Buffer.from(s.signingBytesHex,'hex'));
   const key=createHmac('sha256',secret).update(s.domain).digest();
   expect(key.toString('hex')).toBe(s.derivedKeyHex);
   expect(createHmac('sha256',key).update(Buffer.from(s.signingBytesHex,'hex')).digest('hex')).toBe(s.expectedMacHex);
  }
  if(v.transport === 'nats' && v.direction === 'request') {
   checkCommandVector(v);
  } else if(v.transport === 'http' && v.direction === 'request') {
   // Fixture decode denotes raw body validity. Header/URI validation belongs to
   // this same public decoder and must fail before the proof-only verifier.
   if(v.expected.decode === 'reject' || v.id.startsWith('headers-') || v.id.startsWith('path-')) {
    expect(() => p.decodeHttpRequest(endpoint(v), requestWire(v))).toThrow(p.ProtocolError);
    return;
   }
   const request=p.decodeHttpRequest(endpoint(v), requestWire(v));
   expect(request.connectionRegister() ?? request.connectionRelease() ?? request.provisionalClose() ?? request.roomRegister() ?? request.roomRelease() ?? request.healthBootstrap() ?? request.bootRetire()).toEqual(v.expected.decoded);
   const check=() => p.verifyHttpRequest(request,secret,BigInt(v.verifierUnixSeconds));
   if(v.expected.accept || stateOnly.has(v.id)) expect(check).not.toThrow();
   else expect(check).toThrow(p.ProtocolError);
  } else {
   // ACK and HTTP response APIs combine strict decoding with verification.
   if(v.expected.accept) expect(() => verify(v)).not.toThrow();
   else expect(() => verify(v)).toThrow(p.ProtocolError);
  }
 });
});
describe('request signers and private copies', () => {
 for(const v of corpus.records.filter(v => v.transport==='http' && v.direction==='request' && v.expected.accept)) it(v.id, () => {
  const raw=body(v); const s=v.signing; if(!s) throw new Error('missing signing fixture');
  const token=v.headers.find(([name]) => name==='Authorization')?.[1].slice('Bearer '.length) ?? '';
  const {wire,request}=p.signHttpRequest(endpoint(v),raw,token,{timestamp:s.timestamp,nonce:v.headers[1][1]},secret);
  expect(Buffer.from(wire.body)).toEqual(raw); expect(wire.path).toBe(v.path);
  expect(wire.headers.find(h => h.name.endsWith('-Proof'))?.value).toBe(s.expectedMacHex);
  raw[0]=33; wire.body[0]=33; wire.headers[0].value='0';
  expect(() => p.verifyHttpRequest(request,secret,BigInt(v.verifierUnixSeconds))).not.toThrow();
 });
 it('rejects forged wrapper objects and malformed factories', () => {
  const now=1793000000n;
  expect(() => p.verifyCommand({} as p.Command,secret,now)).toThrow();
  expect(() => p.verifyHttpRequest({} as p.Request,secret,now)).toThrow();
  expect(() => p.signCommand({} as p.Command,secret)).toThrow();
  expect(() => p.signAuthAck({} as p.Command,secret)).toThrow();
  expect(() => p.signEpochAck({} as p.Command,secret)).toThrow();
  expect(() => p.signHealthAck({} as p.Command,false,secret)).toThrow();
  expect(() => p.newRequestContext({} as p.Request,{})).toThrow();
  expect(() => p.signHttpResponse({} as p.Request,null,'1793000000',secret)).toThrow();
  const v=record('connection-register-response');
  expect(() => p.verifyHttpResponse('connection_register',{status:v.status,headers:headers(v),body:body(v)},{} as p.RequestContext,secret,now)).toThrow();
  const request=p.decodeHttpRequest('connection_register',requestWire(record('connection-register')));
  expect(() => p.newRequestContext(request,{})).toThrow();
  expect(() => p.signHttpRequest('connection_register',Buffer.from('{}'),'',{timestamp:'',nonce:''},secret)).toThrow();
  expect(() => p.newAuthCommand(goldenBinding(),'','','',{timestamp:'',nonce:''})).toThrow();
  expect(() => p.newEpochCommand(goldenBinding(),'','',{timestamp:'',nonce:''})).toThrow();
  expect(() => p.newHealthCommand('','','',true,'')).toThrow();
 });
});
describe('command factories and ACK signers match independent golden MACs', () => {
 for(const v of corpus.records.filter(v => v.transport==='nats' && v.direction==='request' && v.expected.accept)) it(v.id, () => {
  const original=p.decodeCommand(body(v)); let built: p.Command;
  const a=original.auth(), e=original.epoch(), h=original.health();
  if(a) built=p.newAuthCommand({userId:a.userId,familyId:a.familyId,target:a.target},a.operationId,a.scope,a.reason,{timestamp:a.timestamp,nonce:a.nonce});
  else if(e) built=p.newEpochCommand({userId:e.userId,familyId:e.familyId,target:e.target},e.parentGeneration,e.supersededCredentialEpoch,{timestamp:e.timestamp,nonce:e.nonce});
  else if(h) built=p.newHealthCommand(h.nodeBootId,h.challenge,h.activation,h.admissionAllowed,h.timestamp);
  else throw new Error('No typed command accessor');
  expect(JSON.parse(Buffer.from(p.signCommand(built,secret)).toString())).toEqual(JSON.parse(body(v).toString()));
  if(a) {a.target.socketId='changed'; expect(() => p.verifyCommand(original,secret,BigInt(v.verifierUnixSeconds))).not.toThrow();}
  const av=records.get(v.id+'-ack'); if(!av) return;
  let ack: Uint8Array;
  if(a) {ack=p.signAuthAck(original,secret); expect(() => p.signEpochAck(original,secret)).toThrow();}
  else if(e) {ack=p.signEpochAck(original,secret); expect(() => p.signAuthAck(original,secret)).toThrow();}
  else {const expected=JSON.parse(body(av).toString()) as {admissionGranted:boolean}; ack=p.signHealthAck(original,expected.admissionGranted,secret);}
  expect(JSON.parse(Buffer.from(ack).toString())).toEqual(JSON.parse(body(av).toString()));
  expect(p.verifyAck(ack,original,secret,BigInt(av.verifierUnixSeconds)).kind()).toBe(original.kind());
 });
});
describe('response signers use retained golden request context', () => {
 for(const v of corpus.records.filter(v => v.transport==='http' && v.direction==='response' && v.expected.accept)) it(v.id, () => {
  const {request,context}=contextFor(record(v.requestId)); const s=v.signing; if(!s) throw new Error('missing signing fixture');
  const payload: p.ResponsePayload = v.status===204 ? null : JSON.parse(body(v).toString()) as p.ResponsePayload;
  const wire=p.signHttpResponse(request,payload,s.timestamp,secret);
  expect(wire.status).toBe(v.status);
  if(v.status===204) expect(wire.body).toHaveLength(0);
  else expect(JSON.parse(Buffer.from(wire.body).toString())).toEqual(JSON.parse(body(v).toString()));
  const fields=[...s.orderedFields];
  if(endpoint(v)==='health_bootstrap') fields[fields.length-1]=createHash('sha256').update(wire.body).digest('hex');
  const key=createHmac('sha256',secret).update(s.domain).digest();
  const mac=createHmac('sha256',key).update(['v1',s.timestamp,...fields].join('\n')).digest('hex');
  expect(wire.headers.find(h => h.name.endsWith('-Proof'))?.value).toBe(mac);
  const verified=p.verifyHttpResponse(endpoint(v),wire,context,secret,BigInt(v.verifierUnixSeconds)); expect(verified.empty()).toBe(v.status===204); expect(verified.endpoint()).toBe(endpoint(v));
  expect(() => p.signHttpResponse(request,{version:1} as p.RegistrationResult,s.timestamp,secret)).toThrow();
 });
});
it('classifies rejection priority and refuses forbidden health grants', () => {
 const reasons: Record<string,string> = {
 'raw-duplicate-version':'malformed_body','raw-escaped-duplicate-version':'malformed_body','raw-nested-escaped-duplicate-boot':'malformed_body','raw-invalid-utf8':'malformed_body','raw-unknown-schema':'invalid_variant','raw-health-spoof-unknown-schema':'invalid_variant','raw-missing-version':'invalid_variant','raw-noncanonical-uuid':'invalid_value','exact-age-7':'proof_stale','proof-wrong-domain':'invalid_proof','headers-duplicate-timestamp':'invalid_headers','headers-comma-nonce':'invalid_headers','headers-missing-proof':'invalid_headers','path-query':'noncanonical_endpoint','body-size-4097':'payload_too_large'};
 for(const [id,reason] of Object.entries(reasons)) expect(() => verify(record(id)),id).toThrowError(expect.objectContaining({reason}));
 const bad=p.decodeCommand(body(record('proof-wrong-domain')));
 expect(() => p.verifyCommand(bad,secret,1793000100n)).toThrowError(expect.objectContaining({reason:'invalid_proof'}));
 const prepared=p.decodeCommand(body(record('health-prepared-false-false')));
 expect(() => p.signHealthAck(prepared,true,secret)).toThrow();
 expect(() => p.newHealthCommand(goldenBinding().target.nodeBootId,'ab'.repeat(32),'prepared',true,'1793000000')).toThrow();
});
it('returns independent typed ACK/response payload copies and copies context expectations', () => {
 for(const id of ['eject-session-logout-ack','credential-eject-ack','health-active-true-true-ack']) {
  const v=record(id);const cmd=p.decodeCommand(body(record(v.requestId)));const ack=p.verifyAck(body(v),cmd,secret,BigInt(v.verifierUnixSeconds));
  expect(ack.auth() ?? ack.epoch() ?? ack.health()).toEqual(v.expected.decoded);
  const a=ack.auth();if(a){a.target.socketId='changed';expect(ack.auth()?.target.socketId).toBe(goldenBinding().target.socketId);expect(ack.epoch()).toBeUndefined();}
 }
 for(const id of ['connection-register-response','room-attach-channel-response','provisional-close-registered-response','bootstrap-active-true-response']){
  const v=record(id);const result=p.verifyHttpResponse(endpoint(v),{status:v.status,headers:headers(v),body:body(v)},contextFor(record(v.requestId)).context,secret,BigInt(v.verifierUnixSeconds));
  expect(result.registration() ?? result.roomRegistration() ?? result.provisionalClose() ?? result.healthBootstrap()).toEqual(v.expected.decoded);
  const x=result.provisionalClose();if(x?.binding){x.binding.target.socketId='changed';expect(result.provisionalClose()?.binding?.target.socketId).toBe(goldenBinding().target.socketId);}
 }
 const rv=record('room-attach-channel');const req=p.decodeHttpRequest(endpoint(rv),requestWire(rv));const binding=goldenBinding();const ctx=p.newRequestContext(req,{binding});binding.target.socketId='changed';binding.familyId='00000014-1111-4111-8111-111111111111';
 const v=record('room-attach-channel-response');expect(() => p.verifyHttpResponse(endpoint(v),{status:v.status,headers:headers(v),body:body(v)},ctx,secret,BigInt(v.verifierUnixSeconds))).not.toThrow();
 const registration=p.decodeHttpRequest('connection_register',requestWire(record('connection-register')));const wrong=p.newRequestContext(registration,{userId:'00000014-1111-4111-8111-111111111111'});const response=record('connection-register-response');
 expect(() => p.verifyHttpResponse('connection_register',{status:response.status,headers:headers(response),body:body(response)},wrong,secret,1793000000n)).toThrowError(expect.objectContaining({reason:'binding_mismatch'}));
});
it('keeps exact freshness arithmetic integral near the signed int64 limit', () => {
 const timestamp=9223372036854775800n;
 const made=p.newAuthCommand(goldenBinding(),'00000007-1111-4111-8111-111111111111','session','logout',{timestamp:timestamp.toString(),nonce:'ab'.repeat(32)});
 const command=p.decodeCommand(p.signCommand(made,secret));
 for(const age of [-2n,-1n,0n,6n,7n]){const verify=() => p.verifyCommand(command,secret,timestamp+age);if(age>=-1n&&age<=6n)expect(verify).not.toThrow();else expect(verify).toThrowError(expect.objectContaining({reason:'proof_stale'}));}
});
it('bounds responses before status, checks status before shape, and requires zero-byte 204', () => {
 const v=record('connection-release-response'), context=contextFor(record(v.requestId)).context;
 for(const raw of [' ','null','{}']) expect(() => p.verifyHttpResponse(endpoint(v),{status:204,headers:headers(v),body:Buffer.from(raw)},context,secret,1793000000n)).toThrow();
 for(const [status,raw,reason] of [[201,'x'.repeat(4097),'payload_too_large'],[201,'{','unexpected_status']] as const) expect(() => p.verifyHttpResponse(endpoint(v),{status,headers:headers(v),body:Buffer.from(raw)},context,secret,1793000000n)).toThrowError(expect.objectContaining({reason}));
 const request=p.decodeHttpRequest('connection_register',requestWire(record('connection-register'))), binding=goldenBinding();binding.target.socketId='wrong-local-socket';
 expect(() => p.newRequestContext(request,{userId:goldenBinding().userId,binding})).toThrow();
});
it('requires strict end-of-input for UUID, epoch, timestamp and security header nonce', () => {
 const base=body(record('eject-session-logout')).toString();
 for(const value of [goldenBinding().userId,goldenBinding().target.credentialEpoch,'1793000000']){
  const changed=base.replace(JSON.stringify(value),JSON.stringify(value+'\n'));expect(changed).not.toBe(base);
  expect(() => p.decodeCommand(Buffer.from(changed))).toThrowError(expect.objectContaining({reason:'invalid_value'}));
 }
 const wire=requestWire(record('connection-register'));const nonce=wire.headers.find(h => h.name.endsWith('-Nonce'));if(!nonce)throw new Error('Missing fixture nonce');nonce.value+='\n';
 expect(() => p.decodeHttpRequest('connection_register',wire)).toThrowError(expect.objectContaining({reason:'invalid_headers'}));
});

it('detects a BOM-accepting decoder and arbitrary exception impostors', () => {
 const bom=record('raw-bom');
 // Test-only injected mutant removes the exact malformed prefix, then executes
 // the actual decoder. Production and the shared artifact remain unchanged.
 const acceptsBom: typeof p.decodeCommand = raw => p.decodeCommand(raw.subarray(3));
 const oldCombinedCheck = () => {
  const command=acceptsBom(body(bom));
  expect(command.auth() ?? command.epoch() ?? command.health()).toEqual(bom.expected.decoded);
  p.verifyCommand(command,secret,BigInt(bom.verifierUnixSeconds));
 };
 // Reproduce the old masking cause: its generic throw matcher accepted this
 // payload AssertionError even though the malformed body had been admitted.
 expect(oldCombinedCheck).toThrowError(expect.objectContaining({name:'AssertionError'}));
 expect(() => checkCommandVector(bom,acceptsBom)).toThrowError(expect.objectContaining({name:'AssertionError'}));
 const throwsOrdinaryError: typeof p.decodeCommand = () => {throw new Error('test decoder crash');};
 expect(() => checkCommandVector(bom,throwsOrdinaryError)).toThrowError(expect.objectContaining({name:'AssertionError'}));
 // The unmodified decoder must supply an actual protocol refusal.
 expect(() => checkCommandVector(bom)).not.toThrow();
});
