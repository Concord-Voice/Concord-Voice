//
// Seam selector for the NSFW gate's "already satisfied" short-circuit (#1625).
// No producer of a prior age-assurance signal exists yet: there is no client-readable
// nsfw_auth state (no GET status endpoint; UserProfile has no age field). This hook is
// the SINGLE integration point that lights up when the SSO assurance path (#1626) or a
// future status endpoint lands — at which point this body reads that source instead of
// returning the inert default. It is a wired-but-currently-inert branch, NOT dead code
// (mirrors the resolveVideoPublisherCap tier-seam pattern, #1294).
// eslint-disable-next-line @eslint-react/no-unnecessary-use-prefix -- intentional hook seam: the `use` prefix is load-bearing because this WILL call a store/context hook once the SSO-assurance producer (#1626) or a status endpoint lands; renaming now would churn every call site back later (#1625).
export function useAgeStatus(): { nsfwAuth: boolean | 'unknown' } {
  return { nsfwAuth: 'unknown' };
}
