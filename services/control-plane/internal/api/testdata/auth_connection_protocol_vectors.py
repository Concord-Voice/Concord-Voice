#!/usr/bin/env python3
"""Test-only #2272 golden-byte dossier generator/checker; no production imports.

Default: compare the committed fixture and cross-check all SHA256/HMAC outputs
with Node's standard crypto implementation. --write explicitly regenerates it.
Decoder/authorization expectations are contract assertions, not a second codec.
"""
import copy
import hashlib
import hmac
import json
from pathlib import Path
import subprocess
import sys

HERE = Path(__file__).resolve().parent
FIXTURE = HERE / 'auth_connection_protocol.json'
SECRET = b'TEST-ONLY-NOT-A-CREDENTIAL-2272-shared-secret'
TOKEN = 'TEST-ONLY-NOT-A-JWT-2272-bearer-\u00e9'
NOW = '1793000000'
VERSION_1 = b'"version":1'
TEST_SOCKET = b'test-socket-\xc3\xa9'
NONCE = 'ab' * 32
EPOCH = '0123456789abcdef' * 2

def uuid(n):
    return f'{n:08x}-1111-4111-8111-111111111111'

TARGET = {'connectionGeneration': uuid(1), 'nodeBootId': uuid(2),
          'socketId': 'test-socket-\u00e9', 'credentialEpoch': EPOCH}
BINDING = {'userId': uuid(3), 'familyId': uuid(4), 'target': TARGET}
ROOM = {'sessionGeneration': uuid(5), 'roomId': uuid(6), 'roomKind': 'channel'}
B = [BINDING['userId'], BINDING['familyId'], *TARGET.values()]
R = list(ROOM.values())
CP = '/api/v1/internal/voice/auth-connections'
CP_RELEASE = CP + '/release'
RP = '/api/v1/voice/enforcement-sessions'
RR = '/api/v1/internal/voice/enforcement-sessions/release'
HP = '/api/v1/internal/voice/enforcement/health'
CS = 'concord.auth-connection-provisional-close'
RS = 'concord.auth-room-binding'
BS = 'concord.auth-boot-retirement'
AC = 'concord/auth-connection/'
EJECT_REQUEST_DOMAIN = AC + 'credential-eject/request/v1'
AR = 'concord/auth-room-binding/'

def raw(obj):
    return json.dumps(obj, ensure_ascii=False, separators=(',', ':')).encode()

def digest(data):
    return hashlib.sha256(data).hexdigest()

def crypto(domain, timestamp, fields):
    assert all('\n' not in s for s in [timestamp, *fields])
    payload = '\n'.join(['v1', timestamp, *fields]).encode()
    key = hmac.digest(SECRET, domain.encode(), 'sha256')
    return {'domain': domain, 'timestamp': timestamp, 'orderedFields': fields,
            'signingBytesHex': payload.hex(), 'derivedKeyHex': key.hex(),
            'expectedMacHex': hmac.digest(key, payload, 'sha256').hex()}

def rejected(category):
    return {'decode': 'reject', 'classification': category, 'macMatches': None,
            'freshness': None, 'binding': None, 'accept': False}


# proof-wrong-<label>: signed field index, then where the decoded body carries it.
PROOF_EDITS = [('domain', None, EJECT_REQUEST_DOMAIN, None),
               ('boot', 8, uuid(20), ('target', 'nodeBootId')),
               ('generation', 7, uuid(21), ('target', 'connectionGeneration')),
               ('family', 6, uuid(22), (None, 'familyId')),
               ('nonce', 11, 'cd'*32, (None, 'nonce'))]
