import ToggleSwitch from './ToggleSwitch';
import CustomSelect from '../ui/CustomSelect';
import { AUDIO_QUALITY_TIERS, type AudioQualityTier } from '../../stores/voiceStore';
import { type AudioPriority } from '../../stores/audioSettingsStore';
import { useDraftAudioSetting, setDraftAudioSetting } from '../../hooks/useDraftSettings';

// ─── Hint Helpers ────────────────────────────────────────────────────────────

/** Describe frame size setting for the hint text. */
export function frameSizeHint(
  adaptivePtime: boolean,
  frameSize: number,
  preferredFrameSize: number
): string {
  if (adaptivePtime)
    return 'Locked by Adaptive Frame Size. Manual frame size is overridden while adaptive sizing is active.';
  if (frameSize === 0)
    return `Packet duration sent to other participants. Currently using the tier's preferred size (${preferredFrameSize}ms).`;
  if (frameSize === 10)
    return 'Packet duration sent to other participants. Currently 10ms \u2014 lowest latency, highest overhead.';
  if (frameSize === 20)
    return 'Packet duration sent to other participants. Currently 20ms \u2014 standard balance of latency and efficiency.';
  if (frameSize === 40)
    return 'Packet duration sent to other participants. Currently 40ms \u2014 reduced overhead, moderately increased latency.';
  return 'Packet duration sent to other participants. Currently 60ms \u2014 maximum efficiency, highest latency.';
}

/** Describe QoS priority setting for the hint text. */
export function qosPriorityHint(priority: string): string {
  if (priority === 'off') return 'Currently off \u2014 no tagging applied.';
  if (priority === 'low') return 'Currently Low (DF) \u2014 minimal differentiation. (RFC 2474)';
  if (priority === 'medium')
    return 'Currently Default (AF41) \u2014 recommended for most networks. (RFC 2597)';
  return 'Currently High (EF) \u2014 highest priority. (RFC 5127)';
}

// ─── Component ───────────────────────────────────────────────────────────────

interface AudioOpusSectionProps {
  qualityTier: AudioQualityTier;
}

