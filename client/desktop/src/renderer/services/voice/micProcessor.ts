import processorUrl from './micProcessor.worklet.js?url&no-inline';

export type GateRuntime = { kind: 'off' } | { kind: 'fixed'; thresholdDbfs: number };

export type PeakWindow = {
  peak: number;
  frames: number;
  valid: boolean;
  overloaded: boolean;
};

export type MicProcessorInitial = {
  protectAgcPeaks: boolean;
  gate: GateRuntime;
};

export type MicProcessorHandle = {
  node: AudioWorkletNode;
  setGate(gate: GateRuntime): void;
  setWindowReporting(enabled: boolean): void;
  close(): void;
};

export async function createMicProcessor(
  context: AudioContext,
  initial: MicProcessorInitial,
  onWindow: (window: PeakWindow) => void,
  onProcessorError: () => void
): Promise<MicProcessorHandle> {
  validateInitial(initial);
  if (!Number.isFinite(context.sampleRate) || context.sampleRate <= 0) {
    throw new TypeError('Invalid microphone processor configuration');
  }
  const reportFrames = Math.max(1, Math.round(context.sampleRate * 0.02));
  let node: AudioWorkletNode | undefined;
  let closed = false;
  let failed = false;

  const notifyError = () => {
    if (failed) return;
    failed = true;
    try {
      onProcessorError();
    } catch {
      // Consumer errors must not escape an AudioWorklet event callback.
    }
  };

  const close = () => {
    if (closed) return;
    closed = true;
    if (!node) return;
    try {
      node.port.postMessage({ type: 'close' });
    } catch {
      // A failed or already closed worklet may have closed its port first.
    }
    try {
      node.port.close();
    } catch {
      // Continue disconnecting even if the port is already unavailable.
    }
    try {
      node.disconnect();
    } catch {
      // Teardown is best effort after the handle has entered its closed state.
    }
  };

  try {
    await context.audioWorklet.addModule(processorUrl);
    const createdNode = new AudioWorkletNode(context, 'concord-mic-processor', {
      processorOptions: initial,
      numberOfInputs: 1,
      numberOfOutputs: 1,
      channelCount: 2,
      channelCountMode: 'clamped-max',
      channelInterpretation: 'speakers',
    });
    node = createdNode;
    node.port.onmessage = ({ data }: MessageEvent<unknown>) => {
      if (closed || failed || !isPeakWindow(data, reportFrames)) return;
      onWindow(data);
    };
    node.addEventListener('processorerror', () => {
      notifyError();
      close();
    });
  } catch (error) {
    notifyError();
    close();
    throw error;
  }

  if (!node) throw new Error('Microphone processor construction failed');
  return {
    node,
    setGate(gate) {
      validateGate(gate);
      if (!closed && !failed) node?.port.postMessage({ type: 'setGate', gate });
    },
    setWindowReporting(enabled) {
      if (typeof enabled !== 'boolean') throw new TypeError('Invalid window reporting setting');
      if (!closed && !failed) node?.port.postMessage({ type: 'setWindowReporting', enabled });
    },
    close,
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  const actual = Object.keys(value);
  return actual.length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function validateGate(value: unknown): asserts value is GateRuntime {
  if (!isRecord(value)) throw new TypeError('Invalid microphone gate');
  if (value.kind === 'off' && hasExactKeys(value, ['kind'])) return;
  if (
    value.kind === 'fixed' &&
    hasExactKeys(value, ['kind', 'thresholdDbfs']) &&
    typeof value.thresholdDbfs === 'number' &&
    Number.isFinite(value.thresholdDbfs) &&
    value.thresholdDbfs >= -80 &&
    value.thresholdDbfs <= -20
  ) {
    return;
  }
  throw new TypeError('Invalid microphone gate');
}

function validateInitial(value: unknown): asserts value is MicProcessorInitial {
  if (
    !isRecord(value) ||
    !hasExactKeys(value, ['protectAgcPeaks', 'gate']) ||
    typeof value.protectAgcPeaks !== 'boolean'
  ) {
    throw new TypeError('Invalid microphone processor configuration');
  }
  try {
    validateGate(value.gate);
  } catch {
    throw new TypeError('Invalid microphone processor configuration');
  }
}

function isPeakWindow(value: unknown, expectedFrames: number): value is PeakWindow {
  if (!isRecord(value) || !hasExactKeys(value, ['peak', 'frames', 'valid', 'overloaded'])) {
    return false;
  }
  return (
    typeof value.peak === 'number' &&
    Number.isFinite(value.peak) &&
    value.peak >= 0 &&
    value.frames === expectedFrames &&
    typeof value.valid === 'boolean' &&
    typeof value.overloaded === 'boolean'
  );
}
