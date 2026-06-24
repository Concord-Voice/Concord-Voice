/**
 * Media E2EE — Insertable Streams frame encryption for voice/video.
 *
 * All media frames are encrypted with AES-256-GCM before being sent to the
 * SFU. The SFU forwards encrypted RTP payloads transparently — it never sees
 * plaintext audio/video.
 *
 * Key derivation:
 *   frameKey = HKDF-SHA256(channelCSK, salt="concord-voice-e2ee", info=senderUserId)
 *
 * Frame format (encrypted, v3 — #1878):
 *   [unencrypted header (1–10 bytes)][AES-GCM ciphertext + 16-byte auth tag][1-byte headerBytes][2-byte keyId BE][4-byte keyVersion BE][12-byte IV][0xDE 0xAD magic]
 *
 * Trailer (21 bytes total): headerBytes + keyId + keyVersion + IV + magic.
 * The headerBytes field tells the receiver how many leading bytes were left
 * unencrypted, so the split point is self-describing and robust against
 * frame misrouting between consumers in a BUNDLE group. The keyVersion field
 * carries the channel-key (CSK) version so receivers derive the exact key a
 * frame needs across mid-session CSK rotations — no lockstep epoch counter.
 *
 * AES-GCM is used WITHOUT additionalData (no AAD). This is intentional:
 * Chromium's SDP BUNDLE can produce payload_type collisions that misroute
 * frames between audio/video consumers. With AAD tied to headerBytes, a
 * misrouted frame would always fail authentication. Without AAD the frame
 * can still decrypt correctly using the sender-encoded headerBytes field.
 *
 * The 2-byte magic trailer (0xDE 0xAD) allows the receiver to distinguish
 * encrypted frames from raw codec frames. Sender transform attachment failures
 * fail closed before publishing unencrypted media.
 *
 * Epoch-based key rotation (#96):
 *   On member join/leave → new epoch → new keys derived
 *   Receivers keep old keys for 10 seconds (overlap window)
 *   Ratchet: newKey = HKDF(oldKey, "concord-e2ee-ratchet")
 *
 * Dual API support:
 *   - Modern (Chromium 129+): RTCRtpScriptTransform with Web Worker
 *   - Legacy (Chromium 86-130): createEncodedStreams on main thread
 *
 * The Worker instantiates this class directly. The main thread derives keys
 * and sends them to the Worker via postMessage (CryptoKey is structured-clonable).
 */

const SALT = new TextEncoder().encode('concord-voice-e2ee');
const RATCHET_INFO = new TextEncoder().encode('concord-e2ee-ratchet');

// Magic trailer bytes appended to every encrypted frame so the receiver can
// distinguish encrypted frames from unencrypted ones.
const MAGIC_0 = 0xde;
const MAGIC_1 = 0xad;
// Media-frame crypto format advertised to the SFU at join time. v1 was the
// legacy AES-128-GCM frame key; v2 was AES-256-GCM keyed only by ratchet keyId;
// v3 additionally stamps the channel-key version so decryption is deterministic
// across mid-session CSK rotations (#1878).
export const MEDIA_E2EE_FRAME_CRYPTO_VERSION = 3;
// v3 trailer: 1B headerBytes + 2B keyId(BE) + 4B keyVersion(BE) + 12B IV + 2B magic = 21
const TRAILER_SIZE_V3 = 21;
// Minimum encrypted frame: 1 header byte + 16-byte GCM tag (empty plaintext) + trailer.
const MIN_GCM_OVERHEAD = 17;

// Reference: ideal unencrypted header bytes per codec (not used directly —
// getUnencryptedBytes() uses static 1/2 for sender/receiver agreement safety).
// VP8 keyframe ideally needs 10 bytes, delta 3 bytes (RFC 6386 §9.1),
// but static 2 bytes is sufficient for the keyframe flag + partition bits.
export const UNENCRYPTED_BYTES: Record<string, number> = {
  opus: 1,
  vp8: 2,
  vp9: 1,
  h264: 2,
  h265: 2,
  av1: 1,
};

/**
 * Determine how many leading bytes of the frame payload must be left
 * unencrypted. The unencrypted prefix preserves codec framing so the
 * decoder can identify keyframes, Opus modes, etc.
 *
 * The sender encodes this value in the frame trailer (1-byte headerBytes
 * field), so the receiver reads it directly instead of recomputing it.
 * This makes decryption robust against BUNDLE frame misrouting where
 * an audio frame may arrive at a video consumer (or vice versa).
 *
 * Using 1 byte for audio (Opus TOC) and 2 bytes for video (covers VP8
 * partition flag + VP9 frame header) is the conservative minimum.
 */