CLOSED_WIRE_EDITS = [
    ('bom', lambda w: b'\xef\xbb\xbf'+w), ('trailing-value', lambda w: w+b'{}'),
    ('unknown-field', lambda w: w[:-1]+b',"extra":true}'),
    ('nested-unknown-field', lambda w: w.replace(b'"target":{', b'"target":{"extra":true,')),
    ('null', lambda w: w.replace(VERSION_1, b'"version":null')),
    ('missing-version', lambda w: w.replace(b'"version":1,', b'')),
    ('unknown-schema', lambda w: w.replace(b'concord.auth-revocation', b'concord.unknown')),
    ('health-spoof-unknown-schema',
     lambda w: w.replace(b'concord.auth-revocation', b'concord.unknown')[:-1]+b',"kind":"health"}'),
    ('wrong-version', lambda w: w.replace(VERSION_1, b'"version":2')),
    ('noncanonical-uuid', lambda w: w.replace(uuid(1).encode(), uuid(1).upper().replace('4111', '4A11').encode())),
    ('nil-uuid', lambda w: w.replace(uuid(1).encode(), b'00000000-0000-0000-0000-000000000000')),
    ('non-rfc-uuid', lambda w: w.replace(uuid(1).encode(), uuid(1).replace('-8111-', '-0111-').encode())),
    ('uuid-version-zero', lambda w: w.replace(uuid(1).encode(), uuid(1).replace('-4111-', '-0111-').encode())),
    ('epoch-uppercase', lambda w: w.replace(EPOCH.encode(), EPOCH.upper().encode())),
    ('nonce-uppercase', lambda w: w.replace(NONCE.encode(), NONCE.upper().encode())),
    ('timestamp-leading-zero', lambda w: w.replace(NOW.encode(), ('0'+NOW).encode())),
    ('timestamp-number', lambda w: w.replace(b'"'+NOW.encode()+b'"', NOW.encode())),
    ('timestamp-zero', lambda w: w.replace(NOW.encode(), b'0')),
    ('timestamp-int64-overflow', lambda w: w.replace(NOW.encode(), b'9223372036854775808')),
    ('timestamp-float', lambda w: w.replace(NOW.encode(), b'1793000000.0')),
    ('socket-empty', lambda w: w.replace(TEST_SOCKET, b'')),
    ('socket-129-bytes', lambda w: w.replace(TEST_SOCKET, b'a'*129))]


def lower(flag):
    return str(flag).lower()


