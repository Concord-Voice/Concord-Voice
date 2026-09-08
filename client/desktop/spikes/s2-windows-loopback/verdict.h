// concord s2 probe — the PURE decision layer, split out so it can be EXECUTED.
//
// Everything here is a function of CaptureResult alone: no COM, no WASAPI, no
// Windows API beyond the HRESULT type. That is the whole reason it lives in its own
// header — s2probe.cpp cannot run anywhere but Windows, and this is the half where
// every verdict is actually decided.
//
// It earned the split. Review of PR #3154 landed eight findings inside these few
// functions, and each one was a case that LOOKED like silence and was not: a ladder
// abandoned mid-way, a stream that never started, a stream that lost frames, a
// stream that delivered no packet. Reasoning about that matrix in prose kept getting
// it wrong. verdict_test.cc runs it instead.
//
// Included by s2probe.cpp, so there is one definition and it is the shipped one.

#ifndef CONCORD_S2_VERDICT_H_
#define CONCORD_S2_VERDICT_H_

#include <stdint.h>

// SELF-CONTAINED ON BOTH PLATFORMS, which it was not: under _WIN32 the shim below
// is skipped, and this header then named HRESULT/S_OK/FAILED with nothing defining
// them -- so verdict_test.cc could not compile on Windows at all. s2probe.cpp only
// worked because it includes <windows.h> first. A header that compiles solely
// because of its includer's ordering is not a header. Found by CodeRabbit on
// PR #3154.
//
// Preferred over documenting a /FIwindows.h flag: a flag is a thing to remember,
// this is a thing that works. The include is idempotent for s2probe.cpp, which has
// already pulled windows.h in by the time it reaches here.
#if defined(_WIN32)
#  ifndef WIN32_LEAN_AND_MEAN
#    define WIN32_LEAN_AND_MEAN
#  endif
#  include <windows.h>
#else
// Native-test shim. The real build gets these from <windows.h>; nothing here uses
// any other Windows facility, which is what makes the test possible at all.
//
// int32_t, NOT long, and the first run of verdict_test.cc is why. Windows `long` is
// 32 bits, so an HRESULT like 0x88890004 is negative and FAILED() is true. On a
// 64-bit Unix `long` is 64 bits, the same value is POSITIVE, and FAILED() silently
// returned false — the CaptureError case classified as Audio. Production was never
// affected; a shim wrong about WIDTH would simply have made every failure-path test
// pass for the wrong reason, which is worse than not having the test.
typedef int32_t HRESULT;
#  define S_OK ((HRESULT)0L)
#  define FAILED(hr) (((HRESULT)(hr)) < 0)
#endif

struct CaptureResult {
    bool     activated       = false;
    bool     initialized     = false;
    bool     captured        = false;
    // A read that failed PART WAY THROUGH. Held separately from lastHr, which by
    // then holds Start()'s success: without it, a device invalidated mid-capture
    // yields a short, quiet buffer that is indistinguishable from a working tap on
    // a quiet app -- and this probe exists precisely to tell those two apart.
    HRESULT  captureError    = S_OK;
    uint32_t rate            = 0;
    uint16_t channels        = 0;
    uint16_t bits            = 0;
    uint64_t framesSeen      = 0;
    uint64_t framesSilentFlag = 0;   // packets the engine marked AUDCLNT_BUFFERFLAGS_SILENT
    // WASAPI sets DATA_DISCONTINUITY when frames were LOST before this packet. If
    // the target's audible interval fell in the gap, what is left looks like
    // silence -- so an otherwise-silent pass carrying one of these is not evidence.
    //
    // EXCLUDES THE FIRST PACKET, which is counted in firstPacketDiscontinuity
    // instead. See that field for why.
    uint64_t discontinuities = 0;
    // The first packet after Start(), held apart from the count above rather than
    // folded into it or dropped.
    //
    // Microsoft documents this flag as meaning "the data in the packet is not
    // correlated with the previous packet's device position; this is possibly due
    // to a stream state transition or timing glitch". The first packet HAS no
    // previous packet and Start() IS a state transition, so it is uncorrelated by
    // construction -- but the docs say "possibly", and the GetBuffer page describes
    // the flag purely as glitch detection and never states it fires at startup. So
    // whether it does is genuinely unsettled, and the two ways of guessing are not
    // equally costly:
    //
    //   count it, and it fires   -> EVERY silent pass classifies Discontinuous,
    //                               conclusive() is false, and the include-audio /
    //                               exclude-silent pair this probe exists to
    //                               produce becomes unreachable. Total failure of
    //                               the instrument, on every run.
    //   ignore it, and it does not fire -> one packet of glitch evidence is lost at
    //                               the head of a multi-second window.
    //
    // Recording it separately is correct under BOTH, and discards nothing: the
    // operator still sees it on the report line, it just does not gate the verdict.
    // Raised by Codex on PR #3154; the asymmetry, not the claim, is what decided it.
    bool     firstPacketDiscontinuity = false;
    // Frames the request implies (rate x seconds). Compared against framesSeen so a
    // stream that delivers a fraction of the interval and then stops signalling is
    // not read as a complete observation.
    uint64_t framesExpected  = 0;
    // True only when THIS pass wrote a complete WAV. The samples can be perfectly
    // valid while no artifact exists -- malloc can fail, or the write can -- and
    // every ambiguous verdict this program produces is RESOLVED BY LISTENING. A
    // conclusive classification with no file to listen to is an unresolvable result
    // dressed as a resolvable one. Found by CodeRabbit on PR #3154.
    bool     wavWritten      = false;
    // Set when a previous run's WAV could not be deleted and is still on disk. The
    // closing "listen to both WAVs" advice is qualified when this is non-null.
    const char* staleWavPath = nullptr;
    // True only if every rung of the format ladder was actually tried. Without it,
    // an activation failure PART WAY THROUGH the ladder reports "every format was
    // rejected" on the strength of one attempt.
    bool     ladderComplete  = false;
    int32_t  peakAbs         = 0;    // max |sample| over the whole capture
    HRESULT  lastHr          = S_OK;
};

