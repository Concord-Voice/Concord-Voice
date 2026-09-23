#import <CoreAudio/CoreAudio.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <Foundation/Foundation.h>

#include <cstdio>
#include <cstdlib>

namespace {

OSStatus g_tapStatus = noErr;
bool g_tapWritesOutput = true;
OSStatus g_aggregateStatus = noErr;
bool g_aggregateWritesOutput = true;
OSStatus g_ioProcStatus = noErr;
bool g_ioProcWritesOutput = true;
AudioDeviceIOProc g_seenIoProc = nullptr;
void* g_seenIoProcCtx = nullptr;

OSStatus testAudioHardwareCreateProcessTap(CATapDescription*, AudioObjectID* outTap) {
  if (g_tapWritesOutput) { *outTap = 0xCAFEu; }
  return g_tapStatus;
}

OSStatus testAudioObjectGetPropertyData(AudioObjectID,
                                        const AudioObjectPropertyAddress* address,
                                        UInt32, const void*, UInt32* ioDataSize,
                                        void* outData) {
  if (address->mSelector == kAudioTapPropertyUID) {
    CFStringRef uid = CFStringCreateWithCString(kCFAllocatorDefault, "tap-uid",
                                                 kCFStringEncodingUTF8);
    *static_cast<CFStringRef*>(outData) = uid;
    *ioDataSize = sizeof(uid);
  }
  return noErr;
}

OSStatus testAudioHardwareCreateAggregateDevice(CFDictionaryRef, AudioObjectID* outAggregate) {
  if (g_aggregateWritesOutput) { *outAggregate = 0xBEEFu; }
  return g_aggregateStatus;
}

OSStatus testAudioDeviceCreateIOProcID(AudioObjectID, AudioDeviceIOProc proc, void* ctx,
                                       AudioDeviceIOProcID* outProcId) {
  g_seenIoProc = proc;
  g_seenIoProcCtx = ctx;
  if (g_ioProcWritesOutput) {
    *outProcId = reinterpret_cast<AudioDeviceIOProcID>(0xD00Du);
  }
  return g_ioProcStatus;
}

}  // namespace

#define AudioHardwareCreateProcessTap testAudioHardwareCreateProcessTap
#define AudioObjectGetPropertyData testAudioObjectGetPropertyData
#define AudioHardwareCreateAggregateDevice testAudioHardwareCreateAggregateDevice
#define AudioDeviceCreateIOProcID testAudioDeviceCreateIOProcID
#include "../rt/platform/macos/tap_backend.mm"
#undef AudioHardwareCreateProcessTap
#undef AudioObjectGetPropertyData
#undef AudioHardwareCreateAggregateDevice
#undef AudioDeviceCreateIOProcID