class Dossier:
    """Builds records in a fixed order; each section adds one family of vectors."""

    def __init__(self):
        self.records = []
        self.td = digest(TOKEN.encode())

    def add(self, id_, obj, domain, fields, *, path=None, response=False,
            request=None, status=None, exact=False, timestamp=NOW):
        c = crypto(domain, timestamp, fields)
        headers = []
        if path:
            body = raw(obj) if obj is not None else b''
            prefix = 'X-Concord-Auth-' + ('Room' if domain.startswith(AR) else 'Connection')
            prefix += '-Response' if response else ''
            headers = [[prefix+'-Timestamp', timestamp], [prefix+'-Nonce', NONCE],
                       [prefix+'-Proof', c['expectedMacHex']]]
        else:
            obj = {**obj, 'proof': c['expectedMacHex']}
            body = raw(obj)
        rec = {'id': id_, 'direction': 'response' if response else 'request',
               'transport': 'http' if path else 'nats', 'rawHex': body.hex(),
               'rawSHA256': digest(body), 'headers': headers, 'verifierUnixSeconds': NOW,
               'proofWindow': 'exact' if exact else 'generic', 'signing': c,
               'expected': {'decode': 'accept', 'decoded': obj, 'macMatches': True,
                            'freshness': True, 'binding': True, 'accept': True}}
        if path:
            rec.update(method='POST', path=path)
        if request:
            rec['requestId'] = request['id']
        if status:
            rec['status'] = status
        self.records.append(rec)
        return rec

    def req(self, id_, obj, suffix, path, fields, root=AC):
        rec = self.add(id_, obj, root+suffix+'/request/v1', fields, path=path)
        if suffix == 'register':
            rec['headers'].append(['Authorization', 'Bearer '+TOKEN])
        return rec

    def resp(self, id_, obj, suffix, path, fields, request, status=200, root=AC):
        return self.add(id_, obj, root+suffix+'/response/v1', fields, path=path,
                        response=True, request=request, status=status)

    def find(self, id_):
        return next(r for r in self.records if r['id'] == id_)

    def case(self, id_, base=None, **changes):
        base = base or self.seed
        v = copy.deepcopy(base)
        v['id'] = id_
        v['basedOn'] = base['id']
        v.update(changes)
        self.records.append(v)
        return v

    def malformed(self, id_, data, category, base=None):
        return self.case(id_, base, rawHex=data.hex(), rawSHA256=digest(data),
                         expected=rejected(category))

    def connection(self):
        regbody = {'version': 1, **TARGET}
        self.reg = reg = self.req('connection-register', regbody, 'register', CP,
                                  ['1', 'POST', CP, self.td, digest(raw(regbody)), TARGET['nodeBootId'], NONCE])
        reg['authorizationTokenUTF8Hex'] = TOKEN.encode().hex()
        self.response = self.resp('connection-register-response',
                                  {**BINDING, 'version': 1, 'voiceEnforcementProtocol': 4},
                                  'register', CP, ['1', CP, NOW, NONCE, reg['rawSHA256'], '4', *B], reg)
        releasebody = {**BINDING, 'version': 1}
        rel = self.req('connection-release', releasebody, 'release', CP_RELEASE,
                       ['1', 'POST', CP_RELEASE, digest(raw(releasebody)), TARGET['nodeBootId'], NONCE])
        self.resp('connection-release-response', None, 'release', CP_RELEASE,
                  ['1', CP_RELEASE, NOW, NONCE, rel['rawSHA256'], '204', *B], rel, 204)

    def ejects(self):
        for scope in ['session', 'user']:
            for reason in ['logout', 'explicit_revoke', 'displacement', 'theft', 'disablement', 'erasure']:
                command = {**BINDING, 'schema': 'concord.auth-revocation', 'version': 1,
                           'operationId': uuid(7), 'scope': scope, 'reason': reason,
                           'timestamp': NOW, 'nonce': NONCE}
                fields = [command['schema'], '1', command['operationId'], scope, reason, *B, NONCE]
                rid = f'eject-{scope}-{reason}'
                cmd = self.add(rid, command, AC+'eject/request/v1', fields, exact=True)
                self.add(rid+'-ack', {**command, 'ok': True}, AC+'eject/ack/v1', fields+['true'],
                         exact=True, response=True, request=cmd)
        self.ep = ep = {**BINDING, 'schema': 'concord.credential-connection-revocation', 'version': 1,
                        'parentGeneration': uuid(8), 'supersededCredentialEpoch': EPOCH,
                        'timestamp': NOW, 'nonce': NONCE}
        self.ef = ef = [ep['schema'], '1', ep['parentGeneration'], *B[:2], EPOCH, *B[2:], NONCE]
        ec = self.add('credential-eject', ep, EJECT_REQUEST_DOMAIN, ef, exact=True)
        self.add('credential-eject-ack', {**ep, 'ok': True}, AC+'credential-eject/ack/v1', ef+['true'],
                 exact=True, response=True, request=ec)

    def health(self):
        for activation, allowed, granted in [('prepared', False, False), ('active', False, False),
                                             ('active', True, False), ('active', True, True)]:
            h = {'schema': 'concord.voice-enforcement-health', 'version': 1, 'protocol': 4,
                 'nodeBootId': TARGET['nodeBootId'], 'challenge': NONCE, 'activation': activation,
                 'admissionAllowed': allowed, 'timestamp': NOW}
            hf = [h['schema'], '1', '4', h['nodeBootId'], NONCE, activation, lower(allowed)]
            hid = f'health-{activation}-{lower(allowed)}-{lower(granted)}'
            hc = self.add(hid, h, AC+'health/request/v1', hf)
            self.add(hid+'-ack', {**h, 'transportOK': True, 'admissionGranted': granted},
                     AC+'health/ack/v1', hf+['true', lower(granted)], response=True, request=hc)
        for activation, granted in [('prepared', False), ('active', False), ('active', True)]:
            hb = {'version': 1, 'nodeBootId': TARGET['nodeBootId'], 'nonce': NONCE}
            hr = self.req('bootstrap-'+activation+'-'+lower(granted), hb, 'health/bootstrap', HP,
                          ['1', 'POST', HP, digest(raw(hb)), TARGET['nodeBootId'], NONCE])
            hs = {'version': 1, 'voiceEnforcementProtocol': 4, 'nodeBootId': TARGET['nodeBootId'],
                  'activation': activation, 'transportOK': True, 'admissionGranted': granted}
            self.resp(hr['id']+'-response', hs, 'health/bootstrap', HP,
                      ['1', HP, NOW, NONCE, hr['rawSHA256'], '200', digest(raw(hs))], hr)

    def rooms(self):
        for mode in ['attach', 'renew']:
            for kind in ['channel', 'dm']:
                room = {**ROOM, 'roomKind': kind}
                r = {'schema': RS, 'version': 1, 'mode': mode, **TARGET, **room}
                rr = self.req(f'room-{mode}-{kind}', r, 'register', RP,
                              [RS, '1', 'POST', RP, self.td, digest(raw(r)), TARGET['nodeBootId'], NONCE], AR)
                rr['authorizationTokenUTF8Hex'] = TOKEN.encode().hex()
                rs = {'schema': RS, 'version': 1, 'voiceEnforcementProtocol': 4, 'binding': BINDING, 'room': room}
                self.resp(rr['id']+'-response', rs, 'register', RP,
                          [RS, '1', RP, NOW, NONCE, rr['rawSHA256'], '200', '4', *B, *room.values()], rr, root=AR)
                rb = {'schema': RS, 'version': 1, 'binding': BINDING, 'room': room}
                rl = self.req(f'room-release-{mode}-{kind}', rb, 'release', RR,
                              [RS, '1', 'POST', RR, digest(raw(rb)), TARGET['nodeBootId'], NONCE], AR)
                self.resp(rl['id']+'-response', None, 'release', RR,
                          [RS, '1', RR, NOW, NONCE, rl['rawSHA256'], '204', *B, *room.values()], rl, 204, AR)

    def provisional_and_boot(self):
        self.pb = pb = {'schema': CS, 'version': 1, 'target': TARGET, 'registrationTokenDigest': self.td,
                        'registrationBodyDigest': self.reg['rawSHA256']}
        self.pp = pp = CP+'/provisional-close'
        pc = self.req('provisional-close', pb, 'provisional-close', pp,
                      [CS, '1', 'POST', pp, digest(raw(pb)), TARGET['nodeBootId'], NONCE])
        for disposition in ['registered', 'unregistered', 'retired_boot']:
            result = {**pb, 'schema': CS+'-result', 'disposition': disposition}
            if disposition == 'registered':
                result['binding'] = BINDING
            self.resp('provisional-close-'+disposition+'-response', result, 'provisional-close', pp,
                      [CS+'-result', '1', pp, NOW, NONCE, pc['rawSHA256'], '200', digest(raw(result))], pc)
        bp = CP+'/retire-boot'
        bb = {'schema': BS, 'version': 1, 'nodeBootId': TARGET['nodeBootId']}
        br = self.req('boot-retirement', bb, 'retire-boot', bp,
                      [BS, '1', 'POST', bp, digest(raw(bb)), TARGET['nodeBootId'], NONCE])
        self.resp('boot-retirement-response', None, 'retire-boot', bp,
                  [BS, '1', bp, NOW, NONCE, br['rawSHA256'], '204', TARGET['nodeBootId']], br, 204)

    # Negative wire bytes are edited directly, never parsed/re-stringified.
    def raw_wire(self):
        self.seed = seed = self.find('eject-session-logout')
        wire = bytes.fromhex(seed['rawHex'])
        self.malformed('raw-duplicate-version', wire.replace(VERSION_1, b'"version":1,"version":1'),
                       'duplicate-decoded-key')
        self.malformed('raw-escaped-duplicate-version', wire.replace(VERSION_1, b'"version":1,"ver\\u0073ion":1'),
                       'duplicate-decoded-key')
        self.malformed('raw-nested-escaped-duplicate-boot',
                       wire.replace(b'"nodeBootId":', b'"nodeBootId":"'+TARGET['nodeBootId'].encode()+b'","nodeBoot\\u0049d":'),
                       'duplicate-decoded-key')
        for label, value in [('invalid-utf8', b'\xff'), ('overlong-utf8', b'\xc0\xaf'), ('utf8-surrogate', b'\xed\xa0\x80'),
                             ('unpaired-high-surrogate', b'\\ud800'), ('unpaired-low-surrogate', b'\\udfff'),
                             ('control-socket', b'\\u0001')]:
            self.malformed('raw-'+label, wire.replace(TEST_SOCKET, value), 'encoding-or-socket-validation')
        for label, edit in CLOSED_WIRE_EDITS:
            self.malformed('raw-'+label, edit(wire), 'closed-wire-validation')
        # Valid raw boundaries and scalar variants signed independently.
        for label, socket, epoch in [('socket-128-bytes', 'é'*64, EPOCH), ('empty-epoch', 'test-socket', ''),
                                     ('paired-surrogate', 'test-\U0001f600', EPOCH)]:
            obj = copy.deepcopy(seed['expected']['decoded'])
            obj.pop('proof')
            obj['target'].update(socketId=socket, credentialEpoch=epoch)
            fields = seed['signing']['orderedFields'].copy()
            fields[-3:-1] = [socket, epoch]
            self.add(label, obj, seed['signing']['domain'], fields, exact=True)

    def sizes_and_windows(self):
        # Whitespace affects HTTP body commitment, but remains valid JSON.
        reg = self.reg
        rb = bytes.fromhex(reg['rawHex'])
        for size in [4095, 4096, 4097]:
            padded = rb+b' '*(size-len(rb))
            v = self.case('body-size-'+str(size), reg, rawHex=padded.hex(), rawSHA256=digest(padded))
            fields = v['signing']['orderedFields'].copy()
            fields[4] = digest(padded)
            v['signing'] = crypto(v['signing']['domain'], NOW, fields)
            v['headers'][2][1] = v['signing']['expectedMacHex']
            if size > 4096:
                v['expected'] = rejected('raw-size')
        for window, base, ages, limits in [('exact', self.seed, [-2, -1, 0, 6, 7], (-1, 6)),
                                           ('generic', reg, [-31, -30, 0, 30, 31], (-30, 30))]:
            for age in ages:
                v = self.case(f'{window}-age-{age}', base, verifierUnixSeconds=str(int(NOW)+age))
                valid = limits[0] <= age <= limits[1]
                v['expected'].update(freshness=valid, accept=valid)

    def wrong_proofs(self):
        # A valid old signature must not authenticate changed supplied fields.
        for label, index, new, where in PROOF_EDITS:
            v = self.case('proof-wrong-'+label)
            signed = v['signing']
            provided = signed['expectedMacHex']
            decoded = copy.deepcopy(v['expected']['decoded'])
            if index is None:
                # Transmit the foreign-domain MAC; verifier domain comes from schema.
                provided = crypto(new, NOW, signed['orderedFields'])['expectedMacHex']
                decoded['proof'] = provided
            else:
                signed['orderedFields'][index] = new
                parent, key = where
                (decoded[parent] if parent else decoded)[key] = new
            data = raw(decoded)
            v.update(rawHex=data.hex(), rawSHA256=digest(data))
            v['signing'] = crypto(signed['domain'], NOW, signed['orderedFields'])
            v['providedMacHex'] = provided
            v['expected'].update(macMatches=False, binding=None, accept=False)
            v['expected']['decoded'] = decoded
        for label, index, value in [('request-timestamp', 2, str(int(NOW)-1)),
                                    ('request-nonce', 3, 'cd'*32), ('request-body-digest', 4, '00'*32)]:
            v = self.case('response-wrong-'+label, self.response)
            wrong_fields = v['signing']['orderedFields'].copy()
            wrong_fields[index] = value
            provided = crypto(v['signing']['domain'], NOW, wrong_fields)['expectedMacHex']
            v['headers'][2][1] = provided
            v['providedMacHex'] = provided
            v['expected'].update(macMatches=False, binding=None, accept=False)

    def binding_mismatches(self):
        # MAC is valid, but the epoch cross-field invariant is not.
        bad_epoch = {**self.ep, 'supersededCredentialEpoch': 'fedcba9876543210'*2}
        fields = self.ef.copy()
        fields[5] = bad_epoch['supersededCredentialEpoch']
        v = self.add('credential-eject-superseded-target-mismatch', bad_epoch,
                     EJECT_REQUEST_DOMAIN, fields, exact=True)
        v['signatureValidIfEvaluated'] = True
        v['expected'].update(decode='reject', classification='epoch-target-mismatch',
                             macMatches=None, freshness=None, binding=None, accept=False)
        # A valid ACK for another operation must not acknowledge this request.
        ack = self.find('eject-session-logout-ack')
        obj = copy.deepcopy(ack['expected']['decoded'])
        obj.pop('proof')
        obj['operationId'] = uuid(40)
        fields = ack['signing']['orderedFields'].copy()
        fields[2] = obj['operationId']
        v = self.add('valid-proof-ack-request-mismatch', obj, AC+'eject/ack/v1', fields,
                     exact=True, response=True, request=self.seed)
        v['expected'].update(binding=False, accept=False, classification='ack-request-mismatch')
        for label, key in [('boot', 'nodeBootId'), ('generation', 'connectionGeneration')]:
            v = self.case('valid-proof-wrong-owned-'+label)
            v['expectedOwnedTarget'] = {**TARGET, key: uuid(30)}
            v['expected'].update(binding=False, accept=False)

    def http_framing(self):
        reg = self.reg
        for label, mutate in [('duplicate-timestamp', lambda h: h+[h[0]]),
                              ('comma-nonce', lambda h: [h[0], [h[1][0], NONCE+','+NONCE], *h[2:]]),
                              ('missing-proof', lambda h: h[:2]+h[3:])]:
            v = self.case('headers-'+label, reg, headers=mutate(copy.deepcopy(reg['headers'])))
            v['expected'].update(accept=False, classification='header-cardinality', macMatches=None)
        for label, path in [('query', CP+'?x=1'), ('slash', CP+'/'), ('encoded-alias', CP.replace('auth', '%61uth'))]:
            v = self.case('path-'+label, reg, path=path)
            v['expected'].update(accept=False, classification='fixed-path', macMatches=None)
        # Preserve raw request bytes while changing one transmitted byte: stale proof.
        v = self.case('http-body-whitespace-with-old-proof', reg)
        data = bytes.fromhex(reg['rawHex'])+b' '
        fields = v['signing']['orderedFields'].copy()
        fields[4] = digest(data)
        v['providedMacHex'] = v['signing']['expectedMacHex']
        v['signing'] = crypto(v['signing']['domain'], NOW, fields)
        v.update(rawHex=data.hex(), rawSHA256=digest(data))
        v['expected'].update(macMatches=False, accept=False)

    def provisional_conflicts(self):
        pb, pp = self.pb, self.pp
        for label, key in [('original-token-digest', 'registrationTokenDigest'),
                           ('original-body-digest', 'registrationBodyDigest')]:
            obj = {**pb, key: '00'*32}
            fields = [CS, '1', 'POST', pp, digest(raw(obj)), TARGET['nodeBootId'], NONCE]
            v = self.add('provisional-wrong-'+label, obj, AC+'provisional-close/request/v1', fields, path=pp)
            v['persistedOriginalCommitments'] = {k: pb[k] for k in ['registrationTokenDigest', 'registrationBodyDigest']}
            v['expected'].update(binding=False, accept=False, classification='original-commitment-conflict')
            v['expectedHTTPStatus'] = 409
        obj = {**pb, 'target': {**TARGET, 'socketId': 'other-test-socket'}}
        v = self.add('provisional-wrong-target', obj, AC+'provisional-close/request/v1',
                     [CS, '1', 'POST', pp, digest(raw(obj)), TARGET['nodeBootId'], NONCE], path=pp)
        v['persistedTarget'] = TARGET
        v['expected'].update(binding=False, accept=False, classification='target-conflict')
        v['expectedHTTPStatus'] = 409

    def late_cases(self):
        health = self.find('health-prepared-false-false')
        self.malformed('health-protocol-downgrade',
                       bytes.fromhex(health['rawHex']).replace(b'"protocol":4', b'"protocol":3'),
                       'closed-protocol', health)
        # Paired surrogate escape spelling is valid and signs the decoded scalar UTF-8.
        paired = self.find('paired-surrogate')
        data = bytes.fromhex(paired['rawHex']).replace('😀'.encode(), b'\\ud83d\\ude00')
        self.case('paired-surrogate-escape', paired, rawHex=data.hex(), rawSHA256=digest(data))
        response = self.response
        v = self.case('response-current-timestamp-distinct-from-request', response)
        v['signing'] = crypto(v['signing']['domain'], str(int(NOW)+1), v['signing']['orderedFields'])
        v['headers'][0][1] = v['signing']['timestamp']
        v['headers'][2][1] = v['signing']['expectedMacHex']
        v = self.case('valid-proof-wrong-owned-family')
        v['expectedOwnedBinding'] = {**BINDING, 'familyId': uuid(31)}
        v['expected'].update(binding=False, accept=False)
        # Signed but wrong HTTP status/response freshness cannot clear a liability.
        v = self.case('response-wrong-status', response, status=201)
        v['expected'].update(accept=False, classification='exact-status')
        v = self.case('response-stale', response, verifierUnixSeconds=str(int(NOW)+31))
        v['expected'].update(accept=False, freshness=False)
        for disposition in ['unregistered', 'retired_boot']:
            base = self.find(f'provisional-close-{disposition}-response')
            data = bytes.fromhex(base['rawHex'])[:-1]+b',"binding":'+raw(BINDING)+b'}'
            self.malformed('provisional-'+disposition+'-forbidden-binding', data, 'closed-variant', base)


