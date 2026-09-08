// S2 — Windows Application Loopback capture probe (ADR-0043 § Verification).
//
// STANDALONE. No Node, no Electron, no Concord code. Build it in a Visual Studio
// Developer Command Prompt with build.cmd, run it against a real window on a real
// Windows box, and read the numbers it prints. It exists to settle two questions
// that documentation disagrees about and that would each invalidate ADR-0043's
// design if answered the wrong way:
//
//   RISK 2 — is the PID that owns a window the PID that renders its audio?
//            Modern apps render from a helper process. PROCESS_LOOPBACK_MODE_
//            INCLUDE_TARGET_PROCESS_TREE is supposed to cover that. "Supposed to."
//
//   RISK 3 — is build 20348 an SDK floor or a RUNTIME floor? If runtime, consumer
//            Windows 10 22H2 (19045) gets nothing and that is a product decision.
//
// THIS FILE DELIBERATELY DOES NOT INCLUDE <audioclientactivationparams.h>.
// That header ships in Windows SDK 10.0.20348.0 and later. Including it would make
// the *build machine's SDK version* a gate on a measurement whose entire subject is
// the *target machine's runtime*. So the three structures are declared locally,
// byte-compatible with the SDK's, and the probe compiles against any SDK back to
// ~10.0.17763 and finds out what the running OS actually does.
//
// JSF++ NOTE: this is a throwaway spike and lives OUTSIDE client/desktop/native/,
// so [internal]rules/native-audio.md does not bind it. It uses <stdio.h> (AV 22
// forbids it in rt/) precisely because printing is its whole job.

#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#define _WIN32_WINNT 0x0A00
#include <windows.h>
#include <objbase.h>
#include <objidl.h>
#include <propidl.h>
#include <mmdeviceapi.h>
#include <audioclient.h>
#include <tlhelp32.h>
#include <psapi.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <errno.h>

// ---------------------------------------------------------------------------
// Local declarations of the process-loopback activation ABI.
// Mirrors <audioclientactivationparams.h> from SDK 10.0.20348.0. See the header
// comment for why this is copied rather than included.
// ---------------------------------------------------------------------------

typedef enum PROCESS_LOOPBACK_MODE_LOCAL {
    PLM_INCLUDE_TARGET_PROCESS_TREE = 0,
    PLM_EXCLUDE_TARGET_PROCESS_TREE = 1
} PROCESS_LOOPBACK_MODE_LOCAL;

typedef struct AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS_LOCAL {
    DWORD                       TargetProcessId;
    PROCESS_LOOPBACK_MODE_LOCAL ProcessLoopbackMode;
} AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS_LOCAL;

typedef enum AUDIOCLIENT_ACTIVATION_TYPE_LOCAL {
    ACTIVATION_TYPE_DEFAULT          = 0,
    ACTIVATION_TYPE_PROCESS_LOOPBACK = 1
} AUDIOCLIENT_ACTIVATION_TYPE_LOCAL;

typedef struct AUDIOCLIENT_ACTIVATION_PARAMS_LOCAL {
    AUDIOCLIENT_ACTIVATION_TYPE_LOCAL ActivationType;
    union {
        AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS_LOCAL ProcessLoopbackParams;
    };
} AUDIOCLIENT_ACTIVATION_PARAMS_LOCAL;

// mmdeviceapi.h defines this only in newer SDKs.
static const wchar_t* const kProcessLoopbackDeviceId = L"VAD\\Process_Loopback";

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

static void utf8Print(const wchar_t* w) {
    if (w == nullptr) { printf("(null)"); return; }
    char buf[1024];
    int n = WideCharToMultiByte(CP_UTF8, 0, w, -1, buf, (int)sizeof(buf), nullptr, nullptr);
    if (n <= 0) { printf("(unprintable)"); return; }
    printf("%s", buf);
}

// The name of an HRESULT we care about, so a failure reads as a diagnosis rather
// than a hex number the reader has to go look up.
static const char* hrName(HRESULT hr) {
    switch (hr) {
        case S_OK:                              return "S_OK";
        // E_INVALIDARG is 0x80070057, which is exactly
        // HRESULT_FROM_WIN32(ERROR_INVALID_PARAMETER). Do not add that as a
        // second case -- it is the same value and will not compile.
        case E_INVALIDARG:                      return "E_INVALIDARG / ERROR_INVALID_PARAMETER";
        case E_NOTIMPL:                         return "E_NOTIMPL";
        case E_ACCESSDENIED:                    return "E_ACCESSDENIED";
        case E_OUTOFMEMORY:                     return "E_OUTOFMEMORY";
        case E_POINTER:                         return "E_POINTER";
        case AUDCLNT_E_UNSUPPORTED_FORMAT:      return "AUDCLNT_E_UNSUPPORTED_FORMAT";
        case AUDCLNT_E_DEVICE_INVALIDATED:      return "AUDCLNT_E_DEVICE_INVALIDATED";
        case AUDCLNT_E_INVALID_DEVICE_PERIOD:   return "AUDCLNT_E_INVALID_DEVICE_PERIOD";
        case AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED: return "AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED";
        case AUDCLNT_E_ENDPOINT_CREATE_FAILED:  return "AUDCLNT_E_ENDPOINT_CREATE_FAILED";
        case AUDCLNT_E_SERVICE_NOT_RUNNING:     return "AUDCLNT_E_SERVICE_NOT_RUNNING";
        case AUDCLNT_E_NOT_INITIALIZED:         return "AUDCLNT_E_NOT_INITIALIZED";
        case AUDCLNT_E_WRONG_ENDPOINT_TYPE:     return "AUDCLNT_E_WRONG_ENDPOINT_TYPE";
        case AUDCLNT_E_EXCLUSIVE_MODE_NOT_ALLOWED: return "AUDCLNT_E_EXCLUSIVE_MODE_NOT_ALLOWED";
        case HRESULT_FROM_WIN32(ERROR_NOT_FOUND):   return "ERROR_NOT_FOUND";
        case HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED): return "ERROR_NOT_SUPPORTED";
        case HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND): return "ERROR_FILE_NOT_FOUND";
        default:                                return "(unrecognised)";
    }
}

