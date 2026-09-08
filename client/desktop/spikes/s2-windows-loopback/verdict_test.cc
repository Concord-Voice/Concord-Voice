// Native test for the s2 probe's decision layer.
//
// The probe itself cannot run anywhere but Windows, so until this existed NOTHING
// about its verdicts had ever been EXECUTED — they were only ever compiled and
// reasoned about, and the reasoning was corrected four separate times during review
// of PR #3154. Eight findings landed inside classify() and the pair verdict, every
// one of them a state that LOOKED like silence and was not.
//
// Build and run anywhere:
//   c++ -std=c++17 -Wall -Wextra -Werror verdict_test.cc -o /tmp/verdict_test && /tmp/verdict_test

#include "verdict.h"

#include <cstdio>
#include <cstdlib>

namespace {

int g_checks = 0;
#define CHECK(cond)                                                            \
  do {                                                                         \
    ++g_checks;                                                                \
    if (!(cond)) {                                                             \
      std::fprintf(stderr, "FAIL %s:%d  %s\n", __FILE__, __LINE__, #cond);     \
      std::abort();                                                            \
    }                                                                          \
  } while (0)

const char* name(PassOutcome o) {
  switch (o) {
    case PassOutcome::ActivationFailed: return "ActivationFailed";
    case PassOutcome::InitializeFailed: return "InitializeFailed";
    case PassOutcome::SetupFailed:      return "SetupFailed";
    case PassOutcome::CaptureError:     return "CaptureError";
    case PassOutcome::NoData:           return "NoData";
    case PassOutcome::Truncated:        return "Truncated";
    case PassOutcome::Discontinuous:    return "Discontinuous";
    case PassOutcome::Silent:           return "Silent";
    case PassOutcome::Audio:            return "Audio";
  }
  return "?";
}

// A pass that got all the way through with audible samples. Each case below starts
// from this and breaks exactly one thing, so the table reads as "what goes wrong".
CaptureResult healthy() {
  CaptureResult r;
  r.activated       = true;
  r.initialized     = true;
  r.ladderComplete  = true;
  r.captured        = true;
  r.captureError    = S_OK;
  r.framesSeen      = 48000;
  r.framesExpected  = 48000;
  r.peakAbs         = 1000;
  r.discontinuities = 0;
  return r;
}

void expect(const char* what, const CaptureResult& r, PassOutcome want) {
  const PassOutcome got = classify(r);
  ++g_checks;
  if (got != want) {
    std::fprintf(stderr, "FAIL %-46s expected %s, got %s\n", what, name(want), name(got));
    std::abort();
  }
  std::printf("  %-46s %s\n", what, name(got));
}

void testEveryOutcomeIsReachableAndDistinct() {
  std::printf("classify() — one case per outcome:\n");

  CaptureResult r = healthy(); r.activated = false;
  expect("never activated", r, PassOutcome::ActivationFailed);

  // The mid-ladder case. This was misreported as InitializeFailed — "every format
  // was rejected" — on the strength of however many rungs ran before it.
  r = healthy(); r.initialized = false; r.ladderComplete = false;
  expect("activation failed PART WAY THROUGH the ladder", r, PassOutcome::ActivationFailed);

  r = healthy(); r.initialized = false; r.ladderComplete = true;
  expect("ladder exhausted, no format accepted", r, PassOutcome::InitializeFailed);

  // SetEventHandle / GetService / Start. Once reported as "the stream started and
  // not one packet arrived" — false, and it buried the responsible HRESULT.
  r = healthy(); r.captured = false; r.framesSeen = 0;
  expect("capture setup failed, stream never ran", r, PassOutcome::SetupFailed);

  r = healthy(); r.captureError = (HRESULT)0x88890004;
  expect("read failed part way through", r, PassOutcome::CaptureError);

  // Once classified Silent, which against an audible control printed RISK 2.
  r = healthy(); r.framesSeen = 0; r.peakAbs = 0;
  expect("started, delivered no packet", r, PassOutcome::NoData);

  // Event starvation after data: one packet arrived, then the stream went quiet
  // for the rest of the interval. framesSeen > 0 made this CONCLUSIVE before.
  r = healthy(); r.framesSeen = 1000;
  expect("delivered a sliver, then stopped signalling", r, PassOutcome::Truncated);

  r = healthy(); r.peakAbs = 0; r.discontinuities = 3;
  expect("frames LOST and the remainder silent", r, PassOutcome::Discontinuous);

  r = healthy(); r.peakAbs = 0;
  expect("clean capture, every sample zero", r, PassOutcome::Silent);

  // THE PROBE'S PRINCIPAL VERDICT DEPENDS ON THIS ONE. Start() is a stream state
  // transition, so the first packet may legitimately carry DATA_DISCONTINUITY on
  // every single run. While that packet was counted, an exclude pass that captured
  // nothing classified Discontinuous instead of Silent -- and conclusive() is false
  // for Discontinuous, so the include-audio / exclude-silent pair this program
  // exists to produce was unreachable. Raised by Codex on PR #3154.
  r = healthy(); r.peakAbs = 0; r.firstPacketDiscontinuity = true;
  expect("startup marker only, remainder silent", r, PassOutcome::Silent);

  // ...and it must not rescue a pass that lost frames LATER, either.
  r = healthy(); r.peakAbs = 0; r.firstPacketDiscontinuity = true; r.discontinuities = 1;
  expect("startup marker AND a real mid-capture gap", r, PassOutcome::Discontinuous);

  r = healthy();
  expect("clean capture with audible samples", r, PassOutcome::Audio);
}

// Hearing the target is conclusive; not hearing it is not. A dropout must never
// turn a genuine positive into a re-run.
void testAudioSurvivesADropout() {
  CaptureResult r = healthy();
  r.discontinuities = 99;
  CHECK(classify(r) == PassOutcome::Audio);
  CHECK(conclusive(classify(r)));
}

// THE INVARIANT THE WHOLE DESIGN RESTS ON. Every false "RISK 2 SUSPECTED" in this
// PR came from a non-completed pass being treated as an observation. Swept
// exhaustively rather than by example: if conclusive() ever admits a pass that did
// not genuinely run to completion, the include/exclude comparison can blame the
// process tree for a capture that produced nothing.
void testConclusiveImpliesThePassActuallyCompleted() {
  int conclusiveSeen = 0;
  for (int activated = 0; activated < 2; ++activated)
  for (int initialized = 0; initialized < 2; ++initialized)
  for (int ladder = 0; ladder < 2; ++ladder)
  for (int captured = 0; captured < 2; ++captured)
  for (int err = 0; err < 2; ++err)
  for (int frames = 0; frames < 2; ++frames)
  for (int peak = 0; peak < 2; ++peak)
  for (int disc = 0; disc < 2; ++disc)
  for (int cover = 0; cover < 2; ++cover) {
    CaptureResult r;
    r.activated       = activated != 0;
    r.initialized     = initialized != 0;
    r.ladderComplete  = ladder != 0;
    r.captured        = captured != 0;
    r.captureError    = err ? (HRESULT)0x88890004 : S_OK;
    r.framesSeen      = frames ? 4800u : 0u;
    r.peakAbs         = peak ? 500 : 0;
    r.discontinuities = disc ? 2u : 0u;
    // Deliberately set on EVERY state in the sweep. classify() must be blind to
    // it, which the paired assertion after the loop proves by construction: if
    // the field ever reached the classifier, one of the 512 states would differ
    // from its firstPacketDiscontinuity=false twin.
    r.firstPacketDiscontinuity = true;
    // cover=0 -> full coverage; cover=1 -> a tenth of the requested interval
    r.framesExpected  = cover ? 48000u : 4800u;

    const PassOutcome o = classify(r);
    // The startup marker changes NOTHING -- asserted on ALL 512 states, above the
    // early-continue, not just the conclusive ones. Placed here deliberately: the
    // states this most needs to cover are the INCONCLUSIVE ones, since the defect
    // was a pass being pushed OUT of Silent into Discontinuous.
    CaptureResult twin = r;
    twin.firstPacketDiscontinuity = false;
    CHECK(classify(twin) == o);

    if (!conclusive(o)) { continue; }
    ++conclusiveSeen;
    // Only Silent and Audio may be conclusive...
    CHECK(o == PassOutcome::Silent || o == PassOutcome::Audio);
    // ...and reaching either REQUIRES every stage to have succeeded.
    CHECK(r.activated);
    CHECK(r.initialized);
    CHECK(r.captured);
    CHECK(!FAILED(r.captureError));
    CHECK(r.framesSeen > 0u);
    // ...including that it covered a plausible share of the requested interval.
    CHECK(r.framesExpected == 0u || r.framesSeen * 2u >= r.framesExpected);
    // Silent specifically must not hide lost frames -- the startup marker is
    // excluded from this count by construction, so this stays exact.
    if (o == PassOutcome::Silent) { CHECK(r.discontinuities == 0u); }
  }
  // Guard against a vacuous sweep: if nothing was conclusive the loop above
  // asserted nothing at all.
  CHECK(conclusiveSeen > 0);
  std::printf("\nexhaustive sweep: 512 states, %d conclusive, all genuinely complete\n",
              conclusiveSeen);
}

void testNonConclusiveOutcomesAreAllRefused() {
  const PassOutcome refused[] = {
      PassOutcome::ActivationFailed, PassOutcome::InitializeFailed,
      PassOutcome::SetupFailed,      PassOutcome::CaptureError,
      PassOutcome::NoData,           PassOutcome::Discontinuous,
      PassOutcome::Truncated};
  for (const PassOutcome o : refused) { CHECK(!conclusive(o)); }
  CHECK(conclusive(PassOutcome::Silent));
  CHECK(conclusive(PassOutcome::Audio));
}

}  // namespace

int main() {
  testEveryOutcomeIsReachableAndDistinct();
  testAudioSurvivesADropout();
  testNonConclusiveOutcomesAreAllRefused();
  testConclusiveImpliesThePassActuallyCompleted();
  std::printf("\nverdict_test: %d checks passed\n", g_checks);
  return 0;
}
