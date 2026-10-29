import assert from 'node:assert/strict';
import test from 'node:test';
import { createRemoteAudioWitness } from './audio-witness.mjs';

function fixture(play = () => Promise.resolve()) {
  const events = [];
  const remoteStream = { getTracks: () => { throw new Error('Borrowed tracks must not be stopped'); } };
  const nodes = ['source', 'analyser', 'silence', 'destination'].map((name) => ({
    name, gain: { value: 1 },
    connect(next) { events.push(['connect', name, next.name]); return next; },
    disconnect() { events.push(['disconnect', name]); },
  }));
  const [source, analyser, silence, destination] = nodes;
  const element = {
    set muted(value) { events.push(['muted', value]); },
    set srcObject(value) { events.push(['srcObject', value]); },
    play() { events.push(['play']); return play(); },
    pause() { events.push(['pause']); },
    remove() { events.push(['remove']); },
  };
  const document = { createElement(tag) { assert.equal(tag, 'audio'); events.push(['create', tag]); return element; } };
  const context = {
    destination,
    createMediaStreamSource(stream) { assert.equal(stream, remoteStream); return source; },
    createAnalyser() { return analyser; }, createGain() { return silence; },
  };
  return { events, remoteStream, nodes, analyser, silence, document, context };
}

test('server resume can finish before the remote pull element waits for playback', async () => {
  const f = fixture();
  const witness = createRemoteAudioWitness(f.context, f.remoteStream, f.document);
  assert(!f.events.some(([name]) => name === 'play'), 'Do not wait for media while the server consumer is paused');
  f.events.push(['server-resumed']);
  await witness.start();
  assert(f.events.findIndex(([name]) => name === 'play') > f.events.findIndex(([name]) => name === 'server-resumed'));
  witness.close();
});

test('remote waveform graph pulls the same remote stream through a muted playing element', async () => {
  const f = fixture();
  const witness = createRemoteAudioWitness(f.context, f.remoteStream, f.document);
  await witness.start();
  assert.equal(witness.analyser, f.analyser);
  assert.deepEqual(f.events.slice(0, 3), [
    ['create', 'audio'], ['muted', true], ['srcObject', f.remoteStream],
  ]);
  assert.equal(f.silence.gain.value, 0, 'Do not introduce audible or unattenuated output');
  assert.deepEqual(f.events.filter(([name]) => name === 'connect'), [
    ['connect', 'source', 'analyser'], ['connect', 'analyser', 'silence'], ['connect', 'silence', 'destination'],
  ]);
  witness.close(); witness.close();
  assert.deepEqual(f.events.filter(([name]) => ['pause', 'srcObject', 'remove', 'disconnect'].includes(name)).slice(1), [
    ['pause'], ['srcObject', null], ['remove'], ['disconnect', 'source'], ['disconnect', 'analyser'], ['disconnect', 'silence'],
  ]);
});

test('a pull element that has not started playing cannot arm the waveform witness', async () => {
  let start;
  const f = fixture(() => new Promise((resolve) => { start = resolve; }));
  let ready = false;
  const witness = createRemoteAudioWitness(f.context, f.remoteStream, f.document);
  const pending = witness.start().then(() => { ready = true; });
  await Promise.resolve();
  assert.equal(ready, false);
  start();
  await pending;
  assert.equal(ready, true);
  witness.close();
});

test('rejected remote playback fails closed and releases the pull element', async () => {
  const f = fixture(() => Promise.reject(new Error('Fixture playback refused')));
  const witness = createRemoteAudioWitness(f.context, f.remoteStream, f.document);
  await assert.rejects(witness.start(), /Fixture remote audio pull playback failed/);
  assert(f.events.some(([name]) => name === 'pause'));
  assert(f.events.some(([name, value]) => name === 'srcObject' && value === null));
  assert(f.events.some(([name]) => name === 'remove'));
  assert.equal(f.events.filter(([name]) => name === 'disconnect').length, 3);
});

test('a graph setup failure closes the owned pull element and any created nodes', () => {
  const f = fixture();
  f.context.createAnalyser = () => { throw new Error('Fixture graph refused'); };
  assert.throws(() => createRemoteAudioWitness(f.context, f.remoteStream, f.document), /Fixture graph refused/);
  assert(f.events.some(([name]) => name === 'pause'));
  assert(f.events.some(([name, value]) => name === 'srcObject' && value === null));
  assert.deepEqual(f.events.filter(([name]) => name === 'disconnect'), [['disconnect', 'source']]);
});

test('playback has a bounded deadline and timeout closes the remote graph', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const f = fixture(() => new Promise(() => {}));
  const witness = createRemoteAudioWitness(f.context, f.remoteStream, f.document);
  const result = assert.rejects(witness.start(), /Fixture remote audio pull playback failed/);
  t.mock.timers.tick(10_000);
  await result;
  assert(f.events.some(([name]) => name === 'pause'));
  assert(f.events.some(([name, value]) => name === 'srcObject' && value === null));
  assert.equal(f.events.filter(([name]) => name === 'disconnect').length, 3);
});
