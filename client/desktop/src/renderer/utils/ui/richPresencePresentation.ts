import type { PresenceStatus } from '../../stores/chat/memberStore';
import type {
  LocalRichPresenceActivity,
  OtherPresenceByUser,
  PresenceSettings,
} from '../../stores/ui/richPresenceStore';

export type PresentedRichPresence =
  | {
      category: 'server_voice';
      label: 'Server Voice';
      headline: 'In voice';
      detail?: string;
      minimized: boolean;
    }
  | {
      category: 'private_call';
      label: 'Private Call';
      headline: 'In a private call' | 'In a group call';
      detail?: string;
      minimized: boolean;
    }
  | {
      category: 'custom_text';
      label: 'Custom Status';
      headline: string;
      emoji?: string;
      minimized: false;
    };

export function presentRemoteActivities(
  entries: OtherPresenceByUser[string] | undefined
): readonly PresentedRichPresence[] {
  const activities: PresentedRichPresence[] = [];
  const serverVoice = entries?.server_voice;
  if (serverVoice?.category === 'server_voice') {
    const activity: PresentedRichPresence = {
      category: 'server_voice',
      label: 'Server Voice',
      headline: 'In voice',
      minimized: serverVoice.minimized,
    };
    if (
      !serverVoice.minimized &&
      serverVoice.payload.channel_name !== undefined &&
      serverVoice.payload.server_name !== undefined
    ) {
      activity.detail = `${serverVoice.payload.channel_name} · ${serverVoice.payload.server_name}`;
    }
    activities.push(activity);
  }

  const privateCall = entries?.private_call;
  if (privateCall?.category === 'private_call') {
    const activity: PresentedRichPresence = {
      category: 'private_call',
      label: 'Private Call',
      headline: privateCall.payload.call_type === 'group' ? 'In a group call' : 'In a private call',
      minimized: privateCall.minimized,
    };
    if (!privateCall.minimized && privateCall.payload.participant_count !== undefined) {
      activity.detail = `With ${privateCall.payload.participant_count} people`;
    }
    activities.push(activity);
  }

  const customText = entries?.custom_text;
  if (customText?.category === 'custom_text') {
    activities.push({
      category: 'custom_text',
      label: 'Custom Status',
      headline: customText.payload.text,
      ...(customText.payload.emoji === undefined ? {} : { emoji: customText.payload.emoji }),
      minimized: false,
    });
  }

  return activities;
}

export function getPresenceActivityAudience(
  category: 'server_voice' | 'private_call',
  settings: Pick<PresenceSettings, 'masterEnabled' | 'serverVoiceTier' | 'privateCallTier'>
): string {
  if (!settings.masterEnabled) return 'Nobody';

  if (category === 'server_voice') {
    if (settings.serverVoiceTier === 1) {
      return 'Friends—and eligible friends-of-friends—who are in this server and can view this voice channel.';
    }
    if (settings.serverVoiceTier === 2)
      return 'People in this server who can view this voice channel.';
    return 'Nobody';
  }

  if (settings.privateCallTier === 1) {
    return 'People currently in this call, plus your friends and eligible friends-of-friends.';
  }
  if (settings.privateCallTier === 2) {
    return 'People currently in this call, plus your friends, eligible friends-of-friends, and people who share a server with you.';
  }
  return 'People currently in this private call.';
}

export function presentSelfActivity(
  activity: LocalRichPresenceActivity | null,
  settings: PresenceSettings | null,
  status: PresenceStatus
): {
  headline: 'In voice' | 'In a private call' | 'In a group call';
  eligibility: string;
  deliveryNote?: 'Not currently shared while Invisible' | 'Not currently shared while Offline';
} | null {
  if (activity === null) return null;

  let headline: 'In voice' | 'In a private call' | 'In a group call';
  if (activity.category === 'server_voice') {
    headline = 'In voice';
  } else if (activity.callType === 'group') {
    headline = 'In a group call';
  } else {
    headline = 'In a private call';
  }
  const eligibility = settings
    ? getPresenceActivityAudience(activity.category, settings)
    : 'Audience unavailable';
  let deliveryNote:
    'Not currently shared while Invisible' | 'Not currently shared while Offline' | undefined;
  if (status === 'invisible') {
    deliveryNote = 'Not currently shared while Invisible';
  } else if (status === 'offline') {
    deliveryNote = 'Not currently shared while Offline';
  }

  return { headline, eligibility, ...(deliveryNote === undefined ? {} : { deliveryNote }) };
}
