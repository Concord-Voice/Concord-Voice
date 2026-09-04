/**
 * #3104 D6 — the PiP RPC channel authenticates its peer.
 *
 * Before this, `PipSignalingProxy.handleMessage` checked the message KIND and
 * nothing else: no pipId registry, no `pip-ready` precondition, no per-window
 * capability. `respond()` posted every reply on the shared `concord-pip`
 * BroadcastChannel, which reaches every same-origin document. Three exploits
 * were demonstrated against the real proxy and are inverted here — each test
 * passes by proving the attack is REPELLED:
 *
 *   E1  passive — a silent listener received live TURN credentials as a side
 *       effect of a legitimate PiP opening.
 *   E2  active  — a document that never registered as a PiP could DEMAND them,
 *       with no call of its own.
 *   E2b active  — the same unauthenticated channel drove `leaveChannel()`.
 *       This half predates #3104.
 *
 * The fix: the main process mints a per-window token (`pip:open`), discloses it
 * to the main window (`pip:opened`) and to that PiP's own main frame
 * (`pip:session`), and BOTH directions of the RPC protocol move to a private
 * BroadcastChannel named from it. Nothing is ever posted on `concord-pip`, so
 * the token is never observable on a shared channel — which is why it also
 * cannot ride in the request envelope.
 *
 * The harness is `FanoutBroadcastChannel`, which implements real WHATWG
 * delivery. `MockBroadcastChannel` structurally cannot express these
 * assertions: it never delivers to a third document.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { FanoutBroadcastChannel } from '../../helpers/fanoutBroadcastChannel';
import { resetAllStores } from '../../helpers/store-helpers';

// Marker strings, deliberately NOT shaped like a real credential. What these
// assertions test is reachability — did this string arrive at a document that
// holds no capability — and the shape contributes nothing to that. A realistic
// base64-of-HMAC literal here only trips the public-mirror gitleaks gate. The
// one place the shape IS load-bearing is the capture-time scrub suite in
// `logBufferService.test.ts`, whose fixture is minted the way `turn.go` mints it.
const TURN_USERNAME = 'sentinel-turn-username-1788000000';
const TURN_CREDENTIAL = 'sentinel-turn-credential-do-not-match';

const LIVE_ICE_SERVERS: RTCIceServer[] = [
  { urls: 'stun:turn.concordvoice.chat:3478' },
  {
    urls: 'turn:turn.concordvoice.chat:3478?transport=udp',
    username: TURN_USERNAME,
    credential: TURN_CREDENTIAL,
  },
];

/**
 * Session capabilities. `mintPipSessionToken` produces base64url of 32 bytes;
 * these are markers instead, for the same reason as the two above. Unguessability
 * is asserted at the mint site (`tests/unit/main/pipSession.test.ts`), not here —
 * here the only property that matters is that the two values differ.
 */
const SESSION_TOKEN = 'sentinel-pip-session-token-a';
const OTHER_TOKEN = 'sentinel-pip-session-token-b';

const mockVoiceService = {
  forwardToServer: vi.fn(),
  getRouterRtpCapabilities: vi.fn().mockReturnValue({ codecs: [] }),
  getConsumerIdsBySource: vi.fn().mockReturnValue([]),
  getConsumerMeta: vi.fn().mockReturnValue(new Map()),
  pauseConsumer: vi.fn(),
  resumeConsumer: vi.fn(),
  emitPreferredLayersForConsumer: vi.fn(),
  deriveFrameKeyForPip: vi.fn(),
  getIceServersForPip: vi.fn().mockReturnValue(LIVE_ICE_SERVERS),
  toggleMute: vi.fn().mockResolvedValue(undefined),
  toggleDeafen: vi.fn(),
  toggleVideo: vi.fn().mockResolvedValue(undefined),
  toggleScreenShare: vi.fn().mockResolvedValue(undefined),
  leaveChannel: vi.fn().mockResolvedValue(undefined),
};

vi.mock('mediasoup-client/types', () => ({}));

import { PipSignalingProxy } from '@/renderer/services/voice/pipSignalingProxy';
import {
  PIP_CHANNEL_PREFIX,
  pipSessionChannelName,
} from '@/renderer/services/voice/pipSignalingTypes';
import { useUserStore } from '@/renderer/stores/auth/userStore';

