/**
 * GIF envelope helpers — keep the encrypt/decrypt JSON shape in one place so
 * the receive paths (real-time WebSocket + REST history fetch) stay in sync.
 *
 * When a user attaches a GIF to an E2EE channel message we encrypt the
 * envelope `{"text":"...","gif_slug":"..."}` instead of plaintext, because
 * the server can't see encrypted content and we still want the slug to be
 * end-to-end protected. The matching unwrap below pulls the slug back out
 * after decryption so the renderer can show the inline GIF.
 */

export interface GifEnvelope {
  text: string;
  gifSlug?: string;
}

/** Parse a decrypted plaintext blob, returning the GIF envelope contents if
 *  the blob looks like one. Plain-text messages round-trip unchanged. */
export function unwrapGifEnvelope(plaintext: string): GifEnvelope {
  // Cheap fast path — only attempt JSON.parse on strings that could be the
  // envelope object. Avoids spamming SyntaxError on every plain-text message.
  if (!plaintext.startsWith('{')) {
    return { text: plaintext };
  }
  try {
    const parsed = JSON.parse(plaintext);
    if (parsed && typeof parsed === 'object' && typeof parsed.gif_slug === 'string') {
      return {
        text: typeof parsed.text === 'string' ? parsed.text : '',
        gifSlug: parsed.gif_slug,
      };
    }
  } catch {
    // Not JSON — treat as plain text
  }
  return { text: plaintext };
}