static void reportHr(const char* what, HRESULT hr) {
    printf("  %-42s hr=0x%08lX  %s\n", what, (unsigned long)hr, hrName(hr));
}

// RtlGetVersion, not GetVersionEx. GetVersionEx reports 6.2 for an unmanifested
// binary on every OS since Windows 8, which would make this probe report a
// fabricated build number for the one question it exists to answer.
static void printOsVersion(DWORD* outBuild) {
    typedef LONG (WINAPI *RtlGetVersionFn)(PRTL_OSVERSIONINFOW);
    *outBuild = 0;
    HMODULE nt = GetModuleHandleW(L"ntdll.dll");
    if (nt == nullptr) { printf("OS build      : UNKNOWN (no ntdll)\n"); return; }
    RtlGetVersionFn fn = reinterpret_cast<RtlGetVersionFn>(
        reinterpret_cast<void*>(GetProcAddress(nt, "RtlGetVersion")));
    if (fn == nullptr) { printf("OS build      : UNKNOWN (no RtlGetVersion)\n"); return; }
    RTL_OSVERSIONINFOW vi;
    ZeroMemory(&vi, sizeof(vi));
    vi.dwOSVersionInfoSize = sizeof(vi);
    if (fn(&vi) != 0) { printf("OS build      : UNKNOWN (RtlGetVersion failed)\n"); return; }
    *outBuild = vi.dwBuildNumber;
    printf("OS build      : %lu.%lu.%lu   <-- RISK 3: the API is documented at 20348\n",
           (unsigned long)vi.dwMajorVersion, (unsigned long)vi.dwMinorVersion,
           (unsigned long)vi.dwBuildNumber);
    if (vi.dwBuildNumber < 20348) {
        printf("                (BELOW the documented floor. If capture works anyway,\n"
               "                 20348 is an SDK floor, not a runtime one. That is the\n"
               "                 single most valuable thing this probe can discover.)\n");
    }
}

static bool imageNameForPid(DWORD pid, wchar_t* out, DWORD cch) {
    out[0] = L'\0';
    HANDLE h = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid);
    if (h == nullptr) return false;
    DWORD n = cch;
    BOOL ok = QueryFullProcessImageNameW(h, 0, out, &n);
    CloseHandle(h);
    if (!ok) return false;
    // Keep only the leaf; the full path is noise in a table.
    wchar_t* slash = wcsrchr(out, L'\\');
    if (slash != nullptr) memmove(out, slash + 1, (wcslen(slash + 1) + 1) * sizeof(wchar_t));
    return true;
}

// RISK 2 lives here. INCLUDE_TARGET_PROCESS_TREE captures the target and its
// DESCENDANTS. If an app renders audio from a SIBLING or from a service (Chrome's
// audio service is a child of the browser process; some apps use a system service
// that is not), the tree will not contain it and the tap comes back silent.
// Printing the tree makes that visible instead of leaving it to be inferred.
static void printProcessTree(DWORD rootPid) {
    HANDLE snap = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
    if (snap == INVALID_HANDLE_VALUE) { printf("  (process snapshot unavailable)\n"); return; }
    PROCESSENTRY32W pe;
    pe.dwSize = sizeof(pe);
    int found = 0;
    if (Process32FirstW(snap, &pe)) {
        do {
            if (pe.th32ParentProcessID == rootPid) {
                printf("    child pid=%-7lu  ", (unsigned long)pe.th32ProcessID);
                utf8Print(pe.szExeFile);
                printf("\n");
                found++;
            }
        } while (Process32NextW(snap, &pe));
    }
    CloseHandle(snap);
    if (found == 0) printf("    (no direct children)\n");
}

// ---------------------------------------------------------------------------
// --list : enumerate visible top-level windows so no second tool is needed
// ---------------------------------------------------------------------------

static BOOL CALLBACK enumProc(HWND hwnd, LPARAM) {
    if (!IsWindowVisible(hwnd)) return TRUE;
    if (GetWindow(hwnd, GW_OWNER) != nullptr) return TRUE;
    wchar_t title[256];
    int len = GetWindowTextW(hwnd, title, 256);
    if (len == 0) return TRUE;
    DWORD pid = 0;
    GetWindowThreadProcessId(hwnd, &pid);
    wchar_t image[MAX_PATH];
    if (!imageNameForPid(pid, image, MAX_PATH)) wcscpy_s(image, MAX_PATH, L"(access denied)");
    printf("  hwnd=0x%p  pid=%-7lu  ", (void*)hwnd, (unsigned long)pid);
    utf8Print(image);
    printf("  |  ");
    utf8Print(title);
    printf("\n");
    return TRUE;
}

// ---------------------------------------------------------------------------
// Activation completion handler. Hand-rolled COM rather than WRL so the build is
// one cl.exe invocation with no extra dependency.
// ---------------------------------------------------------------------------

class ActivateHandler : public IActivateAudioInterfaceCompletionHandler, public IAgileObject {
public:
    ActivateHandler() : refs_(1), done_(nullptr) {
        done_ = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    }
    virtual ~ActivateHandler() { if (done_ != nullptr) CloseHandle(done_); }

    HANDLE event() const { return done_; }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (ppv == nullptr) return E_POINTER;
        if (riid == __uuidof(IUnknown) || riid == __uuidof(IAgileObject)) {
            *ppv = static_cast<IAgileObject*>(this);
        } else if (riid == __uuidof(IActivateAudioInterfaceCompletionHandler)) {
            *ppv = static_cast<IActivateAudioInterfaceCompletionHandler*>(this);
        } else {
            *ppv = nullptr;
            return E_NOINTERFACE;
        }
        AddRef();
        return S_OK;
    }
    STDMETHODIMP_(ULONG) AddRef() override { return InterlockedIncrement(&refs_); }
    STDMETHODIMP_(ULONG) Release() override {
        LONG r = InterlockedDecrement(&refs_);
        if (r == 0) delete this;
        return (ULONG)r;
    }

    STDMETHODIMP ActivateCompleted(IActivateAudioInterfaceAsyncOperation*) override {
        SetEvent(done_);
        return S_OK;
    }

private:
    LONG   refs_;
    HANDLE done_;
};

