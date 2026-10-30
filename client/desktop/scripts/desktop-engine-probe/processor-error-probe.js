'use strict';

// This observes a deliberate fixture process() exception. It does not inject an
// event, alter the production host or claim to test MessagePort exception delivery.
async function probeNativeProcessorError(context, createProcessor) {
  let callbacks = 0;
  const handle = await createProcessor(
    context,
    { protectAgcPeaks: true, gate: { kind: 'off' } },
    () => {},
    () => callbacks++
  );
  const node = handle.node;
  let nativeEvents = 0,
    trustedEvents = 0,
    portCloses = 0,
    disconnects = 0;
  let constructorEntries = 0,
    processEntries = 0;
  let errorEvents = 0,
    trustedErrorEvents = 0;
  let renderCompleted = false,
    inputStarted = false;
  let input;
  const originalClose = node.port.close;
  const originalDisconnect = node.disconnect;
  let timer;
  let eventArrived;
  const event = new Promise((resolve) => {
    eventArrived = resolve;
  });
  const observe = (error) => {
    nativeEvents = Math.min(1000, nativeEvents + 1);
    if (error.isTrusted === true && error.target === node && error.type === 'processorerror')
      trustedEvents = Math.min(1000, trustedEvents + 1);
    eventArrived();
  };
  const observeEntry = ({ data }) => {
    if (!data || typeof data !== 'object' || Object.keys(data).length !== 1) return;
    if (data.type === 'engine-probe-constructor-entered')
      constructorEntries = Math.min(1000, constructorEntries + 1);
    if (data.type === 'engine-probe-process-entered')
      processEntries = Math.min(1000, processEntries + 1);
  };
  const observeCompatibilityError = (error) => {
    errorEvents = Math.min(1000, errorEvents + 1);
    if (error.isTrusted === true && error.target === node && error.type === 'error')
      trustedErrorEvents = Math.min(1000, trustedErrorEvents + 1);
    eventArrived();
  };
  // Forward the real receiver, arguments, return value and exceptions unchanged.
  // A retirement count increments only after the actual native call succeeds.
  node.port.close = function (...args) {
    const result = Reflect.apply(originalClose, this, args);
    portCloses++;
    return result;
  };
  node.disconnect = function (...args) {
    const result = Reflect.apply(originalDisconnect, this, args);
    disconnects++;
    return result;
  };
  node.addEventListener('processorerror', observe);
  // Keep the two native names distinct: compatibility delivery never becomes
  // evidence that the standard processorerror contract succeeded.
  node.addEventListener('error', observeCompatibilityError);
  node.port.addEventListener('message', observeEntry);
  try {
    // Drive the fault with real, nonzero input instead of depending on an
    // inputless graph's scheduling. It still must produce the native error.
    const stimulus = context.createBuffer(
      1,
      Math.max(1, Math.round(context.sampleRate * 0.1)),
      context.sampleRate
    );
    stimulus.getChannelData(0)[0] = 1;
    input = context.createBufferSource();
    input.buffer = stimulus;
    input.connect(node);
    node.connect(context.destination);
    input.start();
    inputStarted = true;
    const rendering = context.startRendering().then(() => {
      renderCompleted = true;
    });
    await Promise.race([
      Promise.all([rendering, event]),
      new Promise((_, reject) => {
        timer = setTimeout(
          () =>
            reject(
              new Error(
                `Native worklet failure deadline: ${JSON.stringify({
                  inputStarted,
                  renderCompleted,
                  nativeEvents,
                  trustedEvents,
                  callbacks,
                  portCloses,
                  disconnects,
                  constructorEntries,
                  processEntries,
                  errorEvents,
                  trustedErrorEvents,
                })}`
              )
            ),
          2000
        );
      }),
    ]);
    const standard = nativeEvents === 1 && trustedEvents === 1;
    const compatibility = errorEvents === 1 && trustedErrorEvents === 1;
    if (
      nativeEvents > 1 ||
      errorEvents > 1 ||
      nativeEvents !== trustedEvents ||
      errorEvents !== trustedErrorEvents ||
      (!standard && !compatibility) ||
      Object.values({ callbacks, portCloses, disconnects }).some((count) => count !== 1)
    )
      throw new Error(
        'Native worklet failure event or production host retirement was not observed exactly once'
      );
    return {
      eventMode: standard ? (compatibility ? 'both' : 'processorerror') : 'error',
      standardProcessorError: standard ? 'PASS' : 'BROKEN',
      compatibilityError: compatibility ? 'PASS' : 'NOT RUN',
      nativeEvents,
      trustedEvents,
      errorEvents,
      trustedErrorEvents,
      callbacks,
      portCloses,
      disconnects,
    };
  } finally {
    clearTimeout(timer);
    // Cleanup cannot satisfy the assertion: evidence was captured and checked first.
    handle.close();
    node.removeEventListener('processorerror', observe);
    node.removeEventListener('error', observeCompatibilityError);
    node.port.removeEventListener('message', observeEntry);
    node.port.close = originalClose;
    node.disconnect = originalDisconnect;
    try {
      if (inputStarted) input.stop();
    } finally {
      input?.disconnect();
    }
  }
}

module.exports = { probeNativeProcessorError };
