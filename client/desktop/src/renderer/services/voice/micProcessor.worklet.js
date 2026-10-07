const PEAK_CAP = 0.5011872053146362; // Float32 below -6 dBFS.

function validGate(gate) {
  return (
    gate?.kind === 'off' ||
    (gate?.kind === 'fixed' &&
      Number.isFinite(gate.thresholdDbfs) &&
      gate.thresholdDbfs >= -80 &&
      gate.thresholdDbfs <= -20)
  );
}

function cappedSample(sample) {
  return Number.isFinite(sample) ? Math.fround(Math.max(-PEAK_CAP, Math.min(PEAK_CAP, sample))) : 0;
}

class ConcordMicProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const initial = options?.processorOptions;
    if (typeof initial?.protectAgcPeaks !== 'boolean' || !validGate(initial.gate)) {
      throw new Error('Invalid microphone processor options');
    }

    this.protectAgcPeaks = initial.protectAgcPeaks;
    this.gate = initial.gate;
    this.gateThreshold = this.gate.kind === 'fixed' ? 10 ** (this.gate.thresholdDbfs / 20) : 0;
    this.gateGain = this.gate.kind === 'off' ? 1 : 0;
    this.gateRelease = Math.exp(-1 / (sampleRate * 0.015));
    this.release = Math.exp(-1 / (sampleRate * 0.1));
    this.windowFrames = Math.max(1, Math.round(sampleRate * 0.02));
    this.delay = Math.max(1, Math.ceil(sampleRate * 0.004));
    this.delayed = [new Float32Array(this.delay), new Float32Array(this.delay)];
    this.delayedValid = new Uint8Array(this.delay);
    this.delayedOverloaded = new Uint8Array(this.delay);
    this.peakIndexes = new Float64Array(this.delay + 1);
    this.peakValues = new Float64Array(this.delay + 1);
    this.channelCount = 0;
    this.resetLookahead();
    this.reporting = false;
    this.resetWindow();
    this.closed = false;

    this.port.onmessage = ({ data }) => {
      if (data?.type === 'close') {
        this.closed = true;
      } else if (data?.type === 'setGate' && validGate(data.gate)) {
        const wasFixed = this.gate.kind === 'fixed';
        this.gate = data.gate;
        this.gateThreshold = this.gate.kind === 'fixed' ? 10 ** (this.gate.thresholdDbfs / 20) : 0;
        if (this.gate.kind === 'off') this.gateGain = 1;
        else if (!wasFixed) this.gateGain = 0;
      } else if (data?.type === 'setWindowReporting' && typeof data.enabled === 'boolean') {
        this.reporting = data.enabled;
        this.resetWindow();
      } else {
        throw new Error('Invalid microphone processor message');
      }
    };
  }

  resetLookahead() {
    this.delayed[0].fill(0);
    this.delayed[1].fill(0);
    this.delayedValid.fill(0);
    this.delayedOverloaded.fill(0);
    this.frameIndex = 0;
    this.queueHead = 0;
    this.queueTail = 0;
    this.queueSize = 0;
    this.envelope = 1;
  }

  resetWindow() {
    this.reportFrames = 0;
    this.reportPeak = 0;
    this.reportValid = true;
    this.reportOverloaded = false;
  }

  observe(peak, valid, overloaded) {
    if (!this.reporting) return;
    this.reportPeak = Math.max(this.reportPeak, peak);
    this.reportValid &&= valid;
    this.reportOverloaded ||= overloaded;
    if (++this.reportFrames === this.windowFrames) {
      this.port.postMessage({
        peak: this.reportPeak,
        frames: this.reportFrames,
        valid: this.reportValid,
        overloaded: this.reportOverloaded,
      });
      this.resetWindow();
    }
  }

  validBlock(input, output, channels, frames) {
    return (
      (channels === 1 || channels === 2) &&
      input.length === channels &&
      output.every((channel) => channel.length === frames) &&
      input.every((channel) => channel.length === frames)
    );
  }

  silenceInvalidBlock(output, frames) {
    for (const channel of output) channel.fill(0);
    this.channelCount = 0;
    this.resetLookahead();
    for (let frame = 0; frame < frames; frame++) this.observe(0, false, false);
  }

  advancePeakQueue(inputPeak) {
    const capacity = this.delay + 1;
    const index = this.frameIndex;
    while (this.queueSize && this.peakIndexes[this.queueHead] < index - this.delay) {
      this.queueHead = (this.queueHead + 1) % capacity;
      this.queueSize--;
    }
    while (this.queueSize) {
      const last = (this.queueTail + capacity - 1) % capacity;
      if (this.peakValues[last] > inputPeak) break;
      this.queueTail = last;
      this.queueSize--;
    }
    this.peakIndexes[this.queueTail] = index;
    this.peakValues[this.queueTail] = inputPeak;
    this.queueTail = (this.queueTail + 1) % capacity;
    this.queueSize++;
  }

  delayAndLimitFrame(inputPeak, channels) {
    this.advancePeakQueue(inputPeak);
    const linkedPeak = this.peakValues[this.queueHead];
    const requiredGain = linkedPeak > PEAK_CAP ? PEAK_CAP / linkedPeak : 1;
    this.envelope =
      requiredGain < this.envelope
        ? requiredGain
        : Math.min(requiredGain, 1 - (1 - this.envelope) * this.release);

    const slot = this.frameIndex % this.delay;
    const current0 = this.frameSample0;
    const current1 = this.frameSample1;
    if (this.frameIndex >= this.delay) {
      this.frameSample0 = this.delayed[0][slot] * this.envelope;
      this.frameSample1 = channels === 2 ? this.delayed[1][slot] * this.envelope : 0;
      this.frameValid = this.delayedValid[slot] === 1;
      this.frameOverloaded = this.delayedOverloaded[slot] === 1;
    } else {
      this.frameSample0 = this.frameSample1 = 0;
      this.frameValid = true;
      this.frameOverloaded = false;
    }
    this.delayed[0][slot] = current0;
    if (channels === 2) this.delayed[1][slot] = current1;
    this.delayedValid[slot] = this.inputValid ? 1 : 0;
    this.delayedOverloaded[slot] = this.inputValid && inputPeak >= 1 ? 1 : 0;
    this.frameIndex++;

    // The envelope does the limiting; this guard covers Float32 rounding only.
    this.frameSample0 = cappedSample(this.frameSample0);
    this.frameSample1 = cappedSample(this.frameSample1);
  }

  writeFrame(output, frame, channels) {
    const peak = Math.max(Math.abs(this.frameSample0), Math.abs(this.frameSample1));
    this.observe(peak, this.frameValid, this.frameOverloaded);
    if (this.gate.kind === 'fixed') {
      this.gateGain = peak >= this.gateThreshold ? 1 : this.gateGain * this.gateRelease;
    }
    output[0][frame] = this.frameSample0 * this.gateGain;
    if (channels === 2) output[1][frame] = this.frameSample1 * this.gateGain;
  }

  processFrame(input, output, frame, channels) {
    let sample0 = input[0][frame];
    let sample1 = channels === 2 ? input[1][frame] : 0;
    const valid = Number.isFinite(sample0) && (channels === 1 || Number.isFinite(sample1));
    if (!valid) sample0 = sample1 = 0;
    const inputPeak = Math.max(Math.abs(sample0), Math.abs(sample1));
    this.frameSample0 = sample0;
    this.frameSample1 = sample1;
    this.inputValid = this.frameValid = valid;
    this.frameOverloaded = valid && inputPeak >= 1;
    if (this.protectAgcPeaks) this.delayAndLimitFrame(inputPeak, channels);
    this.writeFrame(output, frame, channels);
  }

  process(inputs, outputs) {
    const output = outputs[0] ?? [];
    const input = inputs[0] ?? [];
    const channels = output.length;
    const frames = output[0]?.length ?? 0;
    if (this.closed) {
      for (const channel of output) channel.fill(0);
      return false;
    }

    if (!this.validBlock(input, output, channels, frames)) {
      this.silenceInvalidBlock(output, frames);
      return true;
    }

    if (channels !== this.channelCount) {
      this.channelCount = channels;
      this.resetLookahead();
    }

    for (let frame = 0; frame < frames; frame++) {
      this.processFrame(input, output, frame, channels);
    }
    return true;
  }
}

registerProcessor('concord-mic-processor', ConcordMicProcessor);

export {};