// ---------------------------------------------------------------------------
// WAV
// ---------------------------------------------------------------------------

static bool writeWav(const char* path, const unsigned char* pcm, size_t bytes,
                     uint32_t rate, uint16_t channels, uint16_t bits) {
    FILE* f = nullptr;
    if (fopen_s(&f, path, "wb") != 0 || f == nullptr) return false;
    const uint32_t byteRate   = rate * channels * (bits / 8u);
    const uint16_t blockAlign = (uint16_t)(channels * (bits / 8u));
    const uint32_t dataSize   = (uint32_t)bytes;
    const uint32_t riffSize   = 36u + dataSize;
    // EVERY write is checked, and so is the close. A full disk or a failing
    // removable/network destination otherwise yields a truncated file that this
    // function reports as written -- and an ambiguous EXCLUDE result is settled by
    // LISTENING to that artifact, so a silently corrupt WAV is not a cosmetic
    // failure here, it is a wrong answer. Found by Codex on PR #3154.
    bool ok = true;
    ok = ok && (fwrite("RIFF", 1, 4, f) == 4);
    ok = ok && (fwrite(&riffSize, 4, 1, f) == 1);
    ok = ok && (fwrite("WAVEfmt ", 1, 8, f) == 8);
    const uint32_t fmtSize = 16u; const uint16_t pcmTag = 1u;
    ok = ok && (fwrite(&fmtSize, 4, 1, f) == 1);
    ok = ok && (fwrite(&pcmTag, 2, 1, f) == 1);
    ok = ok && (fwrite(&channels, 2, 1, f) == 1);
    ok = ok && (fwrite(&rate, 4, 1, f) == 1);
    ok = ok && (fwrite(&byteRate, 4, 1, f) == 1);
    ok = ok && (fwrite(&blockAlign, 2, 1, f) == 1);
    ok = ok && (fwrite(&bits, 2, 1, f) == 1);
    ok = ok && (fwrite("data", 1, 4, f) == 4);
    ok = ok && (fwrite(&dataSize, 4, 1, f) == 1);
    ok = ok && (bytes == 0u || fwrite(pcm, 1, bytes, f) == bytes);
    // fclose flushes; a write can fail HERE and nowhere earlier.
    const bool closed = (fclose(f) == 0);
    if (!ok || !closed) {
        // Do not leave a truncated file wearing the name of a good one.
        remove(path);
        return false;
    }
    return true;
}

// ---------------------------------------------------------------------------
// The capture itself
// ---------------------------------------------------------------------------

struct FormatCandidate { uint32_t rate; uint16_t channels; uint16_t bits; };
struct TimingCandidate { REFERENCE_TIME buffer; REFERENCE_TIME periodicity; };

// CaptureResult, PassOutcome, classify() and conclusive() live in verdict.h so
// they can be executed by a native test — see that file's header comment.
#include "verdict.h"


// Activate a FRESH process-loopback IAudioClient. A fresh one per Initialize
// attempt is deliberate: IAudioClient has no documented contract for retrying
// Initialize with different parameters after a failure, so a poisoned client would
// make the format ladder below report "no format works" -- an expensive and wrong
// conclusion about the platform.
// `verbose` gates the SUCCESS-path chatter only. A FAILURE always prints, whichever
// attempt it is: the ladder re-activates per rung, and with failures gated on the
// first attempt a retry that could not activate produced no output at all -- the run
// simply ended. Found by Codex on PR #3154.
static IAudioClient* activateLoopbackClient(DWORD pid, PROCESS_LOOPBACK_MODE_LOCAL mode,
                                            bool verbose, HRESULT* outHr) {
    *outHr = E_FAIL;

    AUDIOCLIENT_ACTIVATION_PARAMS_LOCAL params;
    ZeroMemory(&params, sizeof(params));
    params.ActivationType                            = ACTIVATION_TYPE_PROCESS_LOOPBACK;
    params.ProcessLoopbackParams.TargetProcessId     = pid;
    params.ProcessLoopbackParams.ProcessLoopbackMode = mode;

    // Never PropVariantClear this one: pBlobData points at the stack struct above,
    // which the PROPVARIANT does not own. Clearing it would free a stack address.
    PROPVARIANT pv;
    PropVariantInit(&pv);
    pv.vt             = VT_BLOB;
    pv.blob.cbSize    = sizeof(params);
    pv.blob.pBlobData = reinterpret_cast<BYTE*>(&params);

    ActivateHandler* handler = new ActivateHandler();
    IActivateAudioInterfaceAsyncOperation* op = nullptr;

    HRESULT hr = ActivateAudioInterfaceAsync(kProcessLoopbackDeviceId,
                                             __uuidof(IAudioClient), &pv, handler, &op);
    if (verbose || FAILED(hr)) reportHr("ActivateAudioInterfaceAsync (dispatch)", hr);
    if (FAILED(hr)) { *outHr = hr; handler->Release(); return nullptr; }

    // The completion arrives on an MTA worker thread; this program has no message
    // pump, which is why main() initialises COM as MTA. An STA would deadlock here.
    if (WaitForSingleObject(handler->event(), 5000) != WAIT_OBJECT_0) {
        printf("  ActivateAudioInterfaceAsync never completed within 5s\n");
        if (op != nullptr) op->Release();
        handler->Release();
        *outHr = HRESULT_FROM_WIN32(WAIT_TIMEOUT);
        return nullptr;
    }

    HRESULT   activateHr = E_FAIL;
    IUnknown* punk       = nullptr;
    HRESULT   getHr      = op->GetActivateResult(&activateHr, &punk);
    if (verbose || FAILED(getHr) || FAILED(activateHr)) {
        reportHr("GetActivateResult (call)", getHr);
        reportHr("GetActivateResult (activation)", activateHr);
    }
    op->Release();
    handler->Release();
    if (FAILED(getHr))     { *outHr = getHr;     return nullptr; }
    if (FAILED(activateHr)){ *outHr = activateHr; return nullptr; }
    if (punk == nullptr)   { *outHr = E_POINTER;  return nullptr; }

    IAudioClient* client = nullptr;
    hr = punk->QueryInterface(__uuidof(IAudioClient), reinterpret_cast<void**>(&client));
    punk->Release();
    if (verbose || FAILED(hr)) reportHr("QueryInterface(IAudioClient)", hr);
    *outHr = hr;
    return SUCCEEDED(hr) ? client : nullptr;
}