namespace {

int g_checks = 0;
int g_failures = 0;
int g_callbackCalls = 0;

#define CHECK(condition)                                                        \
  do {                                                                          \
    ++g_checks;                                                                 \
    if (!(condition)) {                                                         \
      ++g_failures;                                                             \
      std::fprintf(stderr, "CHECK failed: %s (%s:%d)\n", #condition,           \
                   __FILE__, __LINE__);                                        \
    }                                                                           \
  } while (false)

using concord::audiocap::rt::u32;
namespace macos = concord::audiocap::rt::macos;

void resetStubs() {
  g_tapStatus = noErr;
  g_tapWritesOutput = true;
  g_aggregateStatus = noErr;
  g_aggregateWritesOutput = true;
  g_ioProcStatus = noErr;
  g_ioProcWritesOutput = true;
  g_seenIoProc = nullptr;
  g_seenIoProcCtx = nullptr;
  g_callbackCalls = 0;
}

void callback(void*, const unsigned char* const*, unsigned short, bool, unsigned int,
              unsigned long long) noexcept {
  ++g_callbackCalls;
}

void testCreateTapForwardsErrorOutput() {
  resetStubs();
  g_tapStatus = static_cast<OSStatus>(-7001);
  const u32 objects[1] = {7u};
  u32 tap = 0x1111u;
  CHECK(macos::halCreateTap(objects, 1u, &tap) == static_cast<u32>(g_tapStatus));
  CHECK(tap == 0xCAFEu);
}

void testCreateTapPreservesUntouchedErrorOutput() {
  resetStubs();
  g_tapStatus = static_cast<OSStatus>(-7002);
  g_tapWritesOutput = false;
  const u32 objects[1] = {7u};
  u32 tap = 0x1111u;
  CHECK(macos::halCreateTap(objects, 1u, &tap) == static_cast<u32>(g_tapStatus));
  CHECK(tap == macos::kObjectUnknown);
}

void testCreateTapForwardsSuccessOutput() {
  resetStubs();
  const u32 objects[1] = {7u};
  u32 tap = 0x1111u;
  CHECK(macos::halCreateTap(objects, 1u, &tap) == 0u);
  CHECK(tap == 0xCAFEu);
}

void testCreateAggregateForwardsErrorOutput() {
  resetStubs();
  g_aggregateStatus = static_cast<OSStatus>(-7003);
  u32 aggregate = 0x2222u;
  CHECK(macos::halCreateAggregate(7u, &aggregate) == static_cast<u32>(g_aggregateStatus));
  CHECK(aggregate == 0xBEEFu);
}

void testCreateAggregatePreservesUntouchedErrorOutput() {
  resetStubs();
  g_aggregateStatus = static_cast<OSStatus>(-7004);
  g_aggregateWritesOutput = false;
  u32 aggregate = 0x2222u;
  CHECK(macos::halCreateAggregate(7u, &aggregate) == static_cast<u32>(g_aggregateStatus));
  CHECK(aggregate == macos::kObjectUnknown);
}

void testCreateAggregateForwardsSuccessOutput() {
  resetStubs();
  u32 aggregate = 0x2222u;
  CHECK(macos::halCreateAggregate(7u, &aggregate) == 0u);
  CHECK(aggregate == 0xBEEFu);
}

void testCreateIoProcForwardsErrorOutputAndClearsCallbackSlots() {
  resetStubs();
  g_ioProcStatus = static_cast<OSStatus>(-7005);
  void* procId = reinterpret_cast<void*>(0x3333u);
  CHECK(macos::halCreateIoProc(9u, &callback, reinterpret_cast<void*>(0x44u),
                               &procId) == static_cast<u32>(g_ioProcStatus));
  CHECK(procId == reinterpret_cast<void*>(0xD00Du));
  CHECK(g_seenIoProc != nullptr);
  CHECK(g_seenIoProcCtx == nullptr);
  CHECK(macos::ioProcThunk(0u, nullptr, nullptr, nullptr, nullptr, nullptr, nullptr) == noErr);
  CHECK(g_callbackCalls == 0);
}

void testCreateIoProcPreservesUntouchedErrorOutput() {
  resetStubs();
  g_ioProcStatus = static_cast<OSStatus>(-7006);
  g_ioProcWritesOutput = false;
  void* procId = reinterpret_cast<void*>(0x3333u);
  CHECK(macos::halCreateIoProc(9u, &callback, reinterpret_cast<void*>(0x44u),
                               &procId) == static_cast<u32>(g_ioProcStatus));
  CHECK(procId == nullptr);
}

void testCreateIoProcForwardsSuccessOutput() {
  resetStubs();
  void* procId = nullptr;
  CHECK(macos::halCreateIoProc(9u, &callback, reinterpret_cast<void*>(0x44u),
                               &procId) == 0u);
  CHECK(procId == reinterpret_cast<void*>(0xD00Du));
  CHECK(macos::ioProcThunk(0u, nullptr, nullptr, nullptr, nullptr, nullptr, nullptr) == noErr);
  CHECK(g_callbackCalls == 1);
}

}  // namespace

int main() {
  testCreateTapForwardsSuccessOutput();
  testCreateAggregateForwardsSuccessOutput();
  testCreateIoProcForwardsSuccessOutput();
  std::fprintf(stderr, "success controls passed\n");
  testCreateTapForwardsErrorOutput();
  testCreateTapPreservesUntouchedErrorOutput();
  testCreateAggregateForwardsErrorOutput();
  testCreateAggregatePreservesUntouchedErrorOutput();
  testCreateIoProcForwardsErrorOutputAndClearsCallbackSlots();
  testCreateIoProcPreservesUntouchedErrorOutput();
  std::printf("%d checks, %d failures\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}
