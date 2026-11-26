#pragma once

#include <cstddef>
#include <cstdint>

namespace civ6syncfix {

struct THashMapHeader {
    void*    vtableOrPad;   // +0x00
    void**   buckets;       // +0x08 (length = bucketCount + 1; [bucketCount] == (void*)-1)
    uint32_t bucketCount;   // +0x10
    uint32_t elemCount;     // +0x14
    uint8_t  rehashPolicy[12]; // +0x18
    uint8_t  allocCtx[16];     // +0x24
};

struct Pair16 {
    uint64_t key;
    void*    value;
};

struct Node24 {
    uint64_t key;
    void*    value;
    Node24*  next;
};

struct InsertResult {
    Node24*  node;
    Node24** bucketSlot;
    uint8_t  inserted;
    uint8_t  pad[7];
};

struct RehashDecision {
    uint8_t  needRehash;
    uint8_t  pad[3];
    uint32_t newBucketCount;
};

using FnRehashPolicy = void (*)(void* policy, RehashDecision* outDecision, uint32_t bucketCount,
                                uint32_t elemCount, uint32_t delta);
using FnGameAlloc = void* (*)(size_t size, uint32_t a2, uint32_t flags, uint32_t a4,
                              uint64_t a5, uint32_t a6);
using FnGameFree = void (*)(void* allocCtx, void* ptr, size_t size);

// Install all 64-bit hash patches into the host CivilizationVI process,
// plus GameCore_XP2_FinalRelease.dll bug fixes if GameCore is loaded.
// Idempotent across reloads; if any pattern fails verification, patches 0 bytes and returns false.
bool InstallSyncFixOnce();

// Install (or re-apply after GameCore unload/reload) the 3 native GameCore_XP2_FinalRelease.dll fixes:
//   1. AI Great Work rearrangement O(N^3) late-game lag cap (N > 32)
//   2. National Wonder protection (other civs' in-progress builds are not cancelled)
//   3. Religion AI uninitialized stack bool desync fix
// Safe to call repeatedly; fast no-op once the currently loaded GameCore module is patched.
bool InstallGameCoreFixesIfNeeded();

// Log a formatted line to %LOCALAPPDATA%\Firaxis Games\Sid Meier's Civilization VI\Logs\Civ6SyncFix.log.
void LogInfo(const char* fmt, ...);

constexpr int kPatternCount = 28;
constexpr int kGameCorePatternCount = 5;

// Exposed for offline verification / unit testing only.
struct ResolvedTargets {
    const uint8_t* textBase = nullptr;
    size_t         textSize = 0;
    size_t         offsets[kPatternCount]{};
    bool           found[kPatternCount]{};
    int            matchCounts[kPatternCount]{};
    int            matchedCount = 0;

    // Resolved engine helper RVAs (relative to textBase)
    intptr_t insert24Off = -1;
    intptr_t rehashPolicyOff = -1;
    intptr_t gameAllocOff = -1;
    intptr_t rehash24Off = -1;
    intptr_t gameFreeOff = -1;
};

struct GameCoreTargets {
    const uint8_t* textBase = nullptr;
    size_t         textSize = 0;
    size_t         offsets[kGameCorePatternCount]{};
    bool           found[kGameCorePatternCount]{};
    int            matchCounts[kGameCorePatternCount]{};
    int            matchedCount = 0;

    // Resolved internal RVAs (relative to textBase)
    intptr_t getBuildingsTableOff = -1;
    intptr_t nwNextPlayerOff = -1;
    intptr_t gwComboSkipOff = -1;
};

bool ScanTextSection(const uint8_t* textBase, size_t textSize, ResolvedTargets* out);
const char* PatternName(int index);

bool ScanGameCoreTextSection(const uint8_t* textBase, size_t textSize, GameCoreTargets* out);
const char* GameCorePatternName(int index);

// Test hooks to inject custom allocator/policy in standalone unit tests.
void SetEngineHelpersForTest(FnRehashPolicy policy, FnGameAlloc allocFn, FnGameFree freeFn);
InsertResult* InsertAutoArchive64(THashMapHeader* map, InsertResult* out, const Pair16* kv);
void RehashMap64(THashMapHeader* map, uint32_t newBucketCount, size_t nextOffset);

// Post-reload stale TurnSynchronizationBarrier guard hooks (exposed for unit tests).
void OnSnapshotLoadedHook(void* pSyncMgr);
bool ShouldSkipEndOfTurnBarrier();
void ResetPostReloadGuardForTest();

// Universal AutoVariable-level desync telemetry hooks (exposed for unit tests).
void OnDeltaDesyncVars(void* archive, const uint32_t* begin, const uint32_t* end);
void OnRequestFullSnapshot(void* pSyncMgr);

// GameCore_XP2 National Wonder helper (exposed for unit tests).
struct BuildingsTable {
    const uint8_t* const* begin;
    const uint8_t* const* end;
};
using FnGetBuildingsTable = const BuildingsTable* (*)();
void SetGetBuildingsTableForTest(FnGetBuildingsTable fn);
bool ShouldSkipWonderCancelForPlayer(int32_t buildingIndex, const void* completedCity, const void* pPlayer);

}  // namespace civ6syncfix