static CaptureResult runCapture(DWORD targetPid, PROCESS_LOOPBACK_MODE_LOCAL mode,
                                int seconds, const char* wavPath) {
    CaptureResult res;

    // Remove the destination FIRST. A pass that fails before capture writes no file,
    // so on a re-run with the same prefix and PID an EARLIER run's WAV would sit
    // under this run's expected name while the program still says "listen to both
    // WAVs" -- offering stale audio as evidence from a pass that produced none.
    // Found by Codex on PR #3154.
    if (wavPath != nullptr) {
        // A pass that writes nothing must not leave the PREVIOUS run's audio sitting
        // under this run's name, so the destination goes first.
        //
        // THE VERDICT IS ERRNO, NOT A SECOND PROBE. An earlier version confirmed
        // survival by reopening the file, which fails for the single most likely
        // cause of the removal failing: another process holding it WITHOUT read
        // sharing. Both calls are denied, the flag stays clear, and the warning
        // never fires in exactly the case it was written for -- the check needed the
        // access that was being denied. Found by Codex on PR #3154.
        //
        // errno needs no further syscall, so nothing else can fail: ENOENT means
        // there was nothing to delete, which is the ordinary case, and ANY other
        // failure is treated as "it is still there". That is deliberately
        // conservative -- a spurious caution costs the operator a glance, a missed
        // one hands them a stale recording as evidence.
        errno = 0;
        if (remove(wavPath) != 0 && errno != ENOENT) {
            res.staleWavPath = wavPath;
            printf("  WARNING: could not delete %s (errno %d).\n"
                   "           Assume it is STILL THERE and from an EARLIER RUN. If\n"
                   "           this pass writes no audio, that file is not evidence.\n"
                   "           Close any player holding it, clear the read-only flag,\n"
                   "           and re-run before trusting it.\n",
                   wavPath, errno);
        }
    }

    // GetMixFormat is NOT supported on a process-loopback client, so the format has
    // to be asserted rather than asked for -- and which formats and which
    // buffer/periodicity pairs the engine accepts is itself an unknown the addon
    // needs answered. Walk a ladder and report the HRESULT for every rung.
    //
    // {0, 0} leads because that is the documented shared-mode event-driven form
    // (both durations zero, engine picks). The non-zero rungs follow because
    // Microsoft's own ApplicationLoopback sample passes an explicit 20 ms buffer,
    // and the two cannot both be the only correct answer.
    const FormatCandidate formats[] = {
        { 48000u, 2u, 16u },
        { 44100u, 2u, 16u },
        { 48000u, 1u, 16u },
    };
    const TimingCandidate timings[] = {
        { 0,       0      },
        { 200000,  0      },
        { 200000,  200000 },
        { 1000000, 0      },
    };

    IAudioClient* client = nullptr;
    HRESULT       hr     = E_FAIL;
    bool          first  = true;

    for (size_t fi = 0; fi < ARRAYSIZE(formats) && client == nullptr; ++fi) {
        for (size_t ti = 0; ti < ARRAYSIZE(timings) && client == nullptr; ++ti) {
            IAudioClient* c = activateLoopbackClient(targetPid, mode, first, &hr);
            first = false;
            if (c == nullptr) {
                // Activation, not format, is what failed. Retrying other formats
                // would only reprint the same error. ladderComplete stays false, so
                // classify() reports this as an activation failure rather than
                // claiming every format was rejected on the strength of the rungs
                // that happened to run before it.
                res.lastHr = hr;
                return res;
            }
            res.activated = true;

            WAVEFORMATEX wf;
            ZeroMemory(&wf, sizeof(wf));
            wf.wFormatTag      = WAVE_FORMAT_PCM;
            wf.nChannels       = formats[fi].channels;
            wf.nSamplesPerSec  = formats[fi].rate;
            wf.wBitsPerSample  = formats[fi].bits;
            wf.nBlockAlign     = (WORD)(wf.nChannels * wf.wBitsPerSample / 8u);
            wf.nAvgBytesPerSec = wf.nSamplesPerSec * wf.nBlockAlign;
            wf.cbSize          = 0;

            hr = c->Initialize(AUDCLNT_SHAREMODE_SHARED,
                               AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK,
                               timings[ti].buffer, timings[ti].periodicity, &wf, nullptr);
            printf("  Initialize %5lu Hz %uch %ub  buf=%lld per=%lld  ->  hr=0x%08lX %s\n",
                   (unsigned long)formats[fi].rate, formats[fi].channels, formats[fi].bits,
                   (long long)timings[ti].buffer, (long long)timings[ti].periodicity,
                   (unsigned long)hr, hrName(hr));

            if (SUCCEEDED(hr)) {
                client       = c;
                res.rate     = formats[fi].rate;
                res.channels = formats[fi].channels;
                res.bits     = formats[fi].bits;
            } else {
                c->Release();
            }
        }
    }

    // Falling out of both loops means every rung was tried.
    res.ladderComplete = true;
    res.lastHr = hr;
    if (client == nullptr) return res;
    res.initialized = true;

    HANDLE evt = CreateEventW(nullptr, FALSE, FALSE, nullptr);
    hr = client->SetEventHandle(evt);
    reportHr("SetEventHandle", hr);
    if (FAILED(hr)) { res.lastHr = hr; CloseHandle(evt); client->Release(); return res; }

    IAudioCaptureClient* capture = nullptr;
    hr = client->GetService(__uuidof(IAudioCaptureClient), reinterpret_cast<void**>(&capture));
    reportHr("GetService(IAudioCaptureClient)", hr);
    if (FAILED(hr)) { res.lastHr = hr; CloseHandle(evt); client->Release(); return res; }

    hr = client->Start();
    reportHr("Start", hr);
    if (FAILED(hr)) {
        res.lastHr = hr; capture->Release(); CloseHandle(evt); client->Release(); return res;
    }

    res.framesExpected = (uint64_t)res.rate * (uint64_t)seconds;
    const size_t   cap = (size_t)res.rate * res.channels * (res.bits / 8u) * (size_t)(seconds + 1);
    unsigned char* pcm = (unsigned char*)malloc(cap);
    size_t         used = 0;

    // Latched on the first packet actually handed back by GetBuffer, NOT on the
    // first loop iteration -- the loop can spin on WaitForSingleObject timeouts
    // and on zero-length packets before any data arrives.
    bool sawFirstPacket = false;

    const ULONGLONG deadline = GetTickCount64() + (ULONGLONG)seconds * 1000ull;
    while (GetTickCount64() < deadline) {
        if (WaitForSingleObject(evt, 500) != WAIT_OBJECT_0) continue;

        // GetNextPacketSize drives the loop rather than interpreting GetBuffer's
        // AUDCLNT_S_BUFFER_EMPTY, which is a SUCCESS code and therefore easy to
        // mishandle as data. This is the documented shape.
        UINT32 packet = 0;
        hr = capture->GetNextPacketSize(&packet);
        if (FAILED(hr)) { res.captureError = hr; break; }
        while (SUCCEEDED(hr) && packet > 0) {
            BYTE*  data   = nullptr;
            UINT32 frames = 0;
            DWORD  flags  = 0;
            hr = capture->GetBuffer(&data, &frames, &flags, nullptr, nullptr);
            if (FAILED(hr)) { res.captureError = hr; break; }

            res.framesSeen += frames;
            if ((flags & AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY) != 0) {
                // The first packet is recorded, not counted. Start() is a stream
                // state transition and the first packet has no predecessor to be
                // correlated with, so the flag here may mean nothing at all --
                // and counting it would make every silent pass Discontinuous,
                // which is exactly the verdict this probe needs to be able to
                // reach. See CaptureResult::firstPacketDiscontinuity.
                if (sawFirstPacket) { res.discontinuities += 1u; }
                else                { res.firstPacketDiscontinuity = true; }
            }
            sawFirstPacket = true;
            const bool silentFlag = (flags & AUDCLNT_BUFFERFLAGS_SILENT) != 0;
            if (silentFlag) {
                res.framesSilentFlag += frames;
            } else if (res.bits == 16u && data != nullptr) {
                const int16_t* s = reinterpret_cast<const int16_t*>(data);
                const size_t   n = (size_t)frames * res.channels;
                for (size_t i = 0; i < n; ++i) {
                    const int32_t a = s[i] < 0 ? -(int32_t)s[i] : (int32_t)s[i];
                    if (a > res.peakAbs) res.peakAbs = a;
                }
            }

            const size_t bytes = (size_t)frames * res.channels * (res.bits / 8u);
            if (pcm != nullptr && used + bytes <= cap) {
                if (silentFlag || data == nullptr) memset(pcm + used, 0, bytes);
                else                               memcpy(pcm + used, data, bytes);
                used += bytes;
            }

            // ReleaseBuffer can fail on an invalidated device, and a failure on the
            // LAST packet before the deadline may never be surfaced by another
            // GetNextPacketSize -- so discarding it lets a shortened stream be
            // marked captured and classified as conclusive. Found by Codex on
            // PR #3154.
            const HRESULT relHr = capture->ReleaseBuffer(frames);
            if (FAILED(relHr)) { res.captureError = relHr; break; }
            hr = capture->GetNextPacketSize(&packet);
            if (FAILED(hr)) { res.captureError = hr; break; }
        }
        if (FAILED(res.captureError)) break;
    }

    client->Stop();
    res.captured = true;

    if (wavPath == nullptr) {
        // Nothing was asked for.
    } else if (pcm == nullptr) {
        // This was previously SILENT: the allocation failed, no file was written,
        // and the pass still went on to classify and be listened for.
        printf("  NO WAV: could not allocate %zu bytes for the capture buffer.\n"
               "          The sample counters below are still valid, but there is no\n"
               "          recording to listen to.\n", cap);
    } else if (used == 0u) {
        printf("  NO WAV: this pass captured no audio, so nothing was written.\n");
    } else if (writeWav(wavPath, pcm, used, res.rate, res.channels, res.bits)) {
        res.wavWritten = true;
        // The file at this path is now THIS run's. Whatever happened to its
        // predecessor stopped mattering the moment we overwrote it -- leaving the
        // flag set would have the report warn that a complete, current recording is
        // prior-run evidence, which is the original defect pointing the wrong way.
        // Found by CodeRabbit on PR #3154.
        res.staleWavPath = nullptr;
        printf("  wrote %s (%zu bytes PCM)\n", wavPath, used);
    } else {
        printf("  NO WAV: FAILED to write %s — the partial file was removed.\n", wavPath);
    }
    free(pcm);

    capture->Release();
    CloseHandle(evt);
    client->Release();
    return res;
}

