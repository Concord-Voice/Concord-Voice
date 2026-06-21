import React, { useState, useEffect } from 'react';
import { Mic, Speaker, Video } from 'lucide-react';
import { useVoiceStore } from '../../stores/voiceStore';
import CustomSelect from '../ui/CustomSelect';
import './DeviceSelector.css';

type DeviceKind = 'audioinput' | 'audiooutput' | 'videoinput';

interface DeviceSelectorProps {
  kind: DeviceKind;
}

const LABELS: Record<DeviceKind, string> = {
  audioinput: 'Microphone',
  audiooutput: 'Speaker',
  videoinput: 'Camera',
};

const ICONS: Record<DeviceKind, React.ReactNode> = {
  audioinput: <Mic size={14} />,
  audiooutput: <Speaker size={14} />,
  videoinput: <Video size={14} />,
};

const DeviceSelector: React.FC<DeviceSelectorProps> = ({ kind }) => {
  const [devices, setDevices] = useState<MediaDeviceInfo[]>([]);
  const audioInputDeviceId = useVoiceStore((s) => s.audioInputDeviceId);
  const audioOutputDeviceId = useVoiceStore((s) => s.audioOutputDeviceId);
  const videoDeviceId = useVoiceStore((s) => s.videoDeviceId);
  const setAudioInputDevice = useVoiceStore((s) => s.setAudioInputDevice);
  const setAudioOutputDevice = useVoiceStore((s) => s.setAudioOutputDevice);
  const setVideoDevice = useVoiceStore((s) => s.setVideoDevice);

  const deviceIdByKind = {
    audioinput: audioInputDeviceId,
    audiooutput: audioOutputDeviceId,
    videoinput: videoDeviceId,
  };
  const setDeviceByKind = {
    audioinput: setAudioInputDevice,
    audiooutput: setAudioOutputDevice,
    videoinput: setVideoDevice,
  };
  const currentDeviceId = deviceIdByKind[kind];
  const setDevice = setDeviceByKind[kind];

  useEffect(() => {
    const enumerate = async () => {
      try {
        const allDevices = await navigator.mediaDevices.enumerateDevices();
        setDevices(allDevices.filter((d) => d.kind === kind));
      } catch {
        // Permission denied or not available
      }
    };

    enumerate();

    // Re-enumerate when devices change (plug/unplug)
    navigator.mediaDevices.addEventListener('devicechange', enumerate);
    return () => navigator.mediaDevices.removeEventListener('devicechange', enumerate);
  }, [kind]);

  return (
    <div className="device-selector">
      <label htmlFor={`device-${kind}`} className="device-selector__label">
        {ICONS[kind]}
        {LABELS[kind]}
      </label>
      <CustomSelect
        id={`device-${kind}`}
        className="device-selector__select"
        options={[
          { value: '', label: 'Default' },
          ...devices.map((d) => ({
            value: d.deviceId,
            label: d.label || `${LABELS[kind]} ${d.deviceId.slice(0, 8)}`,
          })),
        ]}
        value={currentDeviceId || ''}
        onChange={(v) => setDevice(v)}
      />
    </div>
  );
};

export default DeviceSelector;