function getUnencryptedBytes(frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame): number {
  const isVideo = 'type' in frame;
  return isVideo ? 2 : 1;
}

/** Derive an AES-256-GCM key from a channel CSK + sender userId via HKDF */
export async function deriveFrameKey(
  channelCSK: CryptoKey,
  senderUserId: string
): Promise<CryptoKey> {
  // Export the CSK to raw bytes for use as HKDF input key material
  const cskBytes = await crypto.subtle.exportKey('raw', channelCSK);

  // Import as HKDF key
  const hkdfKey = await crypto.subtle.importKey('raw', cskBytes, 'HKDF', false, ['deriveKey']);

  // Derive AES-256-GCM frame key (extractable: true so it can be ratcheted on epoch rotation)
  return crypto.subtle.deriveKey(
    {
      name: 'HKDF',
      hash: 'SHA-256',
      salt: SALT,
      info: new TextEncoder().encode(senderUserId),
    },
    hkdfKey,
    { name: 'AES-GCM', length: 256 },
    true,
    ['encrypt', 'decrypt']
  );
}

/** Ratchet a key forward (for epoch rotation) */
export async function ratchetKey(currentKey: CryptoKey): Promise<CryptoKey> {
  const keyBytes = await crypto.subtle.exportKey('raw', currentKey);
  const hkdfKey = await crypto.subtle.importKey('raw', keyBytes, 'HKDF', false, ['deriveKey']);
  return crypto.subtle.deriveKey(
    {
      name: 'HKDF',
      hash: 'SHA-256',
      salt: SALT,
      info: RATCHET_INFO,
    },
    hkdfKey,
    { name: 'AES-GCM', length: 256 },
    true,
    ['encrypt', 'decrypt']
  );
}

// ---------------------------------------------------------------------------
// FrameKeyMissError
// ---------------------------------------------------------------------------

/**
 * #1878: thrown when `decryptFrame` has no key for the frame's exact
 * (senderUserId, keyVersion, keyId) and cannot ratchet to it. Distinct from a
 * WebCrypto `OperationError` (GCM auth failure on a wrong-base key) so the
 * worker can route a typed miss to on-demand key provisioning (`requestFrameKey`)
 * while leaving the OperationError/persistent-failure self-heal path untouched.
 * The message still contains "no decrypt key" so existing regex assertions hold.
 */
export class FrameKeyMissError extends Error {
  override name = 'FrameKeyMissError';
  constructor(
    public senderUserId: string,
    public keyVersion: number,
    public keyId: number,
    message: string
  ) {
    super(message);
  }
}

// ---------------------------------------------------------------------------
// MediaEncryption class
// ---------------------------------------------------------------------------

export class MediaEncryption {
  private currentKeyId = 0;
  // #1878: the channel-key (CSK) version bound to the encrypt key. Stamped into
  // every outgoing frame's v3 trailer so receivers select the exact key.
  private currentKeyVersion = 0;
  private encryptKey: CryptoKey | null = null;
  private readonly decryptKeys: Map<string, { key: CryptoKey; expires: number }> = new Map();
  private rotationChain: Promise<void> = Promise.resolve();

  /**
   * Compose the decrypt-key map key. #1878: keyed by senderId:keyVersion:keyId
   * (was senderId:keyId — collided across CSK versions). Sender userIds are
   * UUIDs (no embedded colons), so the 3-part split is unambiguous.
   */
  private static mapKey(senderUserId: string, keyVersion: number, keyId: number): string {
    return `${senderUserId}:${keyVersion}:${keyId}`;
  }

  // Debounced key rotation — collapses rapid join/leave bursts into a single
  // rotation to avoid O(N²) HKDF cost when many users join simultaneously.
  private rotationTimer: ReturnType<typeof setTimeout> | null = null;
  private rotationPending = false;
  private rotationDeadline = 0; // Max-cap absolute deadline (0 = none)
  private static readonly DEBOUNCE_MS = 2000;
  private static readonly MAX_CAP_MS = 5000;

  /** Initialize encryption for a channel with the given CSK and local userId */
  async init(channelCSK: CryptoKey, localUserId: string): Promise<void> {
    this.encryptKey = await deriveFrameKey(channelCSK, localUserId);
    this.currentKeyId = 0;
  }

