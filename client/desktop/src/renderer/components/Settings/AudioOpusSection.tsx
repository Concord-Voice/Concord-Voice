import { useState } from 'react';
import ToggleSwitch from './ToggleSwitch';
import CustomSelect from '../ui/CustomSelect';
import PremiumGate from '../common/PremiumGate';
import PremiumChip from '../common/PremiumChip';
import { useGateActivation } from '../../hooks/useGateActivation';
import { AUDIO_QUALITY_TIERS, type AudioQualityTier } from '../../stores/voiceStore';
import { type AudioPriority } from '../../stores/audioSettingsStore';
import { useEntitlement } from '../../hooks/useEntitlement';
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

  // Premium gates (#1301):
  //  - L3 Music Mode: binary lock on `allowMusicMode`. Intercepting `dim` gate.
  //  - L2 Frame Size (ptime): the 10 ms option is premium — free floor
  //    `minPtimeMs` is 20, so any ptime BELOW the floor is paid. The select
  //    stays usable for the free options; picking the locked 10 ms snaps back
  //    to the highest free value (20 ms) and surfaces the chip.
  const allowMusicMode = useEntitlement((e) => e.allowMusicMode);
  const minPtimeMs = useEntitlement((e) => e.minPtimeMs);
  const ptimeGate = useGateActivation('audio-tier');
  // Show the snap-back chip after a locked ptime pick. Local UI state only.
  const [ptimeLockHinted, setPtimeLockHinted] = useState(false);

  const tierConfig = AUDIO_QUALITY_TIERS[qualityTier];

  /** The frame-size options, each tagged premium when its ptime is below the
   *  free floor (`minPtimeMs`). `0` (tier default) and values ≥ floor are free. */
  const frameSizeOptions: { value: string; label: string; premium: boolean }[] = [
    { value: '0', label: `Default (${tierConfig.preferredFrameSize} ms)`, premium: false },
    { value: '10', label: '10 ms (lowest latency)', premium: 10 < minPtimeMs },
    { value: '20', label: '20 ms', premium: 20 < minPtimeMs },
    { value: '40', label: '40 ms (efficient)', premium: 40 < minPtimeMs },
    { value: '60', label: '60 ms (most efficient)', premium: 60 < minPtimeMs },
  ];
  const HIGHEST_FREE_PTIME = 20; // the free-tier floor value the snap-back lands on

  const handleFrameSizeChange = (raw: string): void => {
    const chosen = frameSizeOptions.find((o) => o.value === raw);
    if (chosen?.premium) {
      // L2 snap-back: a locked ptime never reaches the store. Clamp to the
      // highest free value and reveal the chip (no mid-action modal).
      setDraftAudioSetting('frameSize', HIGHEST_FREE_PTIME);
      setPtimeLockHinted(true);
      return;
    }
    setPtimeLockHinted(false);
    setDraftAudioSetting('frameSize', Number(raw) as 0 | 10 | 20 | 40 | 60);
  };

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
        <PremiumGate
          mode="dim"
          entitled={allowMusicMode}
          feature="musicMode"
          onActivateSection="music-mode"
        >
          <ToggleSwitch
            checked={musicMode}
            onChange={(v) => setDraftAudioSetting('musicMode', v)}
          />
        </PremiumGate>
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
          {ptimeLockHinted && (
            <span className="settings-row-premium-note">
              <PremiumChip
                label="lower latency"
                onActivate={ptimeGate.onActivate}
                id={ptimeGate.describedById}
              />
            </span>
          )}
        </div>
        <CustomSelect
          className="settings-select"
          // L2: premium ptime options carry a trailing 🔒 + "Premium" marker in
          // their label. The select stays usable; selecting one snaps back.
          options={frameSizeOptions.map((o) => ({
            value: o.value,
            label: o.premium ? `${o.label} \u{1F512} Premium` : o.label,
          }))}
          value={String(adaptivePtime ? 0 : frameSize)}
          onChange={handleFrameSizeChange}
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
