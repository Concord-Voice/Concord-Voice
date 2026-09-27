/**
 * Serialize server moderation work for one participant across the mute and
 * deafen subjects. Callers own validation and error handling.
 */
export function enqueueServerEnforcement(
  chains: Map<string, Promise<void>>,
  channelId: string,
  userId: string,
  apply: () => Promise<void>
): Promise<void> {
  const key = `${channelId}:${userId}`;
  const prior = chains.get(key) ?? Promise.resolve();
  const next = prior.catch(() => undefined).then(apply);
  chains.set(key, next);
  return next.finally(() => {
    if (chains.get(key) === next) chains.delete(key);
  });
}
