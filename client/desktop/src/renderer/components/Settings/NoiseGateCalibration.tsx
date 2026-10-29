import { useId } from 'react';
import type {
  GateCalibrationController,
  GateCalibrationState,
} from '../../hooks/device/useGateCalibration';
import type { UseMicTestReturn } from '../../hooks/device/useMicTest';
import type { CalibrationResult } from '../../services/voice/gateCalibration';

interface NoiseGateCalibrationProps {
  controller: GateCalibrationController;
  micTest: UseMicTestReturn;
  mode: 'autoCalibrate' | 'manualCalibrate';
  threshold: number;
  appliedThreshold: number;
  onThresholdChange(value: number): void;
}

const READING =
  'Soft winds whisper through the trees. Peter packs five bright blue boxes. How clearly can you hear my quiet words?';

const PHASE_MESSAGES: Record<GateCalibrationState['phase'], string> = {
  preparation: 'Ready to prepare microphone calibration.',
  acquiring: 'Requesting microphone access for silent calibration.',
  quiet: 'Measuring the room. Stay quiet with normal background sound.',
  readyToRead: 'Room measurement complete. Select Start reading when ready.',
  speech: 'Reading measurement active. Read naturally into the microphone.',
  review: 'Calibration complete. Review the suggested threshold.',
  refused: 'No reliable starting point was found.',
};

function refusalMessage(result: CalibrationResult | null): string | null {
  if (result?.kind !== 'refused') return null;
  switch (result.reason) {
    case 'overlap':
      return 'Room and voice levels overlap, so no reliable starting point was found. Move the microphone closer and try again, or set the threshold manually.';
    case 'clipping':
      return 'The microphone input reached full scale. Lower the microphone level or upstream gain, apply changes, and try again.';
    case 'silent-reference':
      return 'The room measurement was digital silence, so there is no measured ambient reference. Check upstream filtering and try again with normal room sound.';
    case 'insufficient':
      return 'Not enough useful microphone observations were captured. Try again and read naturally, including softer speech.';
    case 'invalid':
      return 'The microphone measurement was interrupted or invalid. Check your microphone and try again.';
  }
}

function levelText(value: number | null): string {
  if (value === null || !Number.isFinite(value)) return 'Digital silence / no current reading';
  return `${Number(value.toFixed(1))} dBFS`;
}

/** Clamp drawing positions only; measured values retain their full numeric range. */
function meterPosition(value: number | null): number {
  return value === null ? 0 : Math.min(100, Math.max(0, ((value + 80) / 80) * 100));
}

interface CalibrationMeterProps {
  level: number | null;
  threshold: number;
  suggestion: number | null;
}

function CalibrationMeter({ level, threshold, suggestion }: Readonly<CalibrationMeterProps>) {
  return (
    <div className="settings-calibration-meter">
      <div className="settings-calibration-meter-track">
        <meter
          aria-label="Microphone level"
          min={level === null ? -80 : Math.min(-80, level)}
          max={level === null ? 0 : Math.max(0, level)}
          value={level ?? -80}
          aria-valuetext={levelText(level)}
          className="settings-calibration-meter-native"
        />
        <span
          aria-hidden="true"
          className="settings-calibration-marker settings-calibration-marker-current"
          style={{ left: `${meterPosition(threshold)}%` }}
        >
          ▼
        </span>
        {suggestion !== null && (
          <span
            aria-hidden="true"
            className="settings-calibration-marker settings-calibration-marker-suggested"
            style={{ left: `${meterPosition(suggestion)}%` }}
          >
            ▲
          </span>
        )}
      </div>
      <div className="settings-calibration-meter-scale" aria-hidden="true">
        <span>−80</span>
        <span>−60</span>
        <span>−40</span>
        <span>−20</span>
        <span>0 dBFS</span>
      </div>
      <p>Microphone level: {levelText(level)}</p>
      <p>▼ Current threshold: {threshold} dBFS</p>
      {suggestion !== null && <p>▲ Suggested threshold: {suggestion} dBFS</p>}
      <p className="settings-row-hint">
        Digital sample peaks before the noise gate, after microphone processing and level. These are
        not sound-pressure measurements.
      </p>
    </div>
  );
}