static void summarise(const char* label, const CaptureResult& r) {
    printf("\n  [%s]\n", label);
    printf("    activated        : %s\n", r.activated   ? "yes" : "NO");
    printf("    initialized      : %s", r.initialized ? "yes" : "NO");
    if (r.initialized) printf("  (%lu Hz, %u ch, %u bit)", (unsigned long)r.rate, r.channels, r.bits);
    printf("\n");
    printf("    frames captured  : %llu\n", (unsigned long long)r.framesSeen);
    printf("    frames flagged silent by the engine : %llu\n",
           (unsigned long long)r.framesSilentFlag);
    printf("    packets flagged DISCONTINUOUS       : %llu%s\n",
           (unsigned long long)r.discontinuities,
           r.discontinuities > 0u ? "  (excludes the first packet)" : "");
    // Shown, but deliberately NOT added to the count above and NOT part of any
    // verdict. Start() is a stream state transition, so the flag on the very first
    // packet may carry no information at all; suppressing it entirely would hide
    // that from the operator, and counting it would make every silent pass
    // inconclusive. See CaptureResult::firstPacketDiscontinuity.
    if (r.firstPacketDiscontinuity) {
        printf("    first packet also carried DATA_DISCONTINUITY — expected after\n"
               "      Start() and NOT counted above, so it does not affect the verdict\n");
    }
    if (r.framesExpected > 0u) {
        printf("    coverage                            : %llu of ~%llu frames (%.0f%%)\n",
               (unsigned long long)r.framesSeen, (unsigned long long)r.framesExpected,
               100.0 * (double)r.framesSeen / (double)r.framesExpected);
    }
    printf("    peak |sample|    : %ld  (of 32767)\n", (long)r.peakAbs);

    switch (classify(r)) {
        case PassOutcome::ActivationFailed:
            printf("    VERDICT          : ACTIVATION FAILED (hr=0x%08lX %s)\n"
                   "                       No process-loopback client could be opened. If this\n"
                   "                       machine's build is >= 20348 the OS floor is NOT the\n"
                   "                       cause and the hr above is.\n",
                   (unsigned long)r.lastHr, hrName(r.lastHr));
            break;
        case PassOutcome::InitializeFailed:
            printf("    VERDICT          : INITIALIZE FAILED (hr=0x%08lX %s)\n"
                   "                       Activation succeeded and EVERY rung of the format\n"
                   "                       ladder was rejected. This says nothing about the\n"
                   "                       target's audio; it says the engine accepted none of\n"
                   "                       the formats tried. Report the ladder above verbatim.\n",
                   (unsigned long)r.lastHr, hrName(r.lastHr));
            break;
        case PassOutcome::SetupFailed:
            printf("    VERDICT          : CAPTURE SETUP FAILED (hr=0x%08lX %s)\n"
                   "                       A format was accepted, but SetEventHandle,\n"
                   "                       GetService or Start did not take, so the stream\n"
                   "                       NEVER RAN. Not silence, and not a format problem\n"
                   "                       either -- the hr above is the whole diagnosis.\n",
                   (unsigned long)r.lastHr, hrName(r.lastHr));
            break;
        case PassOutcome::CaptureError:
            printf("    VERDICT          : CAPTURE FAILED MID-STREAM (hr=0x%08lX %s)\n"
                   "                       The stream started and then a read failed, so this\n"
                   "                       pass is INCOMPLETE. Its silence is not evidence of\n"
                   "                       anything. Re-run it.\n",
                   (unsigned long)r.captureError, hrName(r.captureError));
            break;
        case PassOutcome::NoData:
            printf("    VERDICT          : NO DATA — the stream started and not one packet\n"
                   "                       ever arrived. This is NOT silence: nothing was\n"
                   "                       captured to be silent. Re-run it; if it repeats,\n"
                   "                       record it as a finding with the Initialize rung\n"
                   "                       that was accepted above.\n");
            break;
        case PassOutcome::Truncated:
            printf("    VERDICT          : TRUNCATED — the stream delivered %llu of the\n"
                   "                       ~%llu frames the %d-second request implies, then\n"
                   "                       stopped signalling. A fraction of a capture is\n"
                   "                       not a capture: it cannot settle the app matrix\n"
                   "                       or the Windows floor. Re-run. If it repeats,\n"
                   "                       that is a finding about how the engine paces a\n"
                   "                       silent target — report it.\n",
                   (unsigned long long)r.framesSeen, (unsigned long long)r.framesExpected,
                   (int)(r.framesExpected / (r.rate ? r.rate : 1u)));
            break;
        case PassOutcome::Discontinuous:
            printf("    VERDICT          : INCONCLUSIVE — %llu packet(s) arrived flagged\n"
                   "                       DATA_DISCONTINUITY, meaning frames were LOST, and\n"
                   "                       what did arrive was silent. The target's audible\n"
                   "                       interval may have been in the gap. Re-run it.\n",
                   (unsigned long long)r.discontinuities);
            break;
        case PassOutcome::Silent:
            printf("    VERDICT          : LIVE BUT SILENT — the stream exists and delivered\n"
                   "                       %llu frames, and every sample is zero. This is the\n"
                   "                       failure shape ADR-0043 D7 found on macOS. Compare\n"
                   "                       against the control pass before concluding anything.\n",
                   (unsigned long long)r.framesSeen);
            break;
        case PassOutcome::Audio:
            printf("    VERDICT          : AUDIO PRESENT\n");
            break;
    }
}

