const PEAK_CAP = 0.5011872053146362; // Float32 below -6 dBFS.
const DYNAMIC_HISTORY = 500; // Ten seconds of complete 20 ms observations.
const DYNAMIC_HOLD_SECONDS = 0.12;
const DYNAMIC_STABLE_TOLERANCE_DB = 3;
const DYNAMIC_MIN_ELEVATED_WINDOWS = 25;
const DYNAMIC_LONG_ELEVATED_WINDOWS = 100;
const DYNAMIC_STALE_ELEVATED_WINDOWS = 150;

function hasExactKeys(value, keys) {
  return (
    value !== null &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    Object.keys(value).length === keys.length &&
    keys.every((key) => Object.hasOwn(value, key))
  );
}

function validGate(gate) {
  return (
    ((gate?.kind === 'off' || gate?.kind === 'dynamic') && hasExactKeys(gate, ['kind'])) ||
    (gate?.kind === 'fixed' &&
      hasExactKeys(gate, ['kind', 'thresholdDbfs']) &&
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
    if (
      !hasExactKeys(initial, ['protectAgcPeaks', 'gate']) ||
      typeof initial.protectAgcPeaks !== 'boolean' ||
      !validGate(initial.gate)
    ) {
      throw new Error('Invalid microphone processor options');
    }

    this.protectAgcPeaks = initial.protectAgcPeaks;
    this.gate = initial.gate;
    this.gateThreshold = this.gate.kind === 'fixed' ? 10 ** (this.gate.thresholdDbfs / 20) : 0;
    this.gateGain = this.gate.kind === 'off' ? 1 : 0;
    this.gateRelease = Math.exp(-1 / (sampleRate * 0.015));
    this.dynamicHoldFrames = Math.round(sampleRate * DYNAMIC_HOLD_SECONDS);
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
    this.dynamicBins = new Uint8Array(DYNAMIC_HISTORY);
    this.dynamicHistogram = new Uint16Array(81);
    this.dynamicStatusFrames = 0;
    this.observedFrames = 0;
    this.resetDynamic();
    this.closed = false;

    this.port.onmessage = ({ data }) => {
      if (data?.type === 'close' && hasExactKeys(data, ['type'])) {
        this.closed = true;
      } else if (
        data?.type === 'setGate' &&
        hasExactKeys(data, ['type', 'gate']) &&
        validGate(data.gate)
      ) {
        const wasFixed = this.gate.kind === 'fixed';
        this.gate = data.gate;
        this.gateThreshold = this.gate.kind === 'fixed' ? 10 ** (this.gate.thresholdDbfs / 20) : 0;
        if (this.gate.kind === 'off') this.gateGain = 1;
        else if (this.gate.kind === 'dynamic') {
          this.gateGain = 1;
          this.resetDynamic();
          this.resetWindow();
        } else if (!wasFixed) this.gateGain = 0;
      } else if (
        data?.type === 'setWindowReporting' &&
        hasExactKeys(data, ['type', 'enabled']) &&
        typeof data.enabled === 'boolean'
      ) {
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

  resetDynamic() {
    this.dynamicHistogram.fill(0);
    this.dynamicCount = 0;
    this.dynamicNext = 0;
    this.dynamicWindows = 0;
    this.dynamicNextModel = 10;
    this.dynamicQualifying = 0;
    this.dynamicLosing = 0;
    this.dynamicConfidentWindows = 0;
    this.dynamicThresholdDbfs = null;
    this.dynamicThreshold = 0;
    this.dynamicLowBin = 0;
    this.dynamicHighBin = 0;
    this.dynamicHold = 0;
    this.dynamicShiftWindows = 0;
    this.dynamicRunBin = -1;
    this.dynamicRunLength = 0;
    this.dynamicOnsetBin = -1;
    this.dynamicBaselineBin = -1;
    this.dynamicHighStable = false;
    this.dynamicElevatedWindows = 0;
    this.dynamicObservedHighBin = -1;
    this.dynamicLastElevatedWindow = 0;
    this.dynamicSawOnset = false;
    this.dynamicOnsetCount = 0;
    this.dynamicAmbiguous = false;
    this.dynamicOnsetWindow = 0;
    this.dynamicLastValidWindow = 0;
  }

  trackDynamicShift(bin) {
    if (this.dynamicThresholdDbfs === null) return false;
    if (bin >= this.dynamicHighBin - 5) this.dynamicLastElevatedWindow = this.dynamicWindows;
    // ponytail: level alone cannot distinguish ambient from equally quiet speech;
    // expire confidence after a short quiet stretch until guided calibration can help.
    else if (
      this.dynamicWindows - this.dynamicLastElevatedWindow >=
      DYNAMIC_STALE_ELEVATED_WINDOWS
    ) {
      this.resetDynamic();
      return true;
    }
    if (
      bin < this.dynamicLowBin - 6 ||
      (bin > this.dynamicLowBin + 6 && bin < this.dynamicHighBin - 5)
    ) {
      this.dynamicShiftWindows++;
      if (this.dynamicShiftWindows >= 50) {
        this.resetDynamic();
        return true;
      }
    } else {
      this.dynamicShiftWindows = 0;
    }
    return false;
  }

  acceptDynamicOnset() {
    if (this.dynamicWindows - this.dynamicOnsetWindow > DYNAMIC_HISTORY) {
      this.dynamicOnsetCount = 0;
    }
    this.dynamicOnsetCount = Math.min(2, this.dynamicOnsetCount + 1);
    // One brief level change could be a background voice; wait for recurrence or a longer regime.
    this.dynamicSawOnset ||=
      this.dynamicOnsetCount >= 2 || this.dynamicElevatedWindows >= DYNAMIC_LONG_ELEVATED_WINDOWS;
    this.dynamicOnsetWindow = this.dynamicWindows;
    this.dynamicObservedHighBin = this.dynamicOnsetBin;
  }

  trackDynamicOnset(bin) {
    if (Math.abs(bin - this.dynamicRunBin) <= DYNAMIC_STABLE_TOLERANCE_DB) {
      this.dynamicRunLength++;
    } else {
      if (this.dynamicRunLength >= 5 && bin - this.dynamicRunBin >= 12) {
        this.dynamicOnsetBin = bin;
        this.dynamicBaselineBin = this.dynamicRunBin;
        this.dynamicHighStable = false;
        this.dynamicElevatedWindows = 0;
      }
      this.dynamicRunBin = bin;
      this.dynamicRunLength = 1;
    }
    if (this.dynamicOnsetBin >= 0 && bin >= this.dynamicBaselineBin + 12) {
      this.dynamicElevatedWindows++;
    }
    if (this.dynamicRunLength !== 5 || this.dynamicOnsetBin < 0) return;
    if (Math.abs(bin - this.dynamicOnsetBin) <= DYNAMIC_STABLE_TOLERANCE_DB) {
      this.dynamicHighStable = true;
    } else if (
      this.dynamicHighStable &&
      Math.abs(bin - this.dynamicBaselineBin) <= DYNAMIC_STABLE_TOLERANCE_DB
    ) {
      if (this.dynamicElevatedWindows >= DYNAMIC_MIN_ELEVATED_WINDOWS) this.acceptDynamicOnset();
      this.dynamicOnsetBin = -1;
      this.dynamicHighStable = false;
      this.dynamicElevatedWindows = 0;
    }
  }

  recordDynamicBin(bin) {
    if (this.dynamicCount === DYNAMIC_HISTORY) {
      this.dynamicHistogram[this.dynamicBins[this.dynamicNext]]--;
    } else {
      this.dynamicCount++;
    }
    this.dynamicBins[this.dynamicNext] = bin;
    this.dynamicHistogram[bin]++;
    this.dynamicNext = (this.dynamicNext + 1) % DYNAMIC_HISTORY;
    this.dynamicLastValidWindow = this.dynamicWindows;
  }

  observeDynamicWindow(peak, valid, overloaded) {
    this.dynamicWindows++;
    if (valid && !overloaded && peak > 0 && peak < 1) {
      const bin = Math.max(0, Math.min(80, Math.floor(20 * Math.log10(peak)) + 80));
      if (this.trackDynamicShift(bin)) return;
      this.trackDynamicOnset(bin);
      this.recordDynamicBin(bin);
    } else {
      this.dynamicRunBin = -1;
      this.dynamicRunLength = 0;
      this.dynamicOnsetBin = -1;
      this.dynamicHighStable = false;
    }
    this.updateDynamicModelIfDue();
  }

  updateDynamicConfidence(low, high, candidate, interval) {
    const separated =
      (this.dynamicThresholdDbfs !== null ||
        (this.dynamicSawOnset &&
          this.dynamicWindows - this.dynamicOnsetWindow <= DYNAMIC_HISTORY)) &&
      high - low >= 12 &&
      candidate >= -80 &&
      candidate <= -20 &&
      candidate <= high - 9;
    if (!separated) {
      this.dynamicQualifying = 0;
      if (this.dynamicThresholdDbfs !== null && ++this.dynamicLosing >= 3) {
        this.resetDynamic();
      }
      return;
    }
    this.dynamicLosing = 0;
    if (++this.dynamicQualifying < 3) return;
    this.dynamicLowBin = low + 80;
    this.dynamicHighBin = high + 80;
    this.dynamicThresholdDbfs =
      this.dynamicThresholdDbfs === null
        ? candidate
        : this.dynamicThresholdDbfs +
          Math.max(-1, Math.min(1, candidate - this.dynamicThresholdDbfs));
    this.dynamicThreshold = 10 ** (this.dynamicThresholdDbfs / 20);
    if (this.dynamicConfidentWindows === 0) this.dynamicLastElevatedWindow = this.dynamicWindows;
    this.dynamicConfidentWindows += interval;
  }

  updateDynamicModelIfDue() {
    if (this.dynamicWindows < this.dynamicNextModel) return;
    if (
      this.dynamicWindows - this.dynamicLastValidWindow >= 50 ||
      (this.dynamicThresholdDbfs === null &&
        this.dynamicSawOnset &&
        this.dynamicWindows - this.dynamicOnsetWindow > DYNAMIC_HISTORY)
    ) {
      this.resetDynamic();
      return;
    }
    let interval = 50;
    if (this.dynamicThresholdDbfs === null) interval = 10;
    else if (this.dynamicConfidentWindows >= 1500) interval = 250;
    this.dynamicNextModel = this.dynamicWindows + interval;
    if (this.dynamicCount < 150) return;

    const lowRank = Math.ceil(this.dynamicCount * 0.2);
    const highRank = Math.ceil(this.dynamicCount * 0.8);
    let seen = 0;
    let low = -81;
    let high = -81;
    for (let bin = 0; bin <= 80; bin++) {
      seen += this.dynamicHistogram[bin];
      if (low < -80 && seen >= lowRank) low = bin - 80;
      if (seen >= highRank) {
        high = bin - 80;
        break;
      }
    }
    if (
      this.dynamicOnsetCount >= 2 &&
      this.dynamicWindows - this.dynamicOnsetWindow <= DYNAMIC_HISTORY
    ) {
      high = Math.max(high, this.dynamicObservedHighBin - 80);
    }
    this.dynamicAmbiguous = high - low >= 3 && high - low < 12;
    this.updateDynamicConfidence(low, high, low + 3, interval);
  }

  dynamicGainForFrame(peak) {
    if (this.dynamicThresholdDbfs === null) return 1;
    if (peak >= this.dynamicThreshold) {
      this.dynamicHold = this.dynamicHoldFrames;
      return 1;
    }
    if (this.dynamicHold > 0) {
      this.dynamicHold--;
      return 1;
    }
    return Math.max(0.25, this.gateGain * this.gateRelease);
  }

  postDynamicStatusIfDue() {
    if (this.observedFrames - this.dynamicStatusFrames < sampleRate) return;
    this.dynamicStatusFrames = this.observedFrames;
    let state = 'adjusted';
    if (this.dynamicThresholdDbfs === null) {
      state = this.dynamicAmbiguous ? 'uncertain' : 'learning';
    }
    this.port.postMessage({
      type: 'dynamicGateStatus',
      state,
      thresholdDbfs: this.dynamicThresholdDbfs,
    });
  }

  observe(peak, valid, overloaded) {
    if (!this.reporting && this.gate.kind !== 'dynamic') return;
    this.observedFrames++;
    this.reportPeak = Math.max(this.reportPeak, peak);
    this.reportValid &&= valid;
    this.reportOverloaded ||= overloaded;
    if (++this.reportFrames === this.windowFrames) {
      if (this.gate.kind === 'dynamic') {
        this.observeDynamicWindow(this.reportPeak, this.reportValid, this.reportOverloaded);
        this.postDynamicStatusIfDue();
      }
      if (this.reporting) {
        this.port.postMessage({
          peak: this.reportPeak,
          frames: this.reportFrames,
          valid: this.reportValid,
          overloaded: this.reportOverloaded,
        });
      }
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
    this.resetDynamic();
    this.resetWindow();
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
    } else if (this.gate.kind === 'dynamic') {
      this.gateGain = this.dynamicGainForFrame(peak);
    }
    output[0][frame] = this.frameSample0 * this.gateGain;
    if (channels === 2) output[1][frame] = this.frameSample1 * this.gateGain;
  }

  processFrame(input, output, frame, channels) {
    let sample0 = input[0][frame];
    let sample1 = channels === 2 ? input[1][frame] : 0;
    const valid = Number.isFinite(sample0) && (channels === 1 || Number.isFinite(sample1));
    if (!valid && this.gate.kind === 'dynamic' && this.reportValid) this.resetDynamic();
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
      this.resetDynamic();
      this.resetWindow();
    }

    for (let frame = 0; frame < frames; frame++) {
      this.processFrame(input, output, frame, channels);
    }
    return true;
  }
}

registerProcessor('concord-mic-processor', ConcordMicProcessor);

export {};
