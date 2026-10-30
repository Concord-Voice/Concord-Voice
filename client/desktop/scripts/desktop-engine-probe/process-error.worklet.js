// A synthetic process()-failure control for the unchanged production host.
// Healthy impulse and calibration checks use the emitted production worklet.
class ProcessorErrorFixture extends AudioWorkletProcessor {
  constructor() {
    super();
    try {
      this.port.postMessage({ type: 'engine-probe-constructor-entered' });
    } catch {
      /* A diagnostic marker must never become the injected exception. */
    }
  }

  process() {
    try {
      this.port.postMessage({ type: 'engine-probe-process-entered' });
    } catch {
      /* Native event delivery, not this marker, is required to pass. */
    }
    throw new Error('Deliberate engine probe process failure');
  }
}

registerProcessor('concord-mic-processor', ProcessorErrorFixture);
