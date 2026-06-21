import React from 'react';
import { Hash, Volume2, Pin } from 'lucide-react';

export const NAME_MIN = 3;
export const NAME_MAX = 100;

export function validateChannelName(name: string): string | undefined {
  const trimmed = name.trim();
  if (!trimmed) {
    return 'Channel name is required';
  }
  if (trimmed.length < NAME_MIN) {
    return `Channel name must be at least ${NAME_MIN} characters`;
  }
  if (trimmed.length > NAME_MAX) {
    return `Channel name must be at most ${NAME_MAX} characters`;
  }
  return undefined;
}

export function getChannelTypeIcon(channelType: 'text' | 'voice' | 'bulletin'): React.ReactNode {
  switch (channelType) {
    case 'text':
      return <Hash size={20} />;
    case 'voice':
      return <Volume2 size={20} />;
    case 'bulletin':
      return <Pin size={20} />;
    default:
      return <Hash size={20} />;
  }
}