/** Every credential-shaped string anywhere in a payload, however nested. */
function credentialHits(payloads: unknown[]): string[] {
  const blob = JSON.stringify(payloads ?? null) ?? '';
  const hits: string[] = [];
  if (blob.includes(TURN_USERNAME)) hits.push(TURN_USERNAME);
  if (blob.includes(TURN_CREDENTIAL)) hits.push(TURN_CREDENTIAL);
  return hits;
}

/** Any session token leaked into a payload is a total break — the token IS the capability. */
function tokenHits(payloads: unknown[]): string[] {
  const blob = JSON.stringify(payloads ?? null) ?? '';
  return [SESSION_TOKEN, OTHER_TOKEN].filter((t) => blob.includes(t));
}

async function settle(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

function rpc(method: string, params: unknown = {}, id = 'req-1', pipId = 'pip-a') {
  return { kind: 'rpc-request', id, pipId, method, params };
}

describe('PiP channel authentication (#3104 D6)', () => {
  let proxy: PipSignalingProxy;

  beforeEach(() => {
    FanoutBroadcastChannel.install();
    resetAllStores();
    vi.clearAllMocks();
    mockVoiceService.forwardToServer.mockReset().mockResolvedValue({
      id: 'server-transport-1',
      iceParameters: { usernameFragment: 'x', password: 'y' },
      iceCandidates: [],
      dtlsParameters: { fingerprints: [] },
    });
    mockVoiceService.getIceServersForPip.mockReset().mockReturnValue(LIVE_ICE_SERVERS);
    mockVoiceService.leaveChannel.mockReset().mockResolvedValue(undefined);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    useUserStore.setState({ user: { id: 'victim-user' } as any });
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    proxy = new PipSignalingProxy(mockVoiceService as any);
    proxy.registerSession('pip-a', SESSION_TOKEN);
  });

  afterEach(() => {
    proxy.dispose();
    FanoutBroadcastChannel.uninstall();
  });

  // ── E1 (passive) ────────────────────────────────────────────────────

  it('E1 — a silent listener on concord-pip receives nothing when a real PiP opens', async () => {
    const eavesdropper = new FanoutBroadcastChannel(PIP_CHANNEL_PREFIX);
    const realPip = new FanoutBroadcastChannel(pipSessionChannelName(SESSION_TOKEN));

    realPip.postMessage(rpc('create-recv-transport'));
    await settle();

    // Positive control FIRST: the legitimate PiP really did get the credentials,
    // so an empty eavesdropper log is repulsion and not a broken fixture.
    expect(credentialHits(realPip.received)).toEqual([TURN_USERNAME, TURN_CREDENTIAL]);

    expect(eavesdropper.received).toEqual([]);
    expect(credentialHits(eavesdropper.received)).toEqual([]);
  });

  it('E1 — a listener on a GUESSED session channel receives nothing', async () => {
    const guesser = new FanoutBroadcastChannel(pipSessionChannelName(OTHER_TOKEN));
    const realPip = new FanoutBroadcastChannel(pipSessionChannelName(SESSION_TOKEN));

    realPip.postMessage(rpc('create-recv-transport'));
    await settle();

    expect(guesser.received).toEqual([]);
  });

  it('E1 — store-driven broadcasts never reach concord-pip either', async () => {
    const eavesdropper = new FanoutBroadcastChannel(PIP_CHANNEL_PREFIX);
    proxy.broadcastProducerAdded('producer-1', 'user-1', 'mic');
    await settle();
    expect(eavesdropper.received).toEqual([]);
  });

  // ── E2 (active) ─────────────────────────────────────────────────────

  it('E2 — an unregistered document cannot demand the credentials on concord-pip', async () => {
    const attacker = new FanoutBroadcastChannel(PIP_CHANNEL_PREFIX);

    attacker.postMessage(rpc('create-recv-transport', {}, 'attacker-1', 'not-a-real-pip'));
    await settle();

    expect(attacker.received).toEqual([]);
    expect(credentialHits(attacker.received)).toEqual([]);
    // Nothing reached the media plane either — the RPC was refused, not answered
    // from a cache.
    expect(mockVoiceService.forwardToServer).not.toHaveBeenCalled();
  });

  it('E2 — an unregistered document cannot demand them on a guessed session channel', async () => {
    const attacker = new FanoutBroadcastChannel(pipSessionChannelName(OTHER_TOKEN));

    attacker.postMessage(rpc('create-recv-transport', {}, 'attacker-2', 'pip-a'));
    await settle();

    expect(attacker.received).toEqual([]);
    expect(mockVoiceService.forwardToServer).not.toHaveBeenCalled();
  });

  // ── E2b (privileged action) ─────────────────────────────────────────

  it('E2b — the unauthenticated channel can no longer drive leaveChannel()', async () => {
    const attacker = new FanoutBroadcastChannel(PIP_CHANNEL_PREFIX);
    attacker.postMessage(rpc('action', { action: 'leave' }, 'attacker-3', 'not-a-real-pip'));
    await settle();
    expect(mockVoiceService.leaveChannel).not.toHaveBeenCalled();
  });

  it('E2b — nor can a document on a guessed session channel', async () => {
    const attacker = new FanoutBroadcastChannel(pipSessionChannelName(OTHER_TOKEN));
    attacker.postMessage(rpc('action', { action: 'leave' }, 'attacker-4', 'pip-a'));
    await settle();
    expect(mockVoiceService.leaveChannel).not.toHaveBeenCalled();
  });

  it('E2b — an authenticated PiP still CAN leave (the guard is not a blanket refusal)', async () => {
    const realPip = new FanoutBroadcastChannel(pipSessionChannelName(SESSION_TOKEN));
    realPip.postMessage(rpc('action', { action: 'leave' }, 'real-1'));
    await settle();
    expect(mockVoiceService.leaveChannel).toHaveBeenCalledTimes(1);
  });

  // ── The token itself is a secret on this path ───────────────────────

  it('never posts a session token on any channel', async () => {
    const eavesdropper = new FanoutBroadcastChannel(PIP_CHANNEL_PREFIX);
    const realPip = new FanoutBroadcastChannel(pipSessionChannelName(SESSION_TOKEN));

    realPip.postMessage(rpc('create-recv-transport'));
    proxy.broadcastProducerClosed('producer-1', 'user-1');
    await settle();

    expect(tokenHits(realPip.received)).toEqual([]);
    expect(tokenHits(eavesdropper.received)).toEqual([]);
  });

  it('opens no channel named plainly concord-pip', () => {
    expect(FanoutBroadcastChannel.named(PIP_CHANNEL_PREFIX)).toHaveLength(0);
  });

  // ── Revocation ──────────────────────────────────────────────────────

  it('revokes the capability when the PiP window closes', async () => {
    const pip = new FanoutBroadcastChannel(pipSessionChannelName(SESSION_TOKEN));

    proxy.onPipClosed('pip-a');
    pip.postMessage(rpc('action', { action: 'leave' }, 'after-close'));
    await settle();

    expect(mockVoiceService.leaveChannel).not.toHaveBeenCalled();
    expect(pip.received).toEqual([]);
  });

  it('binds the pipId to the session rather than trusting the envelope', async () => {
    proxy.registerSession('pip-b', OTHER_TOKEN);
    mockVoiceService.getConsumerMeta.mockReturnValue(
      new Map([['consumer-1', { source: 'mic', producerUserId: 'user-1', producerId: 'p1' }]])
    );

    // pip-a claims to be pip-b so that closing pip-b would resume ITS consumers.
    const pipA = new FanoutBroadcastChannel(pipSessionChannelName(SESSION_TOKEN));
    pipA.postMessage(
      rpc(
        'pip-ready',
        { consumerSources: [{ source: 'mic', producerUserId: 'user-1' }] },
        'r1',
        'pip-b'
      )
    );
    await settle();
    expect(mockVoiceService.pauseConsumer).toHaveBeenCalledWith('consumer-1');

    // Closing the window whose id was CLAIMED must not resume them...
    mockVoiceService.resumeConsumer.mockClear();
    proxy.onPipClosed('pip-b');
    expect(mockVoiceService.resumeConsumer).not.toHaveBeenCalled();

    // ...closing the window that actually asked does.
    proxy.onPipClosed('pip-a');
    expect(mockVoiceService.resumeConsumer).toHaveBeenCalledWith('consumer-1');
  });
});