  /** Initialize encryption with a pre-derived key (used by Worker) */
  initFromKey(key: CryptoKey, keyId: number): void {
    this.encryptKey = key;
    this.currentKeyId = keyId;
  }

  /** Update the current epoch ID without changing the encrypt key (main-thread sync) */
  setCurrentKeyId(keyId: number): void {
    this.currentKeyId = keyId;
  }

  /** Bind the encrypt key's channel-key version (the authoritative CSK version). */
  setKeyVersion(keyVersion: number): void {
    this.currentKeyVersion = keyVersion;
  }

  getKeyVersion(): number {
    return this.currentKeyVersion;
  }

  /**
   * Legacy 2-arg worker-path adder — defaults keyVersion to the encrypt
   * version. Thin wrapper over addDecryptKeyDirectV3; remaining call sites
   * pass version explicitly in a later task (#1878 Task 5).
   */
  addDecryptKeyDirect(senderUserId: string, keyId: number, key: CryptoKey): void {
    this.addDecryptKeyDirectV3(senderUserId, this.currentKeyVersion, keyId, key);
  }

  /** Worker-path: add a pre-derived decrypt key at an explicit (version, keyId). */
  addDecryptKeyDirectV3(
    senderUserId: string,
    keyVersion: number,
    keyId: number,
    key: CryptoKey
  ): void {
    this.decryptKeys.set(MediaEncryption.mapKey(senderUserId, keyVersion, keyId), {
      key,
      expires: Date.now() + 3_600_000,
    });
  }

  /**
   * Legacy 2-arg adder — defaults keyVersion to the encrypt version. Thin
   * wrapper over addDecryptKeyAtVersion (#1878 Task 5 removes remaining
   * version-blind call sites).
   */
  async addDecryptKey(channelCSK: CryptoKey, senderUserId: string, keyId = 0): Promise<CryptoKey> {
    return this.addDecryptKeyAtVersion(channelCSK, senderUserId, this.currentKeyVersion, keyId);
  }

  /**
   * Legacy adder pre-ratcheted to a target epoch — defaults keyVersion to the
   * encrypt version. Thin wrapper over addDecryptKeyAtVersion.
   */
  async addDecryptKeyAtEpoch(
    channelCSK: CryptoKey,
    senderUserId: string,
    targetEpoch: number
  ): Promise<CryptoKey> {
    return this.addDecryptKeyAtVersion(
      channelCSK,
      senderUserId,
      this.currentKeyVersion,
      targetEpoch
    );
  }

  /**
   * Main-path: derive + ratchet a decrypt key for an explicit (version, keyId).
   * Avoids the expensive per-frame self-healing ratchet when a decrypt key is
   * added mid-session (e.g., during tuneIn or late new-producer events).
   */
  async addDecryptKeyAtVersion(
    channelCSK: CryptoKey,
    senderUserId: string,
    keyVersion: number,
    keyId = 0
  ): Promise<CryptoKey> {
    if (keyId > 100) {
      throw new Error('E2EE epoch gap too large (' + keyId + '), rejoin required');
    }
    let key = await deriveFrameKey(channelCSK, senderUserId);
    for (let i = 0; i < keyId; i++) {
      key = await ratchetKey(key);
    }
    this.decryptKeys.set(MediaEncryption.mapKey(senderUserId, keyVersion, keyId), {
      key,
      expires: Date.now() + 3_600_000,
    });
    return key;
  }

  /** Rotate keys (new epoch) — ratchet forward, keep old keys for overlap */
  async rotateKeys(): Promise<void> {
    const run = this.rotationChain.then(
      () => this.rotateKeysOnce(),
      () => this.rotateKeysOnce()
    );
    this.rotationChain = run.catch(() => {});
    return run;
  }

