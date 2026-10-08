import { useMFAChallengeStore } from '../../src/renderer/stores/auth/mfaChallengeStore';
import { recordChallengeIssuer } from '../../src/renderer/services/system/challengeIssuer';
import { captureRuntimeServerSelection } from '../../src/renderer/services/system/runtimeServerBase';

/**
 * Records the current server selection as the issuer of every challenge token
 * the store publishes, before the modal renders it. It stands in for
 * useSSOFlow and apiClient, which record the issuer just before they publish a
 * challenge, in tests that publish challenges through the store directly.
 * Returns the unsubscribe. A test about a challenge with no recorded issuer
 * must not install it.
 */
export function recordIssuerOnPublish(): () => void {
  return useMFAChallengeStore.subscribe((state, prev) => {
    const token = state.challengeToken;
    if (token && token !== prev.challengeToken) {
      recordChallengeIssuer(token, captureRuntimeServerSelection());
    }
  });
}
