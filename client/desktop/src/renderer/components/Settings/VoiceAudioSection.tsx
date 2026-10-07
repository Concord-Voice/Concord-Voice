import React from 'react';
import AudioConfigSection from './AudioConfigSection';
import VideoConfigSection from './VideoConfigSection';

const VoiceAudioSection: React.FC = () => {
  return (
    <>
      <AudioConfigSection />
      <VideoConfigSection />
    </>
  );
};

export default VoiceAudioSection;