  private async rotateKeysOnce(): Promise<void> {
    if (!this.encryptKey) return;

    const oldKeyId = this.currentKeyId;
    const newKeyId = oldKeyId + 1;
    const currentEncryptKey = this.encryptKey;
    const newEncryptKey = await ratchetKey(currentEncryptKey);
    if (this.encryptKey !== currentEncryptKey || this.currentKeyId !== oldKeyId) return;
    this.currentKeyId = newKeyId;
    this.encryptKey = newEncryptKey;

    // Ratchet ALL decrypt keys at oldKeyId → newKeyId so receivers stay in
    // sync with senders.  Without this, decrypt keys stay at keyId 0 while
    // senders advance on every join/leave, causing "No decrypt key" errors
    // once the gap exceeds the self-healing ratchet limit.
    // #1878: the map key is now senderId:keyVersion:keyId (3-part). Match the
    // keyId field by parsing, not endsWith(':oldKeyId') — and re-insert at the
    // SAME keyVersion so each version's ratchet chain stays independent.
    const toRatchet: {
      senderUserId: string;
      keyVersion: number;
      entry: { key: CryptoKey; expires: number };
    }[] = [];
    for (const [id, entry] of this.decryptKeys) {
      const parts = id.split(':');
      if (parts.length !== 3) continue; // sender UUIDs have no colons
      const [senderUserId, ver, kid] = parts;
      if (Number(kid) === oldKeyId) {
        toRatchet.push({ senderUserId, keyVersion: Number(ver), entry });
      }
    }
    for (const { senderUserId, keyVersion, entry } of toRatchet) {
      const newKey = await ratchetKey(entry.key);
      this.decryptKeys.set(MediaEncryption.mapKey(senderUserId, keyVersion, newKeyId), {
        key: newKey,
        expires: Date.now() + 3_600_000,
      });
      // Keep the old keyId for a 10s overlap so in-flight frames from before the
      // ratchet still decrypt. This is a delivery-overlap window, NOT a forward-
      // secrecy boundary (intra-version FS is intentionally dropped — #1878
      // Decision B; FS is enforced by CSK key_version rotation on membership
      // change).
      entry.expires = Date.now() + 10_000;
    }

    // Clean expired keys
    const now = Date.now();
    for (const [id, entry] of this.decryptKeys) {
      if (entry.expires < now) {
        this.decryptKeys.delete(id);
      }
    }
  }

  /** Current epoch number (for sync checks) */
  getCurrentKeyId(): number {
    return this.currentKeyId;
  }

  /**
   * Debounced key rotation: collapses rapid join/leave bursts into a single
   * rotation. Waits 2s of silence, but never delays more than 5s from the
   * first pending event (max cap prevents indefinite deferral during a
   * sustained trickle of joins).
   */
  debouncedRotateKeys(): void {
    this.rotationPending = true;

    // Set the max-cap deadline on the first pending event
    if (!this.rotationDeadline) {
      this.rotationDeadline = Date.now() + MediaEncryption.MAX_CAP_MS;
    }

    if (this.rotationTimer) clearTimeout(this.rotationTimer);

    // If we've already exceeded the max cap, rotate immediately
    const remaining = this.rotationDeadline - Date.now();
    const delay = remaining <= 0 ? 0 : Math.min(MediaEncryption.DEBOUNCE_MS, remaining);

    this.rotationTimer = setTimeout(async () => {
      this.rotationTimer = null;
      this.rotationDeadline = 0;
      if (!this.rotationPending) return;
      this.rotationPending = false;
      await this.rotateKeys();
    }, delay);
  }

  /**
   * Catch up to a server-reported epoch by ratcheting forward.
   * Used by the periodic epoch-sync mechanism to recover from missed events.
   */
  async catchUpToEpoch(targetEpoch: number): Promise<void> {
    while (this.currentKeyId < targetEpoch) {
      const steps = targetEpoch - this.currentKeyId;
      if (steps > 100) {
        throw new Error('E2EE epoch gap too large (' + steps + '), rejoin required');
      }
      const before = this.currentKeyId;
      await this.rotateKeys();
      await this.rotationChain;
      if (this.currentKeyId <= before) {
        throw new Error('E2EE epoch catch-up stalled, rejoin required');
      }
    }
  }

