import type { MessageWithUser } from '../../types/chat';
import { e2eeService } from '../../services/e2eeService';
import { unwrapGifEnvelope } from '../../utils/gifEnvelope';
import MarkdownContent from '../Markdown/MarkdownContent';
import AttachmentDisplay from './AttachmentDisplay';
import GifEmbed from './GifEmbed';
import type { MentionLookup } from './messageUtils';

export type DecryptedPin = MessageWithUser & { decrypted?: boolean; decryptFailed?: boolean };

// Empty mention lookup — pinned messages render mentions as plain `@text`.
// Tying this to the live member/role stores would couple the panel to
// per-channel render context that isn't load-bearing for pin display.
const EMPTY_MENTION_LOOKUP: MentionLookup = {
  users: new Map<string, string>(),
  roles: new Map<string, string>(),
};

export interface PinContentProps {
  readonly message: DecryptedPin;
}

/** Render a pinned message body using the same render pipeline as the main
 *  chat surface: MarkdownContent for text, GifEmbed for KLIPY embeds, and
 *  AttachmentDisplay for image/video/audio/file attachments. */
export function PinContent({ message }: PinContentProps) {
  if (message.decryptFailed) {
    return <span className="pinned-message-encrypted">Unable to decrypt</span>;
  }
  if (!message.decrypted) {
    return <span className="pinned-message-encrypted">Encrypted message</span>;
  }
  const { id, content, gif_slug, attachments, edited_at, channel_id } = message;
  return (
    <>
      {content && (
        <MarkdownContent
          id={id}
          content={content}
          editedAt={edited_at ?? null}
          mentionLookup={EMPTY_MENTION_LOOKUP}
        />
      )}
      {gif_slug && <GifEmbed slug={gif_slug} reduceMotion={false} loadAutomatically={true} />}
      {attachments && attachments.length > 0 && (
        <AttachmentDisplay attachments={attachments} channelId={channel_id} messageBody={content} />
      )}
    </>
  );
}

/** Decrypt pinned messages client-side using the channel's E2EE keys.
 *  Applies `unwrapGifEnvelope` so GIF messages surface their `gif_slug`
 *  field rather than the raw JSON envelope. */
export async function decryptPins(
  contextId: string,
  msgs: MessageWithUser[]
): Promise<DecryptedPin[]> {
  if (!e2eeService.isInitialized) {
    return msgs; // No decrypted flag set — PinContent will show "Encrypted message" placeholder
  }

  // Pre-fetch channel key once
  let channelKey: CryptoKey | null = null;
  try {
    channelKey = await e2eeService.getChannelKey(contextId);
  } catch {
    // Key may not be available yet
  }

  // Collect unique key versions for batch pre-fetch
  const versions = new Set<number>();
  for (const m of msgs) {
    if (m.key_version && m.key_version > 1) {
      versions.add(m.key_version);
    }
  }
  const versionedKeys = new Map<number, CryptoKey>();
  for (const v of versions) {
    try {
      versionedKeys.set(v, await e2eeService.getChannelKeyByVersion(contextId, v));
    } catch {
      // Will fall back to on-demand fetch per message
    }
  }

  return Promise.all(
    msgs.map(async (m) => {
      const message = { ...m, channel_id: m.channel_id || contextId };
      try {
        const kv = message.key_version;
        let plaintext: string;
        if (kv && kv > 1) {
          const vKey = versionedKeys.get(kv);
          plaintext = vKey
            ? await e2eeService.decryptWithKey(message.content, vKey)
            : await e2eeService.decryptForChannelWithVersion(contextId, message.content, kv);
        } else {
          plaintext = channelKey
            ? await e2eeService.decryptWithKey(message.content, channelKey)
            : await e2eeService.decryptForChannel(contextId, message.content);
        }
        const { text, gifSlug } = unwrapGifEnvelope(plaintext);
        return {
          ...message,
          content: text,
          gif_slug: gifSlug ?? message.gif_slug,
          decrypted: true,
        };
      } catch {
        return { ...message, content: '', decryptFailed: true };
      }
    })
  );
}