def build():
    d = Dossier()
    for section in [d.connection, d.ejects, d.health, d.rooms, d.provisional_and_boot, d.raw_wire,
                    d.sizes_and_windows, d.wrong_proofs, d.binding_mismatches, d.http_framing,
                    d.provisional_conflicts, d.late_cases]:
        section()
    return {'schema': 'concord.auth-connection-protocol-vectors', 'version': 1,
            'provenance': {'kind': 'new-independent-contract-vectors', 'sourceBase': '269f0da9690541be842801676e1f292b4c665a27',
                           'protocolSHA256': 'c72459a79641ebb7b5ddd9705f1c6ccb46631a2fb75815a87706dfd5a322588a',
                           'authoritySHA256': 'a9c9c58299c090a11dfab8a45718d9bb42475b838072bdf846e00e36a181f6f7',
                           'cryptoImplementations': ['Python hashlib/hmac', 'Node node:crypto'],
                           'provisionalResponseSchemaRuling': 'Root explicitly confirmed 2026-10-26: response signs concord.auth-connection-provisional-close-result; request signs concord.auth-connection-provisional-close'},
            'testOnlySecretHex': SECRET.hex(), 'testOnlyBearerUTF8Hex': TOKEN.encode().hex(),
            'classificationsAreWireReasonStrings': False, 'records': d.records}