// What a single pass actually ESTABLISHED. Both the per-pass summary and the
// include/exclude comparison read this rather than re-deriving it, because
// deriving it twice is exactly how the two chains drifted apart: a pass that
// activated but failed every Initialize rung printed NO verdict line in one, and
// counted as ordinary silence in the other -- where, paired with any audio in the
// control run, it printed "RISK 2 CONFIRMED". That is the most expensive wrong
// answer this program can give, since it would blame the process tree for what was
// actually a rejected audio format.
enum class PassOutcome {
    ActivationFailed,   // no process-loopback client could be opened
    InitializeFailed,   // activated, EVERY ladder rung tried, none accepted
    SetupFailed,        // Initialize took, but SetEventHandle/GetService/Start did not
    CaptureError,       // stream started, then a read failed part way through
    NoData,             // stream started and not one packet ever arrived
    Truncated,          // delivered SOME audio, then stopped signalling for the rest
    Discontinuous,      // frames were LOST, and what did arrive was silent
    Silent,             // captured cleanly, start to finish; every sample zero
    Audio               // non-zero samples present
};

// ZERO SAMPLES HAS FOUR CAUSES AND ONLY ONE OF THEM IS SILENCE. An earlier version
// of this function collapsed them, so a pass that never received a packet, or lost
// the target's audible interval to a dropout, read as "the app was quiet" -- and
// against an audible control that prints RISK 2 SUSPECTED and sends the operator to
// hunt child PIDs for a capture that yielded no data at all. Found by Codex on
// PR #3154.
static PassOutcome classify(const CaptureResult& r) {
    if (!r.activated)             return PassOutcome::ActivationFailed;
    // An activation that failed PART WAY THROUGH the ladder is an activation
    // failure, not evidence that the formats were rejected: only the rungs before
    // it were ever tried.
    if (!r.initialized)           return r.ladderComplete ? PassOutcome::InitializeFailed
                                                          : PassOutcome::ActivationFailed;
    // `captured` is set only after the capture loop runs, so this separates "the
    // stream never started" from "it started and delivered nothing". Both have
    // framesSeen == 0, and NoData alone would have reported the first as "the
    // stream started and not one packet ever arrived" -- false, and it buried
    // lastHr, which holds the only useful thing: WHICH call failed. Found by
    // CodeRabbit on PR #3154.
    if (!r.captured)              return PassOutcome::SetupFailed;
    if (FAILED(r.captureError))   return PassOutcome::CaptureError;
    if (r.framesSeen == 0u)       return PassOutcome::NoData;
    // Event starvation AFTER data. Every WaitForSingleObject timeout simply loops to
    // the deadline, so an audio-service stall a fraction of a second in produced a
    // pass with framesSeen > 0 that classified CONCLUSIVE -- settling the app matrix
    // or the Windows floor on a sliver of the requested capture. The zero-frame
    // guard cannot see it, because one packet arrived. Found by Codex on PR #3154.
    //
    // Half is a deliberately loose bar. It should not fire on ordinary jitter, and
    // if it fires routinely on real hardware that is itself a finding about how the
    // process-loopback engine paces a silent target -- report it rather than raising
    // the threshold to make it quiet.
    if (r.framesExpected > 0u && r.framesSeen * 2u < r.framesExpected) {
        return PassOutcome::Truncated;
    }
    // Audio is conclusive even across a dropout -- we heard the target. Silence is
    // not, if frames went missing.
    if (r.peakAbs > 0)            return PassOutcome::Audio;
    if (r.discontinuities > 0u)   return PassOutcome::Discontinuous;
    return PassOutcome::Silent;
}

// True only when the pass ran to completion and its silence (or audio) means what
// it appears to mean. A pass that failed earlier carries no information about the
// target's audio, and must never be fed to the include/exclude comparison.
static bool conclusive(PassOutcome o) {
    return o == PassOutcome::Silent || o == PassOutcome::Audio;
}


#endif  // CONCORD_S2_VERDICT_H_