const AudioOpusSection: React.FC<AudioOpusSectionProps> = ({ qualityTier }) => {
  const musicMode = useDraftAudioSetting('musicMode');
  const frameSize = useDraftAudioSetting('frameSize');
  const silenceDetection = useDraftAudioSetting('silenceDetection');
  const inlineFec = useDraftAudioSetting('inlineFec');
  const fecHeadroom = useDraftAudioSetting('fecHeadroom');
  const opusNack = useDraftAudioSetting('opusNack');
  const adaptivePtime = useDraftAudioSetting('adaptivePtime');
  const audioPriority = useDraftAudioSetting('audioPriority');
  const stereoOverride = useDraftAudioSetting('stereoOverride');

  const tierConfig = AUDIO_QUALITY_TIERS[qualityTier];

  return (
    <>
      <h3 className="settings-subsection-title">Opus Codec</h3>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Music Mode</span>
          <span className="settings-row-hint">
            {musicMode
              ? 'Enabled. Noise suppression, echo cancellation, and auto gain control are bypassed. The Opus codec uses full 48 kHz bandwidth optimized for music fidelity.'
              : 'Disabled. Standard voice processing is active with voice-optimized encoding.'}
          </span>
        </div>
        <ToggleSwitch checked={musicMode} onChange={(v) => setDraftAudioSetting('musicMode', v)} />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Adaptive Frame Size (AFS)</span>
          <span className="settings-row-hint">
            {adaptivePtime
              ? 'Enabled. Concord Voice dynamically adjusts packet duration during congestion, trading latency for reliability. Manual Frame Size is overridden.'
              : 'Disabled. Packet duration is fixed at the configured Frame Size value below.'}
          </span>
        </div>
        <ToggleSwitch
          checked={adaptivePtime}
          onChange={(v) => setDraftAudioSetting('adaptivePtime', v)}
        />
      </div>

      <div className={`settings-row ${adaptivePtime ? 'settings-row-disabled' : ''}`}>
        <div className="settings-row-info">
          <span className="settings-row-label">Frame Size (ptime)</span>
          <span className="settings-row-hint">
            {frameSizeHint(adaptivePtime, frameSize, tierConfig.preferredFrameSize)}
          </span>
        </div>
        <CustomSelect
          className="settings-select"
          options={[
            { value: '0', label: `Default (${tierConfig.preferredFrameSize} ms)` },
            { value: '10', label: '10 ms (lowest latency)' },
            { value: '20', label: '20 ms' },
            { value: '40', label: '40 ms (efficient)' },
            { value: '60', label: '60 ms (most efficient)' },
          ]}
          value={String(adaptivePtime ? 0 : frameSize)}
          onChange={(v) => setDraftAudioSetting('frameSize', Number(v) as 0 | 10 | 20 | 40 | 60)}
          disabled={adaptivePtime}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Silence Detection (DTX)</span>
          <span className="settings-row-hint">
            {silenceDetection
              ? 'Enabled. The Opus codec uses discontinuous transmission to send fewer packets during silence, reducing bandwidth usage.'
              : 'Disabled. Packets are sent continuously regardless of input activity. Uses more bandwidth but avoids transition artifacts.'}
          </span>
        </div>
        <ToggleSwitch
          checked={silenceDetection}
          onChange={(v) => setDraftAudioSetting('silenceDetection', v)}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Mic Channel Mode</span>
          <span className="settings-row-hint">
            {(stereoOverride ?? tierConfig.opusStereo)
              ? 'Currently Stereo \u2014 Opus encodes two channels using mid-side (M/S) joint stereo, splitting audio into a summed Mid channel and a difference Side channel. When channels are similar, the Side channel compresses to near-nothing, so stereo does not double bandwidth. Recommended for multi-microphone setups, Hi-Fi, or studio-grade configurations.'
              : 'Currently Mono \u2014 a single audio channel is encoded and delivered. Recommended for most users, especially with standard single-microphone headsets and desk mics.'}
          </span>
        </div>
        <CustomSelect
          className="settings-select"
          options={[
            { value: 'stereo', label: 'Stereo' },
            { value: 'mono', label: 'Mono' },
          ]}
          value={(stereoOverride ?? tierConfig.opusStereo) ? 'stereo' : 'mono'}
          onChange={(v) => setDraftAudioSetting('stereoOverride', v === 'stereo')}
        />
      </div>

      {/* ── Error Correction & Reliability ── */}
      <h3 className="settings-subsection-title">Error Correction & Reliability</h3>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">In-Line Forward Error Correction (FEC)</span>
          <span className="settings-row-hint">
            {inlineFec
              ? 'Enabled. Every packet includes a low-quality copy of the previous frame as a safety net. The receiver uses it to fill gaps when a packet goes missing. VBR keeps the overhead minimal on easy frames.'
              : 'Disabled. Packets carry only primary audio with no redundancy. Maximizes quality per-packet, but the receiver has nothing to fall back on during packet loss.'}
          </span>
        </div>
        <ToggleSwitch checked={inlineFec} onChange={(v) => setDraftAudioSetting('inlineFec', v)} />
      </div>

      {inlineFec && (
        <div className="settings-row">
          <div className="settings-row-info">
            <span className="settings-row-label">Grant FEC Headroom</span>
            <span className="settings-row-hint">
              {fecHeadroom
                ? 'Enabled. When packet loss is detected, the bitrate ceiling is raised so the always-present FEC payload does not eat into primary audio quality.'
                : "Disabled. The bitrate ceiling stays fixed at the tier's base rate. Since FEC is always encoded, it competes with primary audio for the same budget on complex frames."}
            </span>
          </div>
          <ToggleSwitch
            checked={fecHeadroom}
            onChange={(v) => setDraftAudioSetting('fecHeadroom', v)}
          />
        </div>
      )}

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">NACK (Retransmission)</span>
          <span className="settings-row-hint">
            {opusNack
              ? 'Enabled. Concord requests retransmission of lost audio packets from the server. Improves reliability on lossy networks at the cost of added latency.'
              : 'Disabled. Lost packets are not retransmitted. Relies on FEC or interpolation to handle gaps.'}
          </span>
        </div>
        <ToggleSwitch checked={opusNack} onChange={(v) => setDraftAudioSetting('opusNack', v)} />
      </div>

      {/* ── Transport ── */}
      <h3 className="settings-subsection-title">Transport</h3>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Quality of Service for Audio</span>
          <span className="settings-row-hint">
            {
              'Marks audio traffic with DSCP tags so your network can prioritize it. Not all networks honor DSCP tags; some may ignore or strip them, so this setting may have no effect depending on you or your ISP\u2019s network configurations. '
            }
            {qosPriorityHint(audioPriority)}
          </span>
        </div>
        <CustomSelect
          className="settings-select"
          options={[
            { value: 'off', label: 'Off (No Tagging)' },
            { value: 'low', label: 'Low (DF)' },
            { value: 'medium', label: 'Default (AF41)' },
            { value: 'high', label: 'High (EF)' },
          ]}
          value={audioPriority}
          onChange={(v) => setDraftAudioSetting('audioPriority', v as AudioPriority)}
        />
      </div>
    </>
  );
};

export default AudioOpusSection;