NODE = r'''
const fs = require('node:fs'); const crypto = require('node:crypto');
const f = JSON.parse(fs.readFileSync(0, 'utf8'));
const hash = b => crypto.createHash('sha256').update(b).digest('hex');
const mac = (k,b) => crypto.createHmac('sha256',k).update(b).digest();
const byId = new Map(f.records.map(r => [r.id,r]));
const AC='concord/auth-connection/', AR='concord/auth-room-binding/';
const CP='/api/v1/internal/voice/auth-connections';
const RP='/api/v1/voice/enforcement-sessions';
const RR='/api/v1/internal/voice/enforcement-sessions/release';
const HP='/api/v1/internal/voice/enforcement/health';
const text = bytes => new TextDecoder('utf-8',{fatal:true}).decode(bytes);
const decode = r => r.rawHex ? JSON.parse(text(Buffer.from(r.rawHex,'hex'))) : null;
const header = (r,suffix) => {
 const values=r.headers.filter(([n]) => n.toLowerCase().endsWith(suffix.toLowerCase()));
 if(values.length!==1) throw Error(r.id+': wire header cardinality '+suffix);
 return values[0][1];
};
const target = t => [t.connectionGeneration,t.nodeBootId,t.socketId,t.credentialEpoch];
const binding = b => [b.userId,b.familyId,...target(b.target)];
const room = b => [b.sessionGeneration,b.roomId,b.roomKind];
const equal = (a,b) => JSON.stringify(a)===JSON.stringify(b);
function fromWire(r) {
 const b=decode(r), response=r.direction==='response';
 let domain,fields,timestamp,provided;
 if(r.transport==='nats') {
  timestamp=b.timestamp; provided=b.proof;
  if(b.schema==='concord.auth-revocation') {
   domain=AC+'eject/'+(response?'ack':'request')+'/v1';
   fields=[b.schema,String(b.version),b.operationId,b.scope,b.reason,...binding(b),b.nonce];
   if(response) fields.push(String(b.ok));
  } else if(b.schema==='concord.credential-connection-revocation') {
   domain=AC+'credential-eject/'+(response?'ack':'request')+'/v1';
   fields=[b.schema,String(b.version),b.parentGeneration,b.userId,b.familyId,
           b.supersededCredentialEpoch,...target(b.target),b.nonce];
   if(response) fields.push(String(b.ok));
  } else if(b.schema==='concord.voice-enforcement-health') {
   domain=AC+'health/'+(response?'ack':'request')+'/v1';
   fields=[b.schema,String(b.version),String(b.protocol),b.nodeBootId,b.challenge,
           b.activation,String(b.admissionAllowed)];
   if(response) fields.push(String(b.transportOK),String(b.admissionGranted));
  } else throw Error(r.id+': unexpected wire schema');
 } else {
  timestamp=header(r,'-Timestamp'); provided=header(r,'-Proof');
  const source=response?byId.get(r.requestId):r;
  if(!source) throw Error(r.id+': missing referenced request');
  const request=decode(source), p=source.path;
  const requestTime=header(source,'-Timestamp'), nonce=header(source,'-Nonce');
  const bodyHash=hash(Buffer.from(source.rawHex,'hex'));
  const boot=request.nodeBootId || request.target?.nodeBootId || request.binding?.target.nodeBootId;
  const token=()=>hash(Buffer.from(header(source,'authorization').slice('Bearer '.length),'utf8'));
  let suffix, root=AC, schema;
  if(p===CP) suffix='register';
  else if(p===CP+'/release') suffix='release';
  else if(p===RP) {suffix='register';root=AR;schema=request.schema;}
  else if(p===RR) {suffix='release';root=AR;schema=request.schema;}
  else if(p===CP+'/provisional-close') {suffix='provisional-close';schema=response?b.schema:request.schema;}
  else if(p===CP+'/retire-boot') {suffix='retire-boot';schema=request.schema;}
  else if(p===HP) suffix='health/bootstrap';
  else throw Error(r.id+': unexpected fixed wire path');
  domain=root+suffix+'/'+(response?'response':'request')+'/v1';
  const prefix=schema?[schema,'1']:['1'];
  if(!response) {
   fields=[...prefix,source.method,p];
   if(suffix==='register') fields.push(token());
   fields.push(bodyHash,boot,nonce);
  } else {
   fields=[...prefix,p,requestTime,nonce,bodyHash];
   if(suffix==='register' && root===AC) fields.push('4',...binding(b));
   else if(suffix==='release' && root===AC) fields.push('204',...binding(request));
   else if(suffix==='register' && root===AR) fields.push('200','4',...binding(b.binding),...room(b.room));
   else if(suffix==='release' && root===AR) fields.push('204',...binding(request.binding),...room(request.room));
   else if(suffix==='retire-boot') fields.push('204',boot);
   else fields.push('200',hash(Buffer.from(r.rawHex,'hex')));
   if(header(r,'-Nonce')!==nonce) throw Error(r.id+': response header nonce does not echo request');
  }
 }
 return {domain,fields,timestamp,provided};
}

let count = 0, wireCount = 0;
for (const r of f.records) {
 const s=r.signing, body=Buffer.from(r.rawHex,'hex');
 const payload=Buffer.from(['v1',s.timestamp,...s.orderedFields].join('\n'),'utf8');
 const key=mac(Buffer.from(f.testOnlySecretHex,'hex'),Buffer.from(s.domain,'utf8'));
 if(r.expected.accept && r.transport==='http' && r.direction==='request' && !s.orderedFields.includes(hash(body))) throw Error(r.id+': body digest missing');
 for (const [name,a,b] of [['raw SHA256',hash(body),r.rawSHA256],
   ['signing bytes',payload.toString('hex'),s.signingBytesHex],
   ['derived key',key.toString('hex'),s.derivedKeyHex],
   ['HMAC',mac(key,payload).toString('hex'),s.expectedMacHex]]) {
   if(a!==b) throw Error(r.id+': '+name+' mismatch');
 }
 if(r.authorizationTokenUTF8Hex && hash(Buffer.from(r.authorizationTokenUTF8Hex,'hex'))!==s.orderedFields[s.domain.startsWith('concord/auth-room')?4:3]) throw Error(r.id+': token digest');
 if(r.providedMacHex && r.providedMacHex===s.expectedMacHex) throw Error(r.id+': expected MAC mismatch absent');
 if(r.expected.macMatches!==null || r.signatureValidIfEvaluated) {
  const w=fromWire(r);
  if(w.domain!==s.domain || w.timestamp!==s.timestamp || !equal(w.fields,s.orderedFields))
   throw Error(r.id+': reconstructed wire signing context mismatch');
  if(w.provided!==(r.providedMacHex || s.expectedMacHex))
   throw Error(r.id+': transmitted proof mismatch');
  const wireKey=mac(Buffer.from(f.testOnlySecretHex,'hex'),Buffer.from(w.domain,'utf8'));
  const wireMac=mac(wireKey,Buffer.from(['v1',w.timestamp,...w.fields].join('\n'),'utf8')).toString('hex');
  const actualMatch=wireMac===w.provided;
  if(actualMatch!==(r.signatureValidIfEvaluated || r.expected.macMatches))
   throw Error(r.id+': wire MAC outcome mismatch');
  if(r.transport==='nats' && r.direction==='response') {
   const ack=decode(r), req=decode(byId.get(r.requestId));
   for(const k of ['proof','ok','transportOK','admissionGranted']) delete ack[k];
   delete req.proof;
   const same=equal(ack,req);
   if(same!==r.expected.binding) throw Error(r.id+': ACK request binding outcome mismatch');
  }
  if(r.signatureValidIfEvaluated) {
   const b=decode(r);
   if(b.supersededCredentialEpoch===b.target.credentialEpoch) throw Error(r.id+': epoch rejection lacks mismatch');
  }
  wireCount++;
 }
 count++;
}
console.log(`Node crypto independently verified ${count} raw hashes, signing payloads, keys and MACs; reconstructed ${wireCount} wire proof contexts`);
'''

def main():
    fixture=build()
    rendered=json.dumps(fixture,ensure_ascii=True,indent=2)+'\n'
    if sys.argv[1:]==['--write']:
        FIXTURE.write_text(rendered)
    elif sys.argv[1:]:
        raise SystemExit('usage: auth_connection_protocol_vectors.py [--write]')
    else:
        assert FIXTURE.read_text()==rendered, 'fixture differs from independently reproducible contract dossier'
    subprocess.run(['node','-e',NODE],input=rendered,text=True,check=True)
    for r in fixture['records']:
        if r['expected']['decode']=='accept' and r['expected'].get('decoded') is not None:
            body=bytes.fromhex(r['rawHex'])
            if body:
                assert json.loads(body)==r['expected']['decoded'], r['id']+': decoded fixture disagreement'
    ids=[r['id'] for r in fixture['records']]
    assert len(set(ids))==len(ids)
    print(f'Python verified {len(ids)} records, {len({r["signing"]["domain"] for r in fixture["records"]})} distinct domains; fixture matches')

if __name__=='__main__':
    main()
