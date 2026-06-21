/**
 * Type declarations for the WebRTC Encoded Transform API (RTCRtpScriptTransform).
 *
 * Chrome 129+ replacement for the deprecated createEncodedStreams() API.
 * These types are not yet in TypeScript's lib.dom.d.ts.
 */

declare class RTCRtpScriptTransform {
  constructor(worker: Worker, options?: unknown, transfer?: Transferable[]);
}

/** Event fired in the Worker when a sender/receiver transform is assigned */
interface RTCTransformEvent extends Event {
  readonly transformer: RTCRtpScriptTransformer;
}

interface RTCRtpScriptTransformer {
  readonly readable: ReadableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>;
  readonly writable: WritableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>;
  readonly options: unknown;
}

/** Extend RTCRtpSender/RTCRtpReceiver with the transform property */
interface RTCRtpSender {
  transform?: RTCRtpScriptTransform | null;
}

interface RTCRtpReceiver {
  transform?: RTCRtpScriptTransform | null;
}

/** Worker global handler for RTCRtpScriptTransform events */
interface DedicatedWorkerGlobalScope {
  onrtctransform: ((event: RTCTransformEvent) => void) | null | undefined;
}