export function NoiseGateCalibration({
  controller,
  micTest,
  mode,
  threshold,
  appliedThreshold,
  onThresholdChange,
}: Readonly<NoiseGateCalibrationProps>) {
  const sliderId = useId();
  const titleId = useId();
  const { state } = controller;
  const { phase, result } = state;
  const capturing = ['acquiring', 'quiet', 'readyToRead', 'speech'].includes(phase);
  const reviewing = phase === 'review' || phase === 'refused';
  const refusal = refusalMessage(result);
  const summary = reviewing ? result?.summary : null;
  const suggestion = result?.kind === 'recommended' ? result.thresholdDbfs : null;
  let level = state.meterDbfs;
  if (!capturing) {
    level = micTest.isTesting && Number.isFinite(micTest.dbfsLevel) ? micTest.dbfsLevel : null;
  }

  return (
    <section className="settings-calibration" aria-labelledby={titleId}>
      <h3 id={titleId} className="settings-subsection-title">
        {mode === 'autoCalibrate' ? 'Auto Calibrate' : 'Manual Calibrate'}
      </h3>
      <p role="status" aria-live="polite" aria-atomic="true">
        {state.message ?? refusal ?? PHASE_MESSAGES[phase]}
      </p>

      {phase === 'preparation' && (
        <div className="settings-calibration-preparation">
          <p>Choose and apply Concord and external filtering before calibration.</p>
          <p>Keep ordinary background sound, such as a fan, running.</p>
          <p>
            Position your microphone normally and speak from your farthest normal distance and
            orientation.
          </p>
          <p>
            Retest after changing room noise, hardware, microphone position, processing, or level.
          </p>
          <p className="settings-row-hint">
            Calibration silently samples the microphone without playback. For ordinary listening,
            use the Input Test above with headphones.
          </p>
          {!state.canStart && !state.message && (
            <p>Apply microphone processing and level changes, and turn off automatic gain first.</p>
          )}
        </div>
      )}

      {capturing && <p>Microphone active</p>}
      {phase === 'quiet' && <p>{state.secondsRemaining} seconds of room capture remaining</p>}
      {(phase === 'readyToRead' || phase === 'speech') && (
        <div className="settings-calibration-reading">
          <p>{READING}</p>
          <p className="settings-row-hint">
            Use comfortable lower and higher tones and softer natural speech. Captured time is a
            data requirement; it does not mean speech was recognized.
          </p>
          {phase === 'speech' && (
            <p>{state.secondsRemaining} seconds of reading capture remaining</p>
          )}
        </div>
      )}
      {reviewing && <p>Microphone capture stopped.</p>}
      <CalibrationMeter level={level} threshold={threshold} suggestion={suggestion} />
      {micTest.isTesting && micTest.inputOverloaded && (
        <p role="alert">
          Input reached or exceeded 0 dBFS before the noise gate. This may indicate clipping; check
          your microphone level and upstream audio settings.
        </p>
      )}

      {summary && (
        <dl className="settings-calibration-summary">
          <div>
            <dt>Ambient room P95 — 95% of nonzero room peaks were at or below this level</dt>
            <dd>{levelText(summary.noiseP95Dbfs)}</dd>
          </div>
          <div>
            <dt>Voice-phase P10 — 10% of nonzero reading peaks were at or below this level</dt>
            <dd>{levelText(summary.speechP10Dbfs)}</dd>
          </div>
          <div>
            <dt>Voice-phase P90 — 90% of nonzero reading peaks were at or below this level</dt>
            <dd>{levelText(summary.speechP90Dbfs)}</dd>
          </div>
          <div>
            <dt>Digital silence — exact-zero windows in room / reading capture</dt>
            <dd>
              {Math.round(summary.quietZeroFraction * 100)}% /{' '}
              {Math.round(summary.speechZeroFraction * 100)}%
            </dd>
          </div>
        </dl>
      )}
      {reviewing && (
        <p className="settings-row-hint">
          This is an environment-specific starting point. A later quieter word or louder background
          sound may cross it. Amplitude cannot identify speech or guarantee that every word passes.
          Noisy pauses can prevent a recommendation. You can set the threshold manually or use Off.
        </p>
      )}

      <div className="settings-calibration-actions">
        {phase === 'preparation' && (
          <button type="button" onClick={() => void controller.start()} disabled={!state.canStart}>
            {mode === 'autoCalibrate' ? 'Start calibration' : 'Suggest a starting point'}
          </button>
        )}
        {phase === 'readyToRead' && (
          <button type="button" onClick={controller.startReading}>
            Start reading
          </button>
        )}
        {phase === 'speech' && (
          <button
            type="button"
            onClick={controller.finishReading}
            disabled={!state.canFinishReading}
          >
            Done reading
          </button>
        )}
        {reviewing && (
          <>
            {suggestion !== null && (
              <button type="button" onClick={controller.useThreshold}>
                Use threshold
              </button>
            )}
            <button
              type="button"
              onClick={() => void controller.start()}
              disabled={!state.canStart}
            >
              Retry
            </button>
          </>
        )}
        {(capturing || reviewing) && (
          <button key="cancel" type="button" onClick={controller.cancel}>
            Cancel calibration
          </button>
        )}
      </div>

      <div className="settings-row settings-row-child settings-calibration-threshold">
        <div className="settings-row-info">
          <label className="settings-row-label" htmlFor={sliderId}>
            Gate Threshold
          </label>
          <span className="settings-row-hint">Set the threshold yourself.</span>
        </div>
        <div className="settings-slider-wrapper">
          <span className="settings-slider-value">{threshold} dBFS</span>
          <input
            id={sliderId}
            type="range"
            className="settings-slider"
            min={-80}
            max={-20}
            step={1}
            value={threshold}
            onChange={(event) => onThresholdChange(Number(event.currentTarget.value))}
          />
        </div>
      </div>
      <p>Applied threshold: {appliedThreshold} dBFS</p>
      {threshold !== appliedThreshold && <p>Pending threshold: {threshold} dBFS</p>}
      <p className="settings-row-hint">
        Use threshold adds the suggestion to pending settings. Select Apply to hear changes take
        effect; Revert restores the applied settings.
      </p>
    </section>
  );
}