// ---------------------------------------------------------------------------

static void usage(void) {
    printf(
      "S2 Windows Application Loopback probe (ADR-0043)\n"
      "\n"
      "  s2probe --list\n"
      "        Enumerate visible top-level windows: HWND, PID, image, title.\n"
      "\n"
      "  s2probe --hwnd <0xHANDLE|decimal> [--seconds N] [--out PREFIX]\n"
      "  s2probe --pid  <PID>              [--seconds N] [--out PREFIX]\n"
      "        Resolve the target, print its process tree, then capture TWICE:\n"
      "        once INCLUDE_TARGET_PROCESS_TREE and once EXCLUDE_TARGET_PROCESS_TREE.\n"
      "\n"
      "  The exclude run is a POSITIVE CONTROL and is not optional. Silence from an\n"
      "  include-mode tap does not prove the tap works -- the app may simply not have\n"
      "  been playing. Read the pair:\n"
      "\n"
      "     include audio,  exclude silent  -> WORKS, but only if the target kept\n"
      "                                        playing: the passes run BACK TO BACK,\n"
      "                                        so a track ending in between gives the\n"
      "                                        same pair from a whole-system tap.\n"
      "     include silent, exclude audio   -> RISK 2 SUSPECTED, not confirmed. The\n"
      "                                        control only proves SOMETHING outside\n"
      "                                        the tree made sound -- a notification\n"
      "                                        looks identical. Listen to the WAV.\n"
      "     include silent, exclude silent  -> AMBIGUOUS. Either nothing was playing,\n"
      "                                        OR the include tap is live-but-silent\n"
      "                                        and nothing else made a sound. Was the\n"
      "                                        target audible? If yes, that is the\n"
      "                                        finding S2 exists to catch, NOT an\n"
      "                                        invalid run.\n"
      "     include audio,  exclude audio   -> not narrowing, OR unrelated audio\n"
      "                                        contaminated the control. Listen to tell.\n"
      "\n"
      "  A CAPTURE CAN YIELD NO USABLE AUDIO FOR SEVEN REASONS AND ONLY ONE IS\n"
      "  SILENCE. Activation failure,\n"
      "  an exhausted format ladder, a capture that never started, a stream that\n"
      "  delivered only a sliver, a mid-stream read\n"
      "  failure, a stream that never\n"
      "  delivered a packet, and a stream that LOST frames each get their own\n"
      "  verdict plus the responsible HRESULT, and the pair comparison refuses to\n"
      "  run. Every one of them yields zero frames or zero peak, byte-identical to\n"
      "  a working tap on a quiet app -- reading any as silence is what blames the\n"
      "  process tree for a capture that produced nothing.\n"
      "\n"
      "  Defaults: --seconds 8, --out s2\n");
}