  /** Encrypt a media frame (called by Insertable Streams transform).
   *  Throws if encryption fails — caller should drop the frame. */
  async encryptFrame(frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame): Promise<void> {
    if (!this.encryptKey) throw new Error('E2EE: no encrypt key');

    const encryptKey = this.encryptKey;
    const keyId = this.currentKeyId;
    const data = new Uint8Array(frame.data);

    // #1742 root cause: an empty (0-byte) DTX frame would otherwise encrypt to
    // a small fixed blob (0-byte header + 16-byte GCM tag over an empty payload
    // + trailer) that falls just below the decrypt side's "too small to be
    // encrypted" guard (`length < TRAILER_SIZE_V3 + MIN_GCM_OVERHEAD`). Such a
    // frame would be misclassified as unencrypted and fed to the Opus decoder
    // UNDECIPHERED — the receiver-side garble-during-silence (originally
    // confirmed via two-client capture 2026-06-22 under the v2 trailer). An
    // empty frame carries no audio content, so pass it through unchanged: it
    // stays 0 bytes end-to-end and the decoder treats it as DTX silence,
    // symmetric with the decrypt passthrough. Do NOT instead lower the decrypt
    // threshold — the blob would then fail the small-cipher guard with a
    // mismatched header, and the garble merely moves to a different branch.
    if (data.length === 0) return;

    // Determine unencrypted header bytes based on frame type.
    // VP8 keyframes need 10 bytes, delta frames need 3, audio needs 1.
    // Per RFC 6386 §9.1 and the official WebRTC Insertable Streams sample.
    const headerBytes = getUnencryptedBytes(frame);

    // Split: header (unencrypted for SFU routing) + payload (encrypted)
    const header = data.slice(0, headerBytes);
    const payload = data.slice(headerBytes);

    // Generate IV: full 96-bit random nonce per GCM operation.
    const iv = new Uint8Array(12);
    crypto.getRandomValues(iv);

    // Encrypt payload with AES-256-GCM (no AAD — frame misrouting between
    // consumers in a BUNDLE group can change the receiver's frame.type,
    // which would cause an AAD mismatch if we authenticated the header).
    const ciphertext = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, encryptKey, payload);

    // Compose v3 trailer: [headerBytes:1][keyId:2 BE][keyVersion:4 BE][IV:12][magic:2]
    const keyVersion = this.currentKeyVersion;
    const result = new Uint8Array(header.length + ciphertext.byteLength + TRAILER_SIZE_V3);
    result.set(header);
    result.set(new Uint8Array(ciphertext), header.length);
    const view = new DataView(result.buffer);
    let offset = header.length + ciphertext.byteLength;
    // Encode the ACTUAL header length, not the static getUnencryptedBytes()
    // value. They are equal for every real frame (data.length >= headerBytes),
    // so the wire format is byte-identical for production traffic — but for a
    // frame shorter than headerBytes (e.g. a 1-byte video frame), storing the
    // static value would make the decrypt header/ciphertext split overrun into
    // the trailer (#1742 latent video boundary, H5). The 0-byte case is already
    // handled by the early return above; this closes the residual sub-header case.
    result[offset] = header.length; // headerBytes (1)
    offset += 1;
    view.setUint16(offset, keyId & 0xffff, false); // keyId (2, BE)
    offset += 2;
    view.setUint32(offset, keyVersion >>> 0, false); // keyVersion (4, BE)
    offset += 4;
    result.set(iv, offset); // IV (12)
    offset += 12;
    result[offset] = MAGIC_0;
    result[offset + 1] = MAGIC_1;

