import React from 'react';
import DeviceConfigSection from './DeviceConfigSection';
import AudioConfigSection from './AudioConfigSection';
import VideoConfigSection from './VideoConfigSection';

const VoiceAudioSection: React.FC = () => {
  return (
    <>
      <DeviceConfigSection />
      <AudioConfigSection />
      <VideoConfigSection />
    </>
  );
};

export default VoiceAudioSection;