int main(int argc, char** argv) {
    SetConsoleOutputCP(CP_UTF8);

    if (argc < 2) { usage(); return 2; }

    bool     listMode = false;
    HWND     hwnd     = nullptr;
    DWORD    pid      = 0;
    int      seconds  = 8;
    const char* prefix = "s2";

    for (int i = 1; i < argc; ++i) {
        if (strcmp(argv[i], "--list") == 0) {
            listMode = true;
        } else if (strcmp(argv[i], "--hwnd") == 0 && i + 1 < argc) {
            hwnd = (HWND)(uintptr_t)_strtoui64(argv[++i], nullptr, 0);
        } else if (strcmp(argv[i], "--pid") == 0 && i + 1 < argc) {
            pid = (DWORD)strtoul(argv[++i], nullptr, 0);
        } else if (strcmp(argv[i], "--seconds") == 0 && i + 1 < argc) {
            seconds = atoi(argv[++i]);
            if (seconds < 1)  seconds = 1;
            if (seconds > 60) seconds = 60;
        } else if (strcmp(argv[i], "--out") == 0 && i + 1 < argc) {
            prefix = argv[++i];
        } else {
            printf("unrecognised argument: %s\n\n", argv[i]);
            usage();
            return 2;
        }
    }

    printf("=====================================================================\n");
    printf("S2 probe — Windows Application Loopback (ADR-0043 risks 2 and 3)\n");
    printf("=====================================================================\n");
    DWORD build = 0;
    printOsVersion(&build);

    // MTA. ActivateAudioInterfaceAsync delivers its completion on a worker thread
    // and this program has no message pump, so an STA would deadlock the wait.
    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    if (FAILED(hr)) { reportHr("CoInitializeEx(MTA)", hr); return 1; }

    if (listMode) {
        printf("\nVisible top-level windows:\n");
        EnumWindows(enumProc, 0);
        CoUninitialize();
        return 0;
    }

    if (hwnd != nullptr) {
        if (!IsWindow(hwnd)) {
            printf("\nHWND 0x%p is not a live window. Re-run --list.\n", (void*)hwnd);
            CoUninitialize();
            return 1;
        }
        DWORD owner = 0;
        GetWindowThreadProcessId(hwnd, &owner);
        pid = owner;
        printf("\nTarget from HWND 0x%p  ->  pid %lu\n", (void*)hwnd, (unsigned long)pid);
    } else if (pid == 0) {
        printf("\nGive --hwnd or --pid (or --list).\n");
        CoUninitialize();
        return 2;
    } else {
        printf("\nTarget pid %lu\n", (unsigned long)pid);
    }

    wchar_t image[MAX_PATH];
    if (imageNameForPid(pid, image, MAX_PATH)) {
        printf("Target image  : ");
        utf8Print(image);
        printf("\n");
    } else {
        printf("Target image  : (could not open process — elevated? try an admin prompt)\n");
    }

    printf("Process tree (INCLUDE_TARGET_PROCESS_TREE reaches these and their descendants):\n");
    printProcessTree(pid);

    char wavInc[512];
    char wavExc[512];
    sprintf_s(wavInc, sizeof(wavInc), "%s-include-pid%lu.wav", prefix, (unsigned long)pid);
    sprintf_s(wavExc, sizeof(wavExc), "%s-exclude-pid%lu.wav", prefix, (unsigned long)pid);

    printf("\n--- RUN 1 of 2: INCLUDE_TARGET_PROCESS_TREE (%d s) ---\n", seconds);
    printf("    Make the target app play audio NOW.\n");
    CaptureResult inc = runCapture(pid, PLM_INCLUDE_TARGET_PROCESS_TREE, seconds, wavInc);

    printf("\n--- RUN 2 of 2: EXCLUDE_TARGET_PROCESS_TREE (%d s) — positive control ---\n", seconds);
    printf("    Keep the target playing. This run captures EVERYTHING ELSE.\n");
    CaptureResult exc = runCapture(pid, PLM_EXCLUDE_TARGET_PROCESS_TREE, seconds, wavExc);

    printf("\n=====================================================================\n");
    printf("RESULTS\n");
    printf("=====================================================================\n");
    summarise("include target process tree", inc);
    summarise("exclude target process tree (control)", exc);

    // The comparison is only meaningful when BOTH passes ran to completion. An
    // earlier version compared incAudio/excAudio directly, which silently treated
    // "no format was accepted" and "the app was quiet" as the same observation.
    const PassOutcome io = classify(inc);
    const PassOutcome eo = classify(exc);

    printf("\n  READ THE PAIR:\n");
    if (!conclusive(io)) {
        printf("    NO VERDICT. The include pass did not produce a usable observation\n"
               "    (see its line above): activation failed, no format was accepted,\n"
               "    capture setup failed (SetEventHandle / GetService / Start), a read\n"
               "    failed, no packet arrived, or frames were lost. NONE of those\n"
               "    is silence, and treating them as silence is what would blame the\n"
               "    process tree for a capture that yielded nothing. Fix the reported\n"
               "    condition and run it again on build %lu.\n",
               (unsigned long)build);
    } else if (!conclusive(eo)) {
        printf("    NO VERDICT. The include pass %s, but the EXCLUDE control did not\n"
               "    produce a usable observation, so the pair cannot be read. The control is not\n"
               "    optional: without it, silence and 'wrong process' are the same\n"
               "    observation. Fix the control's reported hr and run it again.\n",
               io == PassOutcome::Audio ? "carried audio" : "was silent");
    } else if (io == PassOutcome::Audio && eo == PassOutcome::Silent) {
        // "at the same time" was impossible by construction and is gone: the two
        // passes run BACK TO BACK, never together. If the target stopped playing
        // between them -- a track ending during the include pass is enough -- then a
        // whole-system tap produces this identical pair. Found by Codex on PR #3154.
        printf("    WORKS — CONDITIONAL ON ONE THING ONLY YOU KNOW. Include carried\n"
               "    audio and the exclude control did not, on build %lu.\n"
               "\n"
               "    THE TWO PASSES RAN BACK TO BACK, NOT TOGETHER. So this is proof of\n"
               "    isolation ONLY IF the target was still playing throughout the\n"
               "    EXCLUDE pass. If playback ended in between — a track finishing is\n"
               "    enough — a whole-system tap gives exactly this pair.\n"
               "\n"
               "    Was the target still audible for the whole second capture? If yes,\n"
               "    per-process capture works for this app. If no, or you are unsure,\n"
               "    re-run with continuous audio before recording it.\n",
               (unsigned long)build);
    } else if (io == PassOutcome::Silent && eo == PassOutcome::Audio) {
        // Deliberately SUSPECTED, not CONFIRMED. excAudio only proves that SOME
        // process outside the target tree emitted samples -- a notification chime
        // or a background player produces this exact signature. The program cannot
        // tell whose audio it captured; only the operator listening to the WAV can.
        printf("    RISK 2 SUSPECTED — NOT confirmed, and the difference matters.\n"
               "    The include pass was silent while the control carried audio. That\n"
               "    is consistent with the target rendering audio OUTSIDE its process\n"
               "    tree — but it is equally consistent with any UNRELATED process\n"
               "    (a notification, a background player) having made the only sound.\n"
               "    %s\n", exc.wavWritten
            ? "LISTEN TO the exclude WAV. If it is the target app, risk 2 is real\n"
              "    for this app — and the renderer is OUTSIDE the target's tree by\n"
              "    definition, because exclude mode suppressed the target AND its\n"
              "    descendants and you still heard it. So do NOT retry the child\n"
              "    PIDs listed above: every one of them is inside the tree that was\n"
              "    just proven not to be the source. Look OUTWARD instead — the\n"
              "    target's parent, its siblings under that parent, and any separate\n"
              "    audio or media host the app spawns outside its own tree. Open the\n"
              "    Windows Volume Mixer while the sound is playing: it names the\n"
              "    process actually holding the audio session, which is the PID to\n"
              "    re-run against. If it is something else, silence the machine and\n"
              "    run this again."
            : "AND YOU CANNOT SETTLE IT FROM THIS RUN: the exclude pass wrote NO\n"
              "    WAV (see its line above), so there is nothing to listen to and the\n"
              "    source cannot be identified. Fix that and re-run before recording\n"
              "    anything about risk 2.");
    } else if (io == PassOutcome::Silent && eo == PassOutcome::Silent) {
        // NOT "nothing was playing". A BROKEN include tap delivers valid,
        // zero-filled packets, and if nothing outside the target tree happened to
        // make sound the control is silent too -- producing this exact pair while
        // the target was audible the whole time. That is the live-but-silent
        // failure S2 exists to find, and asserting "nothing was playing" would
        // hide it. The program cannot tell the two apart; only the operator can.
        // Found by Codex on PR #3154.
        printf("    AMBIGUOUS — and ONLY YOU CAN SETTLE IT. Both taps were silent,\n"
               "    which has two completely different explanations:\n"
               "\n"
               "      (a) nothing was actually playing — re-run with audio, or\n"
               "      (b) the INCLUDE tap is LIVE BUT SILENT while nothing outside\n"
               "          the target's tree happened to make a sound.\n"
               "\n"
               "    WAS THE TARGET AUDIBLE while this ran? If NO, it is (a): start\n"
               "    audio and repeat. If YES, it is (b) — and that is a FINDING, the\n"
               "    same live-but-silent shape ADR-0043 D7 found on macOS. Record it\n"
               "    with the accepted Initialize rung printed above.\n");
    } else {
        printf("    NOT NARROWING — or unrelated audio contaminated the control.\n"
               "    Both passes carried audio. Either the tap is behaving like a\n"
               "    whole-system mix, or something other than the target was audible\n"
               "    during the control. %s\n", exc.wavWritten
            ? "LISTEN TO the exclude WAV to tell those\n    apart; only the first is a finding."
            : "The exclude pass wrote NO WAV, so\n    those two cannot be told apart from this run. Re-run before\n    recording either.");
    }
    if (inc.wavWritten && exc.wavWritten) {
        printf("\n  Listen to both WAVs either way. The counters say non-zero samples\n"
               "  arrived; only your ears say they were the right app.\n");
    } else {
        printf("\n  AUDIO IDENTITY CANNOT BE VERIFIED FROM THIS RUN.\n");
        if (!inc.wavWritten) printf("    include pass: no WAV was written\n");
        if (!exc.wavWritten) printf("    exclude pass: no WAV was written\n");
        printf("  The counters above stand on their own, but only listening says the\n"
               "  samples came from the right app -- and there is nothing here to\n"
               "  listen to. Do not treat any file at these paths as evidence from\n"
               "  this run.\n");
    }
    // Qualify that advice when a previous run's file could not be cleared: telling
    // the operator to listen to a WAV this run may never have written is how stale
    // audio becomes current evidence.
    if (inc.staleWavPath != nullptr || exc.staleWavPath != nullptr) {
        printf("\n  CAUTION — a previous run's WAV could not be deleted and is still on\n"
               "  disk:\n");
        if (inc.staleWavPath != nullptr) printf("    %s  (include)\n", inc.staleWavPath);
        if (exc.staleWavPath != nullptr) printf("    %s  (exclude)\n", exc.staleWavPath);
        printf("  If the matching pass above reported NO DATA, CAPTURE SETUP FAILED or\n"
               "  any other non-conclusive verdict, that file is NOT from this run. Do\n"
               "  not listen to it as evidence.\n");
    }
    printf("\n");

    CoUninitialize();
    return 0;
}
