// Test-only remote waveform graph. The runtime probe owns these nodes, while
// the remote consumer retains ownership of its track and the shared context.
export function createRemoteAudioWitness(context, stream, document) {
  const element = document.createElement('audio');
  const nodes = [];
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    element.pause();
    element.srcObject = null;
    element.remove();
    for (const node of nodes) node.disconnect();
  };
  try {
    // Match ParticipantGrid's modern receive graph: a muted media element must
    // pull the SAME remote stream before createMediaStreamSource renders it.
    // The element must never become a second audible route to the speakers.
    element.muted = true;
    element.srcObject = stream;
    const source = context.createMediaStreamSource(stream); nodes.push(source);
    const analyser = context.createAnalyser(); nodes.push(analyser);
    const silence = context.createGain(); nodes.push(silence); silence.gain.value = 0;
    source.connect(analyser).connect(silence).connect(context.destination);
    return { analyser, close, async start() {
      if (closed) throw new Error('Fixture remote audio witness is closed');
      let timer;
      try {
        // The caller resumes the server consumer first: play() can wait for
        // initial media, which a paused server-side consumer cannot deliver.
        await Promise.race([
          element.play(),
          new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('Playback deadline')), 10_000); }),
        ]);
      } catch {
        close();
        throw new Error('Fixture remote audio pull playback failed');
      } finally { clearTimeout(timer); }
    } };
  } catch (error) {
    close();
    throw error;
  }
}