    frame.data = result.buffer;
  }

  /**
   * Self-healing key derivation: when we receive a frame with a keyId we don't
   * have, ratchet forward from the nearest known key for that sender.
   * This handles the race where sender rotated keys before we learned the new epoch.
   * Max 10 ratchets to prevent abuse.
   */
  private async deriveRatchetedKey(
    senderUserId: string,
    keyVersion: number,
    targetKeyId: number
  ): Promise<{ key: CryptoKey; expires: number } | null> {
    let bestKeyId = -1;
    let bestEntry: { key: CryptoKey; expires: number } | null = null;

    // #1878: parse the 3-part key (senderId:keyVersion:keyId) by field, never
    // positional index — and ONLY ratchet entries of the SAME keyVersion. A
    // v(M) key must never ratchet toward a v(N) target.
    for (const [id, entry] of this.decryptKeys) {
      const parts = id.split(':');
      if (parts.length !== 3) continue; // sender UUIDs have no colons
      const [sid, ver, kid] = parts;
      if (sid !== senderUserId) continue;
      if (Number(ver) !== keyVersion) continue;
      const kidNum = Number.parseInt(kid, 10);
      if (kidNum < targetKeyId && kidNum > bestKeyId) {
        bestKeyId = kidNum;
        bestEntry = entry;
      }
    }

    if (!bestEntry || bestKeyId < 0) return null;

    const steps = targetKeyId - bestKeyId;
    if (steps > 100) return null; // Sanity limit (covers many join/leave cycles)

    let currentKey = bestEntry.key;
    for (let i = bestKeyId; i < targetKeyId; i++) {
      currentKey = await ratchetKey(currentKey);
      this.decryptKeys.set(MediaEncryption.mapKey(senderUserId, keyVersion, i + 1), {
        key: currentKey,
        expires: Date.now() + 3_600_000,
      });
    }

    return (
      this.decryptKeys.get(MediaEncryption.mapKey(senderUserId, keyVersion, targetKeyId)) ?? null
    );
  }

  /**
   * Decrypt a media frame (called by Insertable Streams transform).
   *
   * Returns normally if decryption succeeds or the frame is empty (e.g. DTX).
   * Throws for any non-empty frame without the E2EE magic trailer or any frame
   * that carries the trailer but cannot be decrypted. The caller should DROP
   * those frames to avoid feeding plaintext-policy violations or ciphertext
   * into the audio/video decoder.
   */
  async decryptFrame(
    frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    senderUserId: string
  ): Promise<void> {
    const data = new Uint8Array(frame.data);

    // Empty frames can appear for DTX/control cases and carry no plaintext media.
    if (data.length === 0) {
      return;
    }

    const hasMagic = data.length >= 2 && data.at(-1) === MAGIC_1 && data.at(-2) === MAGIC_0;
    if (!hasMagic) {
      throw new Error('E2EE: unencrypted media frame received');
    }

    // Minimum: 1 header + 16 GCM tag + 21 trailer (derive from the constant).
    if (data.length < TRAILER_SIZE_V3 + MIN_GCM_OVERHEAD) {
      throw new Error('E2EE: malformed encrypted frame too small');
    }

    // v3 trailer offsets from end: magic[-2,-1], IV[-14,-3],
    // keyVersion[-18,-15] (4B BE), keyId[-20,-19] (2B BE), headerBytes[-21].
    // The length guard above mathematically guarantees these indices are
    // in-bounds; DataView reads still need an explicit narrow on headerBytes.
    const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
    const n = data.length;
    const iv = data.slice(n - 14, n - 2);
    const keyVersion = view.getUint32(n - 18, false);
    const keyId = view.getUint16(n - 20, false);
    const headerBytes = data.at(n - 21); // Sender-encoded header byte count
    if (headerBytes === undefined) {
      // Unreachable in practice: the length guard above guarantees index -21 is
      // defined. If we ever reach here, the guard was weakened or the data was
      // mutated mid-function — either way the frame is corrupted and
      // silent-passthrough would hide a real invariant bug. The magic-trailer
      // check above already identified this as our frame.
      throw new Error(
        'mediaEncryption: decrypt invariant violated — headerBytes undefined despite length guard'
      );
    }

    // Sanity check: headerBytes should be 1-10
    if (headerBytes < 1 || headerBytes > 10 || headerBytes >= data.length - TRAILER_SIZE_V3) {
      throw new Error('E2EE: malformed encrypted frame trailer');
    }

    const header = data.slice(0, headerBytes);
    const ciphertext = data.slice(headerBytes, data.length - TRAILER_SIZE_V3);

    // AES-GCM ciphertext must be at least 16 bytes (the auth tag alone)
    if (ciphertext.byteLength < 16) {
      throw new Error('E2EE: malformed encrypted frame ciphertext');
    }

    // Find the right decryption key, ratcheting forward if needed. #1878: the
    // map key is senderId:keyVersion:keyId, both read straight from the frame.
    let keyEntry = this.decryptKeys.get(MediaEncryption.mapKey(senderUserId, keyVersion, keyId));
    keyEntry ??= (await this.deriveRatchetedKey(senderUserId, keyVersion, keyId)) ?? undefined;
    if (!keyEntry) {
      // Count available keys for this sender (avoid full iteration in hot path)
      let availableCount = 0;
      for (const id of this.decryptKeys.keys()) {
        if (id.startsWith(`${senderUserId}:`)) availableCount++;
      }
      throw new FrameKeyMissError(
        senderUserId,
        keyVersion,
        keyId,
        `E2EE: no decrypt key for sender=${senderUserId} v=${keyVersion} keyId=${keyId} (${availableCount} keys held for sender)`
      );
    }

    const plaintext = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv },
      keyEntry.key,
      ciphertext
    );

    // Reconstruct: [header][plaintext]
    const result = new Uint8Array(header.length + plaintext.byteLength);
    result.set(header);
    result.set(new Uint8Array(plaintext), header.length);
    frame.data = result.buffer;
  }

  /** Clean up */
  destroy(): void {
    if (this.rotationTimer) {
      clearTimeout(this.rotationTimer);
      this.rotationTimer = null;
    }
    this.rotationPending = false;
    this.rotationDeadline = 0;
    this.rotationChain = Promise.resolve();
    this.encryptKey = null;
    this.decryptKeys.clear();
  }
}
