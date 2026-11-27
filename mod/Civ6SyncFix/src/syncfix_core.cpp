#include "syncfix_core.h"

#include <windows.h>

#include <atomic>
#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <vector>

namespace civ6syncfix {

namespace {

enum PatternIdx {
    kAutoArchHelperLookup = 0,
    kAutoArchHelperCallIns,
    kRegisterLookup1,
    kRegisterLookup2,
    kRegisterCallIns1,
    kRegisterLookup3,
    kRegisterCallIns2,
    kUnregisterLookup,
    kHandleSnap1Lookup1,
    kHandleSnap1Lookup2,
    kHandleSnap1Lookup3,
    kHandleSnap1Lookup4,
    kHandleSnap2Lookup1,
    kHandleSnap2Lookup2,
    kHandleSnap2Lookup3,
    kProcessRecvLoop1,
    kProcessRecvLoop2,
    kSendSyncLookup1,
    kSendSyncLookup2,
    kSendSyncRehash,
    kDeserSyncLookup,
    kDeserSyncRehash,
    kStartGameSyncCheck,
    kOnSyncComplete,
    kOnSnapshotLoaded,
    kOnLoadCompleteBarrier,
    kDeltaDesyncVars,
    kRequestFullSnapshot,
    kCopySyncMapRehash,
    kCopySyncMapInsertLookup,
    kCopySyncMapInsertRehash,
};

struct PatternDef {
    const char* name;
    size_t      targetOffset;  // offset from pattern match start to instruction to patch
    const char* hexPattern;
};

static const PatternDef kPatterns[kPatternCount] = {
    {"AutoArch_Helper_Lookup", 19,
     "40 53 48 83 EC 50 44 8B 49 10 4C 8B DA 4C 8B 51 08 33 D2 41 8B 03 41 F7 F1 49 8B 04 D2 48 85 C0 74 11 4D 8B 03"},
    {"AutoArch_Helper_CallIns", 22,
     "4C 24 60 48 8D 54 24 30 48 89 44 24 20 48 C7 44 24 28 00 00 00 00 E8 CD 0C 00 00 48 8B 44 24 30 48 83 C0 08 48 83"},
    {"Register_Lookup1", 24,
     "8B 48 20 4C 8D 78 10 48 8B 40 18 33 D2 45 8B 4F 10 4C 8B D0 4C 8B 1C C8 41 8B C0 41 F7 F1 49 8B 04 D2 48 85 C0 74 11 0F 1F 00"},
    {"Register_Lookup2", 18,
     "8B 04 CA 49 3B C3 0F 84 D6 07 00 00 45 8B 4F 10 33 D2 41 8B C0 41 F7 F1 49 8B 14 D2 48 85 D2 74 0E 4C 3B 02 74 0D 48 8B"},
    {"Register_CallIns1", 24,
     "00 00 48 8D 55 E0 4C 89 44 24 70 49 8B CF 4C 8D 44 24 70 48 89 74 24 78 E8 ?? B4 FF FF 48 8B 55 E0 48 83 C2 08 4C 39 32"},
    {"Register_Lookup3", 14,
     "BC FC FF FF 4D 8B 46 58 33 D2 45 8B 4F 10 41 8B C0 4D 8B 57 08 41 F7 F1 49 8B 14 D2 48 85 D2 74 10 66 90 4C 3B 02"},
    {"Register_CallIns2", 24,
     "00 00 48 8D 55 E0 4C 89 44 24 70 49 8B CF 4C 8D 44 24 70 48 89 74 24 78 E8 ?? AC FF FF 48 8B 45 E0 48 8D 50 08 B0"},
    {"Unregister_Lookup", 20,
     "0F 84 59 08 00 00 44 8B 48 20 4C 8D 68 10 4D 8B 55 08 33 D2 41 8B C0 41 F7 F1 49 8B 04 D2 49 8D 14 D2 48 85 C0 74 12 4C"},
    {"HandleSnap1_Lookup1", 16,
     "00 E9 F0 FE FF FF 45 8B 56 20 33 D2 4D 8B 5E 18 41 8B C0 41 F7 F2 49 8B 14 D3 48 85 D2 74 10 66 90 4C 3B 02 74 0D 48 8B"},
    {"HandleSnap1_Lookup2", 18,
     "3A C8 0F 85 68 04 00 00 45 8B 4E 20 33 D2 4D 8B 56 18 41 8B C0 41 F7 F1 49 8B 3C D2 48 85 FF 74 0E 4C 3B 07 74 0D 48 8B 7F 10"},
    {"HandleSnap1_Lookup3", 18,
     "07 44 0F B6 4A 08 EB 49 45 8B 56 20 33 D2 4D 8B 5E 18 41 8B C0 41 F7 F2 49 8B 14 D3 48 85 D2 74 13 0F 1F 44 00 00 4C 3B 02 74"},
    {"HandleSnap1_Lookup4", 18,
     "01 33 F6 E9 10 09 00 00 45 8B 4E 20 33 D2 4D 8B 56 18 41 8B C0 41 F7 F1 49 8B 3C D2 48 85 FF 74 0E 4C 3B 07 74 0D 48 8B 7F 10"},
    {"HandleSnap2_Lookup1", 18,
     "07 44 0F B6 4A 08 EB 40 44 8B 53 20 33 D2 4C 8B 5B 18 41 8B C0 41 F7 F2 49 8B 14 D3 48 85 D2 74 0E 4C 3B 02 74 0D 48 8B 52 10"},
    {"HandleSnap2_Lookup2", 18,
     "3A C9 0F 85 BB 12 00 00 44 8B 4B 20 33 D2 4C 8B 53 18 41 8B C0 41 F7 F1 49 8B 3C D2 48 85 FF 74 0E 4C 3B 07 74 0D 48 8B 7F 10"},
    {"HandleSnap2_Lookup3", 16,
     "0F 84 2E 04 00 00 90 4C 8B 95 80 03 00 00 33 D2 41 8B 04 24 45 8B 42 20 4D 8B 4A 18 41 F7 F0 49 8B 04 D1 49 8D 14 D1 48 85 C0"},
    {"ProcessRecv_Loop1", 16,
     "41 BC 17 01 00 00 4C 3B 34 C1 0F 84 EE 08 00 00 8B 4E 20 33 D2 41 8B 06 4C 8B 46 18 F7 F1 49 8B 1C D0 48 85 DB 74 16 49"},
    {"ProcessRecv_Loop2", 18,
     "18 48 3B 3C C8 0F 84 EB 04 00 00 48 8B 9D B8 03 00 00 44 8B 43 50 33 D2 8B 07 4C 8B 4B 48 41 F7 F0 49 8B 14 D1 48 85 D2 74 15"},
    {"SendSync_Lookup1", 20,
     "90 00 00 00 44 8B 41 10 4C 8B D2 4C 8B 49 08 33 D2 48 8B F9 41 8B 02 41 F7 F0 49 8B 04 D1 48 85 C0 74 16 49 8B 0A 0F 1F"},
    {"SendSync_Lookup2", 12,
     "24 38 44 89 7C 24 40 E8 ?? ?? 84 FF 8B 44 24 58 33 D2 44 8B 47 10 48 8B 74 24 58 41 F7 F0 48 8B 47 08 8B EA 48 8B"},
    {"SendSync_Rehash", 14,
     "4C 89 7B 28 44 38 BC 24 A0 00 00 00 74 1E 8B 44 24 58 33 D2 F7 B4 24 A4 00 00 00 48 8B CF 8B EA 8B 94 24 A4 00 00 00 E8 ?? FD 89 FF 8B C5 48 8D"},
    {"DeserSync_Lookup", 16,
     "89 08 48 83 C0 04 48 83 EA 01 75 E9 48 8B 7D EF 44 8B 65 EF 33 D2 44 8B 46 10 41 8B C4 41 F7 F0 48 8B 46 08 44 89 75 0F"},
    {"DeserSync_Rehash", 14,
     "48 C7 43 28 00 00 00 00 80 7D 77 00 74 16 33 D2 41 8B C4 F7 75 7B 48 8B CE 44 8B F2 8B 55 7B E8 ?? ?? 88 FF 41 8B C6 4C"},
    {"StartGameSyncCheck", 0,
     "40 55 41 56 48 81 EC E8 02 00 00 8B 51 08 48 8B E9 48 8B 0D ?? ?? ?? 01 E8 ?? ?? 00 00 4C 8B F0"},
    {"OnSyncComplete", 0,
     "48 8B C4 48 89 58 20 55 56 57 48 81 EC 90 02 00 00 48 8B E9 4C 89 70 18 48 8D 48 08 48 C7 40 08 01 00 00 00"},
    {"OnSnapshotLoaded", 0,
     "40 53 48 83 EC 60 80 79 38 00 48 8B D9 74 78 8B 51 08 48 8B 0D ?? ?? ?? ?? 48 89 7C 24 70 E8 ?? ?? ?? ?? C6 43 38 00 48"},
    {"OnLoadComplete_Barrier", 0,
     "41 80 7F 08 00 0F 85 5F 02 00 00 48 8B 03 48 8B CB FF 90 F0 00 00 00 84 C0 0F 84 4B 02 00 00"},
    // Offset +3 (`mov rbx, [rbp-0x10]; cmp rbx, [rbp-8]`, 8B) avoids mid-patch jump targets
    // at site+3 from the empty-desyncedVars logging paths (DX11 0x7F46F3/0x7F4B9B, DX12 0x7F5903/0x7F5DAB).
    {"DeltaDesyncVars", 3,
     "45 33 FF 48 8B 5D F0 48 3B 5D F8 74 15 8B 13 48 8B 4F 08 E8 ?? ?? ?? ?? 48 83 C3 04 48 3B 5D F8 75 EB"},
    {"RequestFullSnapshot", 0,
     "48 89 5C 24 08 57 48 83 EC 60 8B 51 08 48 8B D9 48 8B 0D ?? ?? ?? ?? E8 ?? ?? ?? ?? C6 43 38 01"},
    // Pending-queue NetGameSyncCheck::m_Archives operator= copy path (THashMap<uint64_t, ArchiveSyncInfo>, 48-byte nodes)
    {"CopySyncMap_Rehash", 24,
     "48 8D 54 24 60 E8 ?? ?? ?? ?? 80 7C 24 60 00 74 0C 8B 54 24 64 48 8B CE E8 ?? ?? ?? ?? 48 8B 07 48 3B 45 00 74 66"},
    {"CopySyncMap_Insert_Lookup", 4,
     "48 83 EC 30 45 8B 30 48 8B DA 44 8B 51 10 33 D2 41 8B C6 49 8B F0 41 F7 F2 48 8B 41 08 48 8B F9 8B EA 4C 8D 04 D0 48 8B 04 D0 48 85 C0 74 15 48 8B 0E 48 3B 08 0F 84 9B 00 00 00 48 8B 40 28"},
    {"CopySyncMap_Insert_Rehash", 10,
     "80 7C 24 50 00 48 8B F0 74 17 33 D2 41 8B C6 F7 74 24 54 48 8B CF 8B EA 8B 54 24 54 E8 ?? ?? ?? ?? 48 8B 47 08 8B CD 48 8D 14 CD 00 00 00 00 48 8B 0C 02 48 89 4E 28"},
};

FnRehashPolicy      g_fnRehashPolicy    = nullptr;
FnGameAlloc         g_fnAlloc           = nullptr;
FnGameFree          g_fnFree            = nullptr;
FnGetBuildingsTable g_fnGetBuildingsTbl = nullptr;

enum GameCorePatternIdx {
    kGcGwPreAlloc = 0,
    kGcGwComboLoop,
    kGcNwPlayerLoop,
    kGcNwGetBuildingsTable,
    kGcRelUninitBool,
};

static const PatternDef kGameCorePatterns[kGameCorePatternCount] = {
    // GreatWorkCollectionAnalysis::Execute — pre-allocation `imul ecx, ebx` (N*N -> cap at N when N > 32)
    {"GC_GW_PreAlloc", 18,
     "8B 44 24 58 48 C1 FA 02 48 8B CA 48 C1 E9 3F 48 03 CA 0F AF CB 48 3B C1 73 0F 8B D1 41 B0 01 48 8D 4C 24 50"},
    // GreatWorkCollectionAnalysis::Execute — 2-work/3-work combinatorial loop guard (`r14d == 0 || r14d > 32`)
    {"GC_GW_ComboLoop", 12,
     "89 75 A8 48 85 F6 0F 85 24 FD FF FF 44 8B 74 24 60 45 85 F6 0F 84 99 02 00 00 BA 01 00 00 00 89 95 50 03 00 00"},
    // OnWonderCompleted — player loop start (skip cancelling other civs' builds for National Wonders)
    {"GC_NW_PlayerLoop", 8,
     "48 8B 00 48 89 44 24 48 48 8B 98 D0 06 00 00 48 8B 9B D0 00 00 00 48 85 DB 0F 84 D9 03 00 00 90 48 8B 0B 8B D6"},
    // OnWonderCompleted — call to GetBuildingsTable() (`call rel32; mov r8d, esi; mov rdx, [rax]; ...`)
    {"GC_NW_GetBuildingsTbl", 0,
     "E8 ?? ?? ?? 00 44 8B C6 48 8B 10 48 8B 48 08 48 2B CA 48 C1 F9 03 4C 3B C1 73 06 4A 8B 3C C2 EB 02 33 FF"},
    // AI::Religion off-turn update — uninitialized stack bool at [rsp+0x60] before copying into synced m_conversionInfo
    {"GC_Rel_UninitBool", 17,
     "66 44 89 64 24 40 66 44 89 6C 24 42 4C 89 74 24 58 44 89 74 24 64 44 89 74 24 78 E8 ?? ?? ?? 00 48 8B F8 8B 40 10 8B 57 08 3B C2"},
};

SRWLOCK g_logLock      = SRWLOCK_INIT;
SRWLOCK g_gameCoreLock = SRWLOCK_INIT;
wchar_t g_logPath[MAX_PATH * 2]{};
bool    g_logPathReady = false;

HMODULE        g_patchedGameCoreMod      = nullptr;
const uint8_t* g_patchedGameCoreSentinel = nullptr;
HMODULE        g_failedGameCoreMod       = nullptr;

std::atomic<bool> g_installAttempted{false};
std::atomic<bool> g_installSucceeded{false};
std::atomic<int64_t> g_syncStartQpc{0};
std::atomic<bool> g_skipNextEndOfTurnBarrier{false};
std::atomic<uint32_t> g_deltaLogsThisTurn{0};
int64_t g_qpcFreq = 0;

void EnsureLogPath() {
    if (g_logPathReady) return;
    wchar_t localApp[MAX_PATH]{};
    DWORD n = GetEnvironmentVariableW(L"LOCALAPPDATA", localApp, MAX_PATH);
    if (n > 0 && n < MAX_PATH) {
        _snwprintf(g_logPath, MAX_PATH * 2 - 1,
                   L"%s\\Firaxis Games\\Sid Meier's Civilization VI\\Logs\\Civ6SyncFix.log",
                   localApp);
    } else {
        wcscpy(g_logPath, L"Civ6SyncFix.log");
    }
    g_logPathReady = true;
}

int ParseHexByte(char hi, char lo) {
    auto v = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        return -1;
    };
    int a = v(hi), b = v(lo);
    return (a < 0 || b < 0) ? -1 : ((a << 4) | b);
}

void ParsePattern(const char* hex, std::vector<uint8_t>& bytes, std::vector<uint8_t>& mask) {
    bytes.clear();
    mask.clear();
    for (const char* p = hex; *p;) {
        while (*p == ' ' || *p == '\t') ++p;
        if (!*p) break;
        if (p[0] == '?' && p[1] == '?') {
            bytes.push_back(0);
            mask.push_back(0);
            p += 2;
        } else {
            int val = ParseHexByte(p[0], p[1]);
            bytes.push_back(static_cast<uint8_t>(val < 0 ? 0 : val));
            mask.push_back(1);
            p += 2;
        }
    }
}

int32_t ReadRel32(const uint8_t* p) {
    int32_t v = 0;
    std::memcpy(&v, p, sizeof(v));
    return v;
}

// Allocate one 4 KB RWX page within +/- 1.25 GB of anchor so E9/E8/0F 85 rel32 always reaches the whole .text section.
uint8_t* AllocNearPage(uintptr_t anchor) {
    constexpr uintptr_t kGran = 0x10000;
    constexpr uintptr_t kMaxDist = 0x50000000ULL;  // 1.25 GB (< 2 GB rel32 limit)

    uintptr_t lo = (anchor > kMaxDist) ? ((anchor - kMaxDist) & ~(kGran - 1)) : kGran;
    uintptr_t hi = anchor + kMaxDist;

    // Scan upward from anchor first, then downward.
    for (int dir = 0; dir < 2; ++dir) {
        uintptr_t cur = (dir == 0) ? ((anchor + kGran) & ~(kGran - 1))
                                   : ((anchor - kGran) & ~(kGran - 1));
        while ((dir == 0 && cur < hi) || (dir == 1 && cur >= lo)) {
            MEMORY_BASIC_INFORMATION mbi{};
            if (VirtualQuery(reinterpret_cast<void*>(cur), &mbi, sizeof(mbi)) == 0) {
                break;
            }
            if (mbi.State == MEM_FREE && mbi.RegionSize >= 0x1000) {
                uintptr_t cand = (reinterpret_cast<uintptr_t>(mbi.BaseAddress) + kGran - 1) & ~(kGran - 1);
                if (cand >= lo && cand < hi) {
                    void* mem = VirtualAlloc(reinterpret_cast<void*>(cand), 0x1000,
                                             MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE);
                    if (mem) {
                        std::memset(mem, 0xCC, 0x1000);
                        return static_cast<uint8_t*>(mem);
                    }
                }
            }
            if (dir == 0) {
                uintptr_t next = reinterpret_cast<uintptr_t>(mbi.BaseAddress) + mbi.RegionSize;
                if (next <= cur) break;
                cur = next;
            } else {
                uintptr_t base = reinterpret_cast<uintptr_t>(mbi.BaseAddress);
                if (base < kGran || base <= lo) break;
                cur = base - kGran;
            }
        }
    }
    return nullptr;
}

struct StubBuilder {
    uint8_t* base = nullptr;
    size_t   pos  = 0;
    size_t   cap  = 0;

    uint8_t* cur() const { return base + pos; }

    void align16() {
        while ((pos & 15) != 0 && pos < cap) {
            base[pos++] = 0xCC;
        }
    }

    void emitBytes(const uint8_t* src, size_t n) {
        std::memcpy(base + pos, src, n);
        pos += n;
    }

    template <size_t N>
    void emit(const uint8_t (&arr)[N]) {
        emitBytes(arr, N);
    }

    void emitU64(uint64_t v) {
        std::memcpy(base + pos, &v, sizeof(v));
        pos += sizeof(v);
    }

    void emitRel32Branch(uint8_t opcode, const uint8_t* dst) {
        base[pos++] = opcode;
        intptr_t rel = reinterpret_cast<intptr_t>(dst) - reinterpret_cast<intptr_t>(base + pos + 4);
        int32_t r32 = static_cast<int32_t>(rel);
        std::memcpy(base + pos, &r32, 4);
        pos += 4;
    }

    void emitRel32Jcc(uint8_t cc, const uint8_t* dst) {
        base[pos++] = 0x0F;
        base[pos++] = cc;
        intptr_t rel = reinterpret_cast<intptr_t>(dst) - reinterpret_cast<intptr_t>(base + pos + 4);
        int32_t r32 = static_cast<int32_t>(rel);
        std::memcpy(base + pos, &r32, 4);
        pos += 4;
    }
};

bool WriteCodeBytes(uint8_t* dst, const uint8_t* src, size_t n) {
    DWORD oldProt = 0;
    if (!VirtualProtect(dst, n, PAGE_EXECUTE_READWRITE, &oldProt)) {
        return false;
    }
    std::memcpy(dst, src, n);
    DWORD dummy = 0;
    VirtualProtect(dst, n, oldProt, &dummy);
    FlushInstructionCache(GetCurrentProcess(), dst, n);
    return true;
}

bool WriteByteAt(uint8_t* dst, uint8_t val) {
    return WriteCodeBytes(dst, &val, 1);
}

bool PatchJmpRel32(uint8_t* site, size_t origLen, const uint8_t* target) {
    if (origLen < 5 || origLen > 64) return false;
    uint8_t buf[64];
    buf[0] = 0xE9;
    intptr_t rel = reinterpret_cast<intptr_t>(target) - reinterpret_cast<intptr_t>(site + 5);
    if (rel < INT32_MIN || rel > INT32_MAX) return false;
    int32_t r32 = static_cast<int32_t>(rel);
    std::memcpy(buf + 1, &r32, 4);
    for (size_t i = 5; i < origLen; ++i) buf[i] = 0x90;
    return WriteCodeBytes(site, buf, origLen);
}

bool PatchCallRel32(uint8_t* site, const uint8_t* target) {
    uint8_t buf[5];
    buf[0] = 0xE8;
    intptr_t rel = reinterpret_cast<intptr_t>(target) - reinterpret_cast<intptr_t>(site + 5);
    if (rel < INT32_MIN || rel > INT32_MAX) return false;
    int32_t r32 = static_cast<int32_t>(rel);
    std::memcpy(buf + 1, &r32, 4);
    return WriteCodeBytes(site, buf, 5);
}

void Rehash48_64(THashMapHeader* map, uint32_t newBucketCount) {
    RehashMap64(map, newBucketCount, 0x28);
}

void InspectAutoArchiveStats(void* pSyncMgr, uint32_t* outElems, uint32_t* outBuckets,
                             uint32_t* outMaxChain, uint32_t* outMisplaced) {
    *outElems = *outBuckets = *outMaxChain = *outMisplaced = 0;
    if (!pSyncMgr) return;
    auto* map = reinterpret_cast<THashMapHeader*>(reinterpret_cast<uint8_t*>(pSyncMgr) + 0x10);
    if (!map->buckets || map->bucketCount == 0 || map->bucketCount > 10000000U) return;

    *outElems = map->elemCount;
    *outBuckets = map->bucketCount;
    uint32_t maxChain = 0;
    uint32_t misplaced = 0;
    for (uint32_t i = 0; i < map->bucketCount; ++i) {
        uint32_t len = 0;
        for (Node24* n = reinterpret_cast<Node24*>(map->buckets[i]); n != nullptr; n = n->next) {
            ++len;
            if ((n->key % map->bucketCount) != i) {
                ++misplaced;
            }
            if (len > 200000U) break;
        }
        if (len > maxChain) maxChain = len;
    }
    *outMaxChain = maxChain;
    *outMisplaced = misplaced;
}

void OnStartGameSyncCheck(void* pSyncMgr) {
    // Ensure GameCore_XP2 fixes are active (handles GameCore unload/reload across sessions).
    InstallGameCoreFixesIfNeeded();

    // Any live sync start supersedes any pending post-resync reload guard.
    g_skipNextEndOfTurnBarrier.store(false, std::memory_order_release);
    g_deltaLogsThisTurn.store(0, std::memory_order_relaxed);

    LARGE_INTEGER now{};
    QueryPerformanceCounter(&now);
    g_syncStartQpc.store(now.QuadPart, std::memory_order_relaxed);

    uint32_t elems = 0, buckets = 0, maxChain = 0, misplaced = 0;
    InspectAutoArchiveStats(pSyncMgr, &elems, &buckets, &maxChain, &misplaced);

    // Belt-and-suspenders: if any archive was somehow registered before our DLL attached,
    // re-bucket m_AutoArchives in place right now so every lookup is guaranteed consistent.
    if (misplaced > 0 && pSyncMgr && g_fnAlloc && g_fnFree) {
        auto* map = reinterpret_cast<THashMapHeader*>(reinterpret_cast<uint8_t*>(pSyncMgr) + 0x10);
        RehashMap64(map, map->bucketCount, 0x10);
        LogInfo("Re-bucketed %u pre-existing AutoArchives into 64-bit hash layout (buckets=%u).",
                misplaced, map->bucketCount);
        InspectAutoArchiveStats(pSyncMgr, &elems, &buckets, &maxChain, &misplaced);
    }

    LogInfo("SyncStart: archives=%u buckets=%u max_chain=%u misplaced=%u",
            elems, buckets, maxChain, misplaced);
}

void OnSyncCompleteHook(void* pSyncMgr) {
    LARGE_INTEGER now{};
    QueryPerformanceCounter(&now);
    int64_t start = g_syncStartQpc.load(std::memory_order_relaxed);
    double ms = (start > 0 && g_qpcFreq > 0)
                    ? (static_cast<double>(now.QuadPart - start) * 1000.0 / static_cast<double>(g_qpcFreq))
                    : 0.0;
    uint32_t elems = 0, buckets = 0, maxChain = 0, misplaced = 0;
    InspectAutoArchiveStats(pSyncMgr, &elems, &buckets, &maxChain, &misplaced);
    LogInfo("SyncComplete: %.2f ms (archives=%u buckets=%u max_chain=%u)",
            ms, elems, buckets, maxChain);
}

bool GetExeTextSection(HMODULE exeMod, uint8_t** outBase, size_t* outSize) {
    auto* base = reinterpret_cast<uint8_t*>(exeMod);
    auto* dos = reinterpret_cast<const IMAGE_DOS_HEADER*>(base);
    if (dos->e_magic != IMAGE_DOS_SIGNATURE) return false;
    auto* nt = reinterpret_cast<const IMAGE_NT_HEADERS64*>(base + dos->e_lfanew);
    if (nt->Signature != IMAGE_NT_SIGNATURE) return false;
    const IMAGE_SECTION_HEADER* sec = IMAGE_FIRST_SECTION(nt);
    for (WORD i = 0; i < nt->FileHeader.NumberOfSections; ++i) {
        if (std::memcmp(sec[i].Name, ".text", 5) == 0) {
            *outBase = base + sec[i].VirtualAddress;
            *outSize = sec[i].Misc.VirtualSize ? sec[i].Misc.VirtualSize : sec[i].SizeOfRawData;
            return true;
        }
    }
    return false;
}

}  // namespace

const char* PatternName(int index) {
    if (index < 0 || index >= kPatternCount) return "invalid";
    return kPatterns[index].name;
}

const char* GameCorePatternName(int index) {
    if (index < 0 || index >= kGameCorePatternCount) return "invalid";
    return kGameCorePatterns[index].name;
}

void SetEngineHelpersForTest(FnRehashPolicy policy, FnGameAlloc allocFn, FnGameFree freeFn) {
    g_fnRehashPolicy = policy;
    g_fnAlloc = allocFn;
    g_fnFree = freeFn;
}

void SetGetBuildingsTableForTest(FnGetBuildingsTable fn) {
    g_fnGetBuildingsTbl = fn;
}

bool ShouldSkipWonderCancelForPlayer(int32_t buildingIndex, const void* completedCity, const void* pPlayer) {
    if (buildingIndex < 0 || !completedCity || !pPlayer || !g_fnGetBuildingsTbl) {
        return false;
    }
    const BuildingsTable* tbl = g_fnGetBuildingsTbl();
    if (!tbl || !tbl->begin || !tbl->end || tbl->end <= tbl->begin) {
        return false;
    }
    const size_t count = static_cast<size_t>(tbl->end - tbl->begin);
    if (static_cast<size_t>(buildingIndex) >= count) {
        return false;
    }
    const uint8_t* info = tbl->begin[buildingIndex];
    if (!info) return false;

    // BuildingInfo layout verified from BuildingInfo::LoadFromSQLRow (GameCore_XP2_FinalRelease.dll):
    //   +0x158 bit 6 (0x40): IsWonder
    //   +0x3C  int32:        MaxPlayerInstances (1 for National Wonder)
    //   +0x40  int32:        MaxWorldInstances  (-1 for National Wonder)
    const bool isWonder = (info[0x158] & 0x40U) != 0;
    int32_t maxPlayer = 0;
    int32_t maxWorld = 0;
    std::memcpy(&maxPlayer, info + 0x3C, sizeof(maxPlayer));
    std::memcpy(&maxWorld, info + 0x40, sizeof(maxWorld));
    if (!isWonder || maxPlayer != 1 || maxWorld != -1) {
        return false;
    }

    int16_t completedOwner = -1;
    int32_t iterPlayerId = -1;
    std::memcpy(&completedOwner, completedCity, sizeof(completedOwner));
    std::memcpy(&iterPlayerId, reinterpret_cast<const uint8_t*>(pPlayer) + 0xD8, sizeof(iterPlayerId));

    if (iterPlayerId != static_cast<int32_t>(completedOwner)) {
        LogInfo("NationalWonderFix: preserved player %d's in-progress buildingIdx=%d (completed by player %d).",
                iterPlayerId, buildingIndex, static_cast<int>(completedOwner));
        return true;
    }
    return false;
}

void OnSnapshotLoadedHook(void* pSyncMgr) {
    if (!pSyncMgr) return;
    const uint8_t reqFlag = *(reinterpret_cast<const uint8_t*>(pSyncMgr) + 0x38);
    if (reqFlag != 0) {
        // Mid-game resync NetSnapshot (requested by RequestFullSnapshot) arrived.
        // The engine is about to clear [pSyncMgr+0x38] and send NetGameSyncProcessed,
        // finishing THIS turn's sync on both Host and Client. When async snapshot loading
        // later invokes GameCoreController::OnLoadComplete, suppress the stale end-of-turn
        // TurnSynchronizationBarrier so Client doesn't send a Turn N NetGameSyncCheck
        // while Host is already on Turn N+1 (Firaxis vanilla 2-turn oscillation bug).
        g_skipNextEndOfTurnBarrier.store(true, std::memory_order_release);
        LogInfo("Resync NetSnapshot received (m_bRequestedSnapshot=1): armed post-reload stale-barrier guard.");
    } else {
        // Non-resync snapshot (e.g. lobby start / save load / hot-join): ensure guard is disarmed.
        g_skipNextEndOfTurnBarrier.store(false, std::memory_order_release);
    }
}

bool ShouldSkipEndOfTurnBarrier() {
    InstallGameCoreFixesIfNeeded();
    if (g_skipNextEndOfTurnBarrier.exchange(false, std::memory_order_acq_rel)) {
        LogInfo("Post-resync reload: skipped stale TurnSynchronizationBarrier (prevented 2-turn desync oscillation).");
        return true;
    }
    return false;
}

void ResetPostReloadGuardForTest() {
    g_skipNextEndOfTurnBarrier.store(false, std::memory_order_release);
}

namespace {

struct FireWireString {
    const char* begin;
    const char* end;
    const char* cap;
};

using FnGetAutoVarName = const FireWireString* (*)(void* archive, void* varPtr);

void CopySanitizedRange(const char* begin, const char* end, char* out, size_t outCap) {
    if (!out || outCap == 0) return;
    out[0] = '\0';
    if (!begin || !end || end <= begin) return;
    size_t len = static_cast<size_t>(end - begin);
    if (len > 96) len = 96;
    if (len >= outCap) len = outCap - 1;
    size_t w = 0;
    for (size_t i = 0; i < len; ++i) {
        unsigned char c = static_cast<unsigned char>(begin[i]);
        if (c == 0) break;
        out[w++] = (c >= 32 && c < 127) ? static_cast<char>(c) : '?';
    }
    out[w] = '\0';
}

}  // namespace

void OnDeltaDesyncVars(void* archive, const uint32_t* begin, const uint32_t* end) {
    if (!archive) return;
    // Cap per-turn detailed telemetry to avoid flooding disk on massive cascading desyncs.
    uint32_t seq = g_deltaLogsThisTurn.fetch_add(1, std::memory_order_relaxed);
    if (seq >= 64) {
        if (seq == 64) {
            LogInfo("DESYNC_DELTA: (further delta field logs suppressed for this turn, >64 archives)");
        }
        return;
    }

    const auto* raw = reinterpret_cast<const uint8_t*>(archive);
    uint64_t key = 0;
    std::memcpy(&key, raw + 0x58, sizeof(key));
    const uint32_t playerId = static_cast<uint32_t>(key & 0xFFFFU);
    const uint32_t typeId   = static_cast<uint32_t>((key >> 16) & 0xFFFFU);
    const uint32_t instId   = static_cast<uint32_t>(key >> 32);

    const char* typeBegin = nullptr;
    const char* typeEnd   = nullptr;
    std::memcpy(&typeBegin, raw + 0x60, sizeof(typeBegin));
    std::memcpy(&typeEnd,   raw + 0x68, sizeof(typeEnd));
    char typeName[100]{};
    CopySanitizedRange(typeBegin, typeEnd, typeName, sizeof(typeName));
    if (typeName[0] == '\0') {
        std::strcpy(typeName, "Unknown");
    }

    void** varsBegin = nullptr;
    void** varsEnd   = nullptr;
    std::memcpy(&varsBegin, raw + 0x08, sizeof(varsBegin));
    std::memcpy(&varsEnd,   raw + 0x10, sizeof(varsEnd));
    size_t totalVars = (varsBegin && varsEnd && varsEnd >= varsBegin)
                           ? static_cast<size_t>(varsEnd - varsBegin)
                           : 0;

    void** vtable = nullptr;
    std::memcpy(&vtable, raw, sizeof(vtable));
    FnGetAutoVarName fnGetName = (vtable && vtable[1])
                                     ? reinterpret_cast<FnGetAutoVarName>(vtable[1])
                                     : nullptr;

    size_t mismatchCount = (begin && end && end > begin) ? static_cast<size_t>(end - begin) : 0;
    char fieldsBuf[1024]{};
    size_t pos = 0;
    size_t shown = mismatchCount > 16 ? 16 : mismatchCount;

    for (size_t i = 0; i < shown; ++i) {
        uint32_t idx = begin[i];
        char varName[100]{};
        if (idx < totalVars && varsBegin && varsBegin[idx] && fnGetName) {
            const FireWireString* fs = fnGetName(archive, varsBegin[idx]);
            if (fs) {
                CopySanitizedRange(fs->begin, fs->end, varName, sizeof(varName));
            }
        }
        const char* sep = (i == 0) ? "" : ", ";
        int n = std::snprintf(fieldsBuf + pos, sizeof(fieldsBuf) - pos, "%s#%u:%s",
                              sep, idx, varName[0] ? varName : "<unnamed>");
        if (n <= 0 || static_cast<size_t>(n) >= sizeof(fieldsBuf) - pos) break;
        pos += static_cast<size_t>(n);
    }
    if (mismatchCount > shown && pos + 8 < sizeof(fieldsBuf)) {
        std::snprintf(fieldsBuf + pos, sizeof(fieldsBuf) - pos, ", ...(+%zu)", mismatchCount - shown);
    }

    LogInfo("DESYNC_DELTA: archive=%s (%u-%u-%u key=0x%llX) vars[%zu]=[%s]",
            typeName, playerId, typeId, instId,
            static_cast<unsigned long long>(key), mismatchCount, fieldsBuf);
}

void OnRequestFullSnapshot(void* pSyncMgr) {
    uint32_t elems = 0, buckets = 0, maxChain = 0, misplaced = 0;
    InspectAutoArchiveStats(pSyncMgr, &elems, &buckets, &maxChain, &misplaced);
    LogInfo("RESYNC_REQUEST: Client requesting full snapshot (NetSnapshotRequest | archives=%u buckets=%u max_chain=%u).",
            elems, buckets, maxChain);
}

void LogInfo(const char* fmt, ...) {
    AcquireSRWLockExclusive(&g_logLock);
    EnsureLogPath();

    SYSTEMTIME st{};
    GetLocalTime(&st);

    FILE* f = _wfopen(g_logPath, L"a");
    if (f) {
        fprintf(f, "[%04u-%02u-%02u %02u:%02u:%02u.%03u] [Civ6SyncFix] ",
                st.wYear, st.wMonth, st.wDay, st.wHour, st.wMinute, st.wSecond, st.wMilliseconds);
        va_list ap;
        va_start(ap, fmt);
        vfprintf(f, fmt, ap);
        va_end(ap);
        fputc('\n', f);
        fclose(f);
    }
    ReleaseSRWLockExclusive(&g_logLock);
}

void RehashMap64(THashMapHeader* map, uint32_t newBucketCount, size_t nextOffset) {
    size_t allocBytes = static_cast<size_t>(newBucketCount + 1) * sizeof(void*);
    void** newBuckets = reinterpret_cast<void**>(g_fnAlloc(allocBytes, 0, 0x400000, 0, 0, 0));
    std::memset(newBuckets, 0, static_cast<size_t>(newBucketCount) * sizeof(void*));
    newBuckets[newBucketCount] = reinterpret_cast<void*>(~static_cast<uintptr_t>(0));

    uint32_t oldCount = map->bucketCount;
    void** oldBuckets = map->buckets;
    for (uint32_t i = 0; i < oldCount; ++i) {
        uint8_t* node = reinterpret_cast<uint8_t*>(oldBuckets[i]);
        while (node != nullptr) {
            auto** nextField = reinterpret_cast<uint8_t**>(node + nextOffset);
            uint8_t* next = *nextField;
            uint64_t key = *reinterpret_cast<const uint64_t*>(node);
            uint64_t idx = key % newBucketCount;
            *nextField = reinterpret_cast<uint8_t*>(newBuckets[idx]);
            newBuckets[idx] = node;
            node = next;
        }
    }

    if (oldCount > 1) {
        g_fnFree(map->allocCtx, oldBuckets, static_cast<size_t>(oldCount + 1) * sizeof(void*));
    }
    map->bucketCount = newBucketCount;
    map->buckets = newBuckets;
}

InsertResult* InsertAutoArchive64(THashMapHeader* map, InsertResult* out, const Pair16* kv) {
    const uint64_t key = kv->key;
    const uint32_t bcount = map->bucketCount;
    uint64_t bucket = key % bcount;

    auto** slot = reinterpret_cast<Node24**>(&map->buckets[bucket]);
    for (Node24* cur = *slot; cur != nullptr; cur = cur->next) {
        if (cur->key == key) {
            out->node = cur;
            out->bucketSlot = slot;
            out->inserted = 0;
            return out;
        }
    }

    RehashDecision dec{};
    g_fnRehashPolicy(map->rehashPolicy, &dec, bcount, map->elemCount, 1);

    auto* node = reinterpret_cast<Node24*>(g_fnAlloc(sizeof(Node24), 0, 0, 0, 0, 0));
    if (node) {
        node->key = kv->key;
        node->value = kv->value;
    }
    node->next = nullptr;

    if (dec.needRehash != 0) {
        bucket = key % dec.newBucketCount;
        RehashMap64(map, dec.newBucketCount, offsetof(Node24, next));
    }

    auto** dstSlot = reinterpret_cast<Node24**>(&map->buckets[bucket]);
    node->next = *dstSlot;
    *dstSlot = node;
    map->elemCount += 1;

    out->node = node;
    out->bucketSlot = dstSlot;
    out->inserted = 1;
    return out;
}

bool ScanTextSection(const uint8_t* textBase, size_t textSize, ResolvedTargets* out) {
    *out = ResolvedTargets{};
    out->textBase = textBase;
    out->textSize = textSize;

    std::vector<uint8_t> patBytes;
    std::vector<uint8_t> patMask;

    for (int p = 0; p < kPatternCount; ++p) {
        ParsePattern(kPatterns[p].hexPattern, patBytes, patMask);
        const size_t plen = patBytes.size();
        if (plen == 0 || plen > textSize) continue;

        // Find first non-wildcard byte as anchor.
        size_t anchor = 0;
        while (anchor < plen && patMask[anchor] == 0) ++anchor;
        const uint8_t anchorByte = patBytes[anchor];

        int count = 0;
        size_t firstHit = 0;
        const size_t limit = textSize - plen;
        for (size_t i = 0; i <= limit; ++i) {
            if (textBase[i + anchor] != anchorByte) continue;
            bool match = true;
            for (size_t j = 0; j < plen; ++j) {
                if (patMask[j] && textBase[i + j] != patBytes[j]) {
                    match = false;
                    break;
                }
            }
            if (match) {
                if (count == 0) firstHit = i + kPatterns[p].targetOffset;
                ++count;
            }
        }
        out->matchCounts[p] = count;
        if (count == 1) {
            out->found[p] = true;
            out->offsets[p] = firstHit;
            out->matchedCount++;
        }
    }

    if (out->matchedCount != kPatternCount) {
        return false;
    }

    // Resolve internal engine helpers via relative call offsets from Insert24 (0x7F1D30 / 0x7F2F30).
    // All three CallIns sites must target the exact same Insert24 function.
    const size_t c0 = out->offsets[kAutoArchHelperCallIns];
    const size_t c1 = out->offsets[kRegisterCallIns1];
    const size_t c2 = out->offsets[kRegisterCallIns2];
    if (textBase[c0] != 0xE8 || textBase[c1] != 0xE8 || textBase[c2] != 0xE8) return false;

    const intptr_t ins0 = static_cast<intptr_t>(c0) + 5 + ReadRel32(textBase + c0 + 1);
    const intptr_t ins1 = static_cast<intptr_t>(c1) + 5 + ReadRel32(textBase + c1 + 1);
    const intptr_t ins2 = static_cast<intptr_t>(c2) + 5 + ReadRel32(textBase + c2 + 1);
    if (ins0 != ins1 || ins0 != ins2 || ins0 < 0 || static_cast<size_t>(ins0 + 0xD0) >= textSize) {
        return false;
    }
    out->insert24Off = ins0;

    // Inside Insert24: +0x70 = call RehashPolicy, +0x91 = call Alloc, +0xC6 = call Rehash24.
    if (textBase[ins0 + 0x70] != 0xE8 || textBase[ins0 + 0x91] != 0xE8 || textBase[ins0 + 0xC6] != 0xE8) {
        return false;
    }
    out->rehashPolicyOff = ins0 + 0x70 + 5 + ReadRel32(textBase + ins0 + 0x71);
    out->gameAllocOff    = ins0 + 0x91 + 5 + ReadRel32(textBase + ins0 + 0x92);
    out->rehash24Off     = ins0 + 0xC6 + 5 + ReadRel32(textBase + ins0 + 0xC7);

    if (out->rehashPolicyOff < 0 || static_cast<size_t>(out->rehashPolicyOff) >= textSize) return false;
    if (out->gameAllocOff < 0 || static_cast<size_t>(out->gameAllocOff) >= textSize) return false;
    if (out->rehash24Off < 0 || static_cast<size_t>(out->rehash24Off + 0xE2) >= textSize) return false;

    // Inside Rehash24: +0xDC = call Free.
    if (textBase[out->rehash24Off + 0xDC] != 0xE8) return false;
    out->gameFreeOff = out->rehash24Off + 0xDC + 5 + ReadRel32(textBase + out->rehash24Off + 0xDD);
    if (out->gameFreeOff < 0 || static_cast<size_t>(out->gameFreeOff) >= textSize) return false;

    // Verify all 4 64-bit NetGameSyncCheck Rehash48 call sites resolve to the exact same Rehash48 helper,
    // and CopySyncMap_Rehash's preceding RehashPolicy call resolves to out->rehashPolicyOff.
    const size_t r48_0 = out->offsets[kSendSyncRehash] + 25;
    const size_t r48_1 = out->offsets[kDeserSyncRehash] + 17;
    const size_t r48_2 = out->offsets[kCopySyncMapRehash];
    const size_t r48_3 = out->offsets[kCopySyncMapInsertRehash] + 18;
    if (r48_0 + 5 > textSize || r48_1 + 5 > textSize || r48_2 + 5 > textSize || r48_3 + 5 > textSize ||
        r48_2 < 19 ||
        textBase[r48_0] != 0xE8 || textBase[r48_1] != 0xE8 ||
        textBase[r48_2] != 0xE8 || textBase[r48_3] != 0xE8 ||
        textBase[r48_2 - 19] != 0xE8) {
        return false;
    }
    const intptr_t rh0 = static_cast<intptr_t>(r48_0) + 5 + ReadRel32(textBase + r48_0 + 1);
    const intptr_t rh1 = static_cast<intptr_t>(r48_1) + 5 + ReadRel32(textBase + r48_1 + 1);
    const intptr_t rh2 = static_cast<intptr_t>(r48_2) + 5 + ReadRel32(textBase + r48_2 + 1);
    const intptr_t rh3 = static_cast<intptr_t>(r48_3) + 5 + ReadRel32(textBase + r48_3 + 1);
    const intptr_t cpPol = static_cast<intptr_t>(r48_2 - 19) + 5 + ReadRel32(textBase + r48_2 - 18);
    if (rh0 != rh1 || rh0 != rh2 || rh0 != rh3 || cpPol != out->rehashPolicyOff ||
        rh0 < 0 || static_cast<size_t>(rh0) >= textSize) {
        return false;
    }
    out->rehash48Off = rh0;

    // Verify OnLoadComplete_Barrier has `cmp BYTE PTR [r15+0x8], 0; jne rel32` with valid target.
    const size_t bOff = out->offsets[kOnLoadCompleteBarrier];
    if (bOff + 11 > textSize || textBase[bOff + 5] != 0x0F || textBase[bOff + 6] != 0x85) {
        return false;
    }
    const intptr_t skipOff = static_cast<intptr_t>(bOff) + 11 + ReadRel32(textBase + bOff + 7);
    if (skipOff < 0 || static_cast<size_t>(skipOff) >= textSize) return false;

    return true;
}

bool ScanGameCoreTextSection(const uint8_t* textBase, size_t textSize, GameCoreTargets* out) {
    *out = GameCoreTargets{};
    out->textBase = textBase;
    out->textSize = textSize;

    std::vector<uint8_t> patBytes;
    std::vector<uint8_t> patMask;

    for (int p = 0; p < kGameCorePatternCount; ++p) {
        ParsePattern(kGameCorePatterns[p].hexPattern, patBytes, patMask);
        const size_t plen = patBytes.size();
        if (plen == 0 || plen > textSize) continue;

        size_t anchor = 0;
        while (anchor < plen && patMask[anchor] == 0) ++anchor;
        const uint8_t anchorByte = patBytes[anchor];

        int count = 0;
        size_t firstHit = 0;
        const size_t limit = textSize - plen;
        for (size_t i = 0; i <= limit; ++i) {
            if (textBase[i + anchor] != anchorByte) continue;
            bool match = true;
            for (size_t j = 0; j < plen; ++j) {
                if (patMask[j] && textBase[i + j] != patBytes[j]) {
                    match = false;
                    break;
                }
            }
            if (match) {
                if (count == 0) firstHit = i + kGameCorePatterns[p].targetOffset;
                ++count;
            }
        }
        out->matchCounts[p] = count;
        if (count == 1) {
            out->found[p] = true;
            out->offsets[p] = firstHit;
            out->matchedCount++;
        }
    }

    if (out->matchedCount != kGameCorePatternCount) {
        return false;
    }

    // Verify GW_PreAlloc: `0F AF CB  48 3B C1  73 0F`
    {
        const size_t off = out->offsets[kGcGwPreAlloc];
        const uint8_t exp[8] = {0x0F, 0xAF, 0xCB, 0x48, 0x3B, 0xC1, 0x73, 0x0F};
        if (off + sizeof(exp) > textSize || std::memcmp(textBase + off, exp, sizeof(exp)) != 0) {
            return false;
        }
    }

    // Verify GW_ComboLoop: `44 8B 74 24 60  45 85 F6  0F 84 <rel32>`
    {
        const size_t off = out->offsets[kGcGwComboLoop];
        const uint8_t exp[10] = {0x44, 0x8B, 0x74, 0x24, 0x60, 0x45, 0x85, 0xF6, 0x0F, 0x84};
        if (off + 14 > textSize || std::memcmp(textBase + off, exp, sizeof(exp)) != 0) {
            return false;
        }
        const intptr_t skipOff = static_cast<intptr_t>(off) + 14 + ReadRel32(textBase + off + 10);
        if (skipOff < 0 || static_cast<size_t>(skipOff) >= textSize) return false;
        out->gwComboSkipOff = skipOff;
    }

    // Verify NW_PlayerLoop and resolve next-player reload instruction (`mov rcx, [rsp+0x58]`)
    {
        const size_t off = out->offsets[kGcNwPlayerLoop];
        const uint8_t exp[14] = {0x48, 0x8B, 0x98, 0xD0, 0x06, 0x00, 0x00,
                                 0x48, 0x8B, 0x9B, 0xD0, 0x00, 0x00, 0x00};
        if (off + 23 > textSize || std::memcmp(textBase + off, exp, sizeof(exp)) != 0) {
            return false;
        }
        if (textBase[off + 17] != 0x0F || textBase[off + 18] != 0x84) return false;
        const intptr_t jeTarget = static_cast<intptr_t>(off) + 23 + ReadRel32(textBase + off + 19);
        const intptr_t nextPlrOff = jeTarget - 5;
        const uint8_t expMovRcx[5] = {0x48, 0x8B, 0x4C, 0x24, 0x58};
        if (nextPlrOff < 0 || static_cast<size_t>(nextPlrOff + 5) > textSize ||
            std::memcmp(textBase + nextPlrOff, expMovRcx, sizeof(expMovRcx)) != 0) {
            return false;
        }
        out->nwNextPlayerOff = nextPlrOff;
    }

    // Verify NW_GetBuildingsTbl (`E8 <rel32>`)
    {
        const size_t off = out->offsets[kGcNwGetBuildingsTable];
        if (off + 5 > textSize || textBase[off] != 0xE8) return false;
        const intptr_t fnOff = static_cast<intptr_t>(off) + 5 + ReadRel32(textBase + off + 1);
        if (fnOff < 0 || static_cast<size_t>(fnOff) >= textSize) return false;
        out->getBuildingsTableOff = fnOff;
    }

    // Verify Rel_UninitBool (`44 89 74 24 64`)
    {
        const size_t off = out->offsets[kGcRelUninitBool];
        const uint8_t exp[5] = {0x44, 0x89, 0x74, 0x24, 0x64};
        if (off + sizeof(exp) > textSize || std::memcmp(textBase + off, exp, sizeof(exp)) != 0) {
            return false;
        }
    }

    return true;
}

bool InstallGameCoreFixesIfNeeded() {
    AcquireSRWLockExclusive(&g_gameCoreLock);

    HMODULE gcMod = GetModuleHandleW(L"GameCore_XP2_FinalRelease.dll");
    if (!gcMod) {
        ReleaseSRWLockExclusive(&g_gameCoreLock);
        return false;
    }

    // Fast path: same HMODULE and our in-place sentinel byte (0x4C at Rel_UninitBool) is still patched.
    // If Civ6 unloaded and reloaded a fresh GameCore_XP2_FinalRelease.dll even at the exact same base,
    // the sentinel byte reverts to 0x44 and we cleanly re-apply all 3 fixes!
    if (gcMod == g_patchedGameCoreMod && g_patchedGameCoreSentinel && *g_patchedGameCoreSentinel == 0x4C) {
        ReleaseSRWLockExclusive(&g_gameCoreLock);
        return true;
    }

    uint8_t* textBase = nullptr;
    size_t textSize = 0;
    if (!GetExeTextSection(gcMod, &textBase, &textSize)) {
        if (g_failedGameCoreMod != gcMod) {
            g_failedGameCoreMod = gcMod;
            LogInfo("WARNING: Could not locate .text section of GameCore_XP2_FinalRelease.dll (%p).", gcMod);
        }
        ReleaseSRWLockExclusive(&g_gameCoreLock);
        return false;
    }

    GameCoreTargets gcr{};
    if (!ScanGameCoreTextSection(textBase, textSize, &gcr)) {
        if (g_failedGameCoreMod != gcMod) {
            g_failedGameCoreMod = gcMod;
            LogInfo("WARNING: GameCore_XP2_FinalRelease.dll (%p) signatures did not match (%d/%d). "
                    "Another core-fix mod (such as Civ6CoreFix/3823991616) may already be loaded; skipping GameCore fixes safely.",
                    gcMod, gcr.matchedCount, kGameCorePatternCount);
            for (int i = 0; i < kGameCorePatternCount; ++i) {
                if (!gcr.found[i]) {
                    LogInfo("  GC MISSING/AMBIGUOUS: %s (matches=%d)", GameCorePatternName(i), gcr.matchCounts[i]);
                }
            }
        }
        ReleaseSRWLockExclusive(&g_gameCoreLock);
        return false;
    }

    uint8_t* stubPage = AllocNearPage(reinterpret_cast<uintptr_t>(textBase + textSize / 2));
    if (!stubPage) {
        if (g_failedGameCoreMod != gcMod) {
            g_failedGameCoreMod = gcMod;
            LogInfo("ERROR: Failed to allocate near trampoline page for GameCore_XP2_FinalRelease.dll (%p).", gcMod);
        }
        ReleaseSRWLockExclusive(&g_gameCoreLock);
        return false;
    }

    g_fnGetBuildingsTbl = reinterpret_cast<FnGetBuildingsTable>(textBase + gcr.getBuildingsTableOff);

    StubBuilder sb{stubPage, 0, 0x1000};

    // 1A) GreatWorkCollectionAnalysis::Execute pre-alloc cap (6 stolen bytes: `imul ecx, ebx; cmp rax, rcx`)
    sb.align16();
    const uint8_t* stubGwPreAlloc = sb.cur();
    {
        const uint8_t code[] = {
            0x83, 0xFB, 0x20,       // cmp ebx, 32
            0x77, 0x03,             // ja  +3 (skip imul; reserve N instead of N*N)
            0x0F, 0xAF, 0xCB,       // imul ecx, ebx
            0x48, 0x3B, 0xC1        // cmp rax, rcx
        };
        sb.emit(code);
        sb.emitRel32Branch(0xE9, textBase + gcr.offsets[kGcGwPreAlloc] + 6);
    }

    // 1B) GreatWorkCollectionAnalysis::Execute 2-work/3-work combo loop guard (14 stolen bytes)
    sb.align16();
    const uint8_t* stubGwComboLoop = sb.cur();
    {
        const uint8_t* skipTarget = textBase + gcr.gwComboSkipOff;
        const uint8_t loadTest[] = {
            0x44, 0x8B, 0x74, 0x24, 0x60,  // mov r14d, DWORD PTR [rsp+0x60]
            0x45, 0x85, 0xF6               // test r14d, r14d
        };
        const uint8_t cmpCap[] = {
            0x41, 0x83, 0xFE, 0x20         // cmp r14d, 32
        };
        sb.emit(loadTest);
        sb.emitRel32Jcc(0x84, skipTarget);  // je skipTarget (N == 0)
        sb.emit(cmpCap);
        sb.emitRel32Jcc(0x87, skipTarget);  // ja skipTarget (N > 32 -> skip O(N^2)/O(N^3) combos)
        sb.emitRel32Branch(0xE9, textBase + gcr.offsets[kGcGwComboLoop] + 14);
    }

    // 2) OnWonderCompleted National Wonder protection (14 stolen bytes; rsp%16==0 at site)
    sb.align16();
    const uint8_t* stubNwPlayerLoop = sb.cur();
    {
        const uint8_t* site = textBase + gcr.offsets[kGcNwPlayerLoop];
        const uint8_t* nextPlayerTarget = textBase + gcr.nwNextPlayerOff;
        const uint8_t saveAndArgs[] = {
            0x50, 0x51, 0x52, 0x41, 0x50, 0x41, 0x51, 0x41, 0x52, 0x41, 0x53,  // push rax,rcx,rdx,r8..r11 (56B)
            0x48, 0x83, 0xEC, 0x28,                                            // sub rsp, 0x28 (40B -> rsp%16==0)
            0x89, 0xF1,                                                        // mov ecx, esi (buildingIndex)
            0x48, 0x8B, 0x95, 0x50, 0x01, 0x00, 0x00,                          // mov rdx, QWORD PTR [rbp+0x150] (completedCity)
            0x49, 0x89, 0xC0,                                                  // mov r8, rax (pPlayer)
            0x48, 0xB8                                                         // mov rax, &ShouldSkipWonderCancelForPlayer
        };
        const uint8_t restAndTest[] = {
            0xFF, 0xD0,                                                        // call rax -> al = shouldSkip
            0x48, 0x83, 0xC4, 0x28,                                            // add rsp, 0x28
            0x41, 0x5B, 0x41, 0x5A, 0x41, 0x59, 0x41, 0x58, 0x5A, 0x59,        // pop r11..r8,rdx,rcx
            0x84, 0xC0,                                                        // test al, al
            0x58                                                               // pop rax (preserves ZF!)
        };
        sb.emit(saveAndArgs);
        sb.emitU64(reinterpret_cast<uint64_t>(&ShouldSkipWonderCancelForPlayer));
        sb.emit(restAndTest);
        sb.emitRel32Jcc(0x85, nextPlayerTarget);  // jne nextPlayerTarget (skip cancelling this civ!)
        sb.emitBytes(site, 14);                   // orig: mov rbx,[rax+0x6d0]; mov rbx,[rbx+0xd0]
        sb.emitRel32Branch(0xE9, site + 14);
    }

    DWORD oldStubProt = 0;
    VirtualProtect(stubPage, 0x1000, PAGE_EXECUTE_READ, &oldStubProt);
    FlushInstructionCache(GetCurrentProcess(), stubPage, 0x1000);

    bool ok = true;
    ok &= PatchJmpRel32(textBase + gcr.offsets[kGcGwPreAlloc], 6, stubGwPreAlloc);
    ok &= PatchJmpRel32(textBase + gcr.offsets[kGcGwComboLoop], 14, stubGwComboLoop);
    ok &= PatchJmpRel32(textBase + gcr.offsets[kGcNwPlayerLoop], 14, stubNwPlayerLoop);

    // 3) AI::Religion uninitialized stack bool fix: `44 89 74 24 64` -> `4C 89 74 24 60`
    //    (`mov DWORD PTR [rsp+0x64], r14d` -> `mov QWORD PTR [rsp+0x60], r14`, zeroing [rsp+0x60..0x67])
    {
        const uint8_t relFix[5] = {0x4C, 0x89, 0x74, 0x24, 0x60};
        ok &= WriteCodeBytes(textBase + gcr.offsets[kGcRelUninitBool], relFix, sizeof(relFix));
    }

    if (ok) {
        g_patchedGameCoreMod = gcMod;
        g_patchedGameCoreSentinel = textBase + gcr.offsets[kGcRelUninitBool];
        g_failedGameCoreMod = nullptr;
        LogInfo("SUCCESS: Applied all 3 GameCore_XP2 fixes @ %p (trampolines=%zu bytes @ %p): "
                "AI GreatWork lag cap=32, National Wonder protection (GetBldg=+0x%llX), Religion AI uninit-bool desync fix.",
                gcMod, sb.pos, stubPage, static_cast<unsigned long long>(gcr.getBuildingsTableOff));
    } else {
        LogInfo("ERROR: One or more GameCore_XP2 patch writes failed @ %p!", gcMod);
    }

    ReleaseSRWLockExclusive(&g_gameCoreLock);
    return ok;
}

bool InstallSyncFixOnce() {
    // Process-wide environment guard prevents duplicate EXE patching if both C6SE and GameCore DLLs exist.
    if (g_installAttempted.exchange(true)) {
        InstallGameCoreFixesIfNeeded();
        return g_installSucceeded.load();
    }
    wchar_t envBuf[8]{};
    if (GetEnvironmentVariableW(L"CIV6_SYNCFIX_V1_ACTIVE", envBuf, 8) > 0) {
        LogInfo("Already active in this process (CIV6_SYNCFIX_V1_ACTIVE=1). Skipping EXE re-patch.");
        g_installSucceeded.store(true);
        InstallGameCoreFixesIfNeeded();
        return true;
    }

    LARGE_INTEGER freq{};
    QueryPerformanceFrequency(&freq);
    g_qpcFreq = freq.QuadPart;

    char exePath[MAX_PATH * 2]{};
    GetModuleFileNameA(nullptr, exePath, sizeof(exePath));
    HMODULE exeMod = GetModuleHandleW(nullptr);
    LogInfo("=======================================================");
    LogInfo("Civ6SyncFix v1.5 (64-bit Sync + Post-Reload Fix + GameCore Fixes + Desync Telemetry)");
    LogInfo("Process: %s | ExeBase: %p", exePath, exeMod);

    uint8_t* textBase = nullptr;
    size_t textSize = 0;
    if (!GetExeTextSection(exeMod, &textBase, &textSize)) {
        LogInfo("ERROR: Could not locate .text section of host executable. No bytes patched.");
        return false;
    }

    ResolvedTargets rt{};
    if (!ScanTextSection(textBase, textSize, &rt)) {
        LogInfo("ERROR: Signature verification failed (%d/%d matched). Refusing to patch!",
                rt.matchedCount, kPatternCount);
        for (int i = 0; i < kPatternCount; ++i) {
            if (!rt.found[i]) {
                LogInfo("  MISSING/AMBIGUOUS: %s (matches=%d)", PatternName(i), rt.matchCounts[i]);
            }
        }
        return false;
    }

    // Bind game's own memory allocator and rehash policy.
    g_fnRehashPolicy = reinterpret_cast<FnRehashPolicy>(textBase + rt.rehashPolicyOff);
    g_fnAlloc        = reinterpret_cast<FnGameAlloc>(textBase + rt.gameAllocOff);
    g_fnFree         = reinterpret_cast<FnGameFree>(textBase + rt.gameFreeOff);

    LogInfo("All %d/%d signatures uniquely verified!", rt.matchedCount, kPatternCount);
    LogInfo("Resolved helpers: Insert24=+0x%llX Policy=+0x%llX Alloc=+0x%llX Rehash48=+0x%llX Free=+0x%llX",
            static_cast<unsigned long long>(rt.insert24Off),
            static_cast<unsigned long long>(rt.rehashPolicyOff),
            static_cast<unsigned long long>(rt.gameAllocOff),
            static_cast<unsigned long long>(rt.rehash48Off),
            static_cast<unsigned long long>(rt.gameFreeOff));

    // Allocate one executable page within +/- 1.25 GB of .text midpoint so all patches stay in rel32 range.
    uint8_t* stubPage = AllocNearPage(reinterpret_cast<uintptr_t>(textBase + textSize / 2));
    if (!stubPage) {
        LogInfo("ERROR: Failed to allocate near trampoline page within +/-1.5 GB. No bytes patched.");
        return false;
    }

    StubBuilder sb{stubPage, 0, 0x1000};

    // --- Build near trampolines ---
    // 1) Near jump to InsertAutoArchive64
    sb.align16();
    const uint8_t* stubInsert24 = sb.cur();
    {
        const uint8_t movRax[] = {0x48, 0xB8};
        const uint8_t jmpRax[] = {0xFF, 0xE0};
        sb.emit(movRax);
        sb.emitU64(reinterpret_cast<uint64_t>(&InsertAutoArchive64));
        sb.emit(jmpRax);
    }

    // 1b) Near jump to Rehash48_64 (for CopySyncMap_Rehash call site)
    sb.align16();
    const uint8_t* stubRehash48 = sb.cur();
    {
        const uint8_t movRax[] = {0x48, 0xB8};
        const uint8_t jmpRax[] = {0xFF, 0xE0};
        sb.emit(movRax);
        sb.emitU64(reinterpret_cast<uint64_t>(&Rehash48_64));
        sb.emit(jmpRax);
    }

    // 2) ProcessRecv_Loop1 (14B: mov ecx,[rsi+20]; xor edx,edx; mov rax,[r14]; mov r8,[rsi+18]; div rcx)
    sb.align16();
    const uint8_t* stubProcLoop1 = sb.cur();
    {
        const uint8_t code[] = {
            0x8B, 0x4E, 0x20,       // mov ecx, DWORD PTR [rsi+0x20]
            0x33, 0xD2,             // xor edx, edx
            0x49, 0x8B, 0x06,       // mov rax, QWORD PTR [r14]
            0x4C, 0x8B, 0x46, 0x18, // mov r8, QWORD PTR [rsi+0x18]
            0x48, 0xF7, 0xF1        // div rcx
        };
        sb.emit(code);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kProcessRecvLoop1] + 14);
    }

    // 3) ProcessRecv_Loop2 (15B: mov r8d,[rbx+50]; xor edx,edx; mov rax,[rdi]; mov r9,[rbx+48]; div r8)
    sb.align16();
    const uint8_t* stubProcLoop2 = sb.cur();
    {
        const uint8_t code[] = {
            0x44, 0x8B, 0x43, 0x50, // mov r8d, DWORD PTR [rbx+0x50]
            0x33, 0xD2,             // xor edx, edx
            0x48, 0x8B, 0x07,       // mov rax, QWORD PTR [rdi]
            0x4C, 0x8B, 0x4B, 0x48, // mov r9, QWORD PTR [rbx+0x48]
            0x49, 0xF7, 0xF0        // div r8
        };
        sb.emit(code);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kProcessRecvLoop2] + 15);
    }

    // 4) SendSync_Lookup2 (18B: mov rax,[rsp+58]; xor edx,edx; mov r8d,[rdi+10]; mov rsi,rax; div r8)
    sb.align16();
    const uint8_t* stubSendLookup2 = sb.cur();
    {
        const uint8_t code[] = {
            0x48, 0x8B, 0x44, 0x24, 0x58, // mov rax, QWORD PTR [rsp+0x58]
            0x33, 0xD2,                   // xor edx, edx
            0x44, 0x8B, 0x47, 0x10,       // mov r8d, DWORD PTR [rdi+0x10]
            0x48, 0x89, 0xC6,             // mov rsi, rax
            0x49, 0xF7, 0xF0              // div r8
        };
        sb.emit(code);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kSendSyncLookup2] + 18);
    }

    // 5) SendSync_Rehash (30B -> 64-bit div + call Rehash48_64)
    sb.align16();
    const uint8_t* stubSendRehash = sb.cur();
    {
        const uint8_t pre[] = {
            0x48, 0x8B, 0x44, 0x24, 0x58,             // mov rax, QWORD PTR [rsp+0x58]
            0x8B, 0x8C, 0x24, 0xA4, 0x00, 0x00, 0x00, // mov ecx, DWORD PTR [rsp+0xa4]
            0x33, 0xD2,                               // xor edx, edx
            0x48, 0xF7, 0xF1,                         // div rcx
            0x8B, 0xEA,                               // mov ebp, edx
            0x48, 0x8B, 0xCF,                         // mov rcx, rdi
            0x8B, 0x94, 0x24, 0xA4, 0x00, 0x00, 0x00, // mov edx, DWORD PTR [rsp+0xa4]
            0x48, 0xB8                                // mov rax, imm64
        };
        const uint8_t callRax[] = {0xFF, 0xD0};
        sb.emit(pre);
        sb.emitU64(reinterpret_cast<uint64_t>(&Rehash48_64));
        sb.emit(callRax);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kSendSyncRehash] + 30);
    }

    // 6) DeserSync_Rehash (22B -> 64-bit div + call Rehash48_64)
    sb.align16();
    const uint8_t* stubDeserRehash = sb.cur();
    {
        const uint8_t pre[] = {
            0x8B, 0x4D, 0x7B,       // mov ecx, DWORD PTR [rbp+0x7b]
            0x33, 0xD2,             // xor edx, edx
            0x4C, 0x89, 0xE0,       // mov rax, r12
            0x48, 0xF7, 0xF1,       // div rcx
            0x44, 0x8B, 0xF2,       // mov r14d, edx
            0x48, 0x8B, 0xCE,       // mov rcx, rsi
            0x8B, 0x55, 0x7B,       // mov edx, DWORD PTR [rbp+0x7b]
            0x48, 0xB8              // mov rax, imm64
        };
        const uint8_t callRax[] = {0xFF, 0xD0};
        sb.emit(pre);
        sb.emitU64(reinterpret_cast<uint64_t>(&Rehash48_64));
        sb.emit(callRax);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kDeserSyncRehash] + 22);
    }

    // 6b) CopySyncMap_Insert_Rehash (23B -> 64-bit div + call Rehash48_64)
    sb.align16();
    const uint8_t* stubCopyInsertRehash = sb.cur();
    {
        const uint8_t pre[] = {
            0x8B, 0x4C, 0x24, 0x54,  // mov ecx, DWORD PTR [rsp+0x54]
            0x33, 0xD2,              // xor edx, edx
            0x4C, 0x89, 0xF0,        // mov rax, r14
            0x48, 0xF7, 0xF1,        // div rcx
            0x8B, 0xEA,              // mov ebp, edx
            0x48, 0x8B, 0xCF,        // mov rcx, rdi
            0x8B, 0x54, 0x24, 0x54,  // mov edx, DWORD PTR [rsp+0x54]
            0x48, 0xB8               // mov rax, imm64
        };
        const uint8_t callRax[] = {0xFF, 0xD0};
        sb.emit(pre);
        sb.emitU64(reinterpret_cast<uint64_t>(&Rehash48_64));
        sb.emit(callRax);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kCopySyncMapInsertRehash] + 23);
    }

    // 7) StartGameSyncCheck read-only timing/stats observer (11 stolen bytes)
    sb.align16();
    const uint8_t* stubStartSync = sb.cur();
    {
        const uint8_t save[] = {
            0x50, 0x51, 0x52, 0x41, 0x50, 0x41, 0x51, 0x41, 0x52, 0x41, 0x53,
            0x48, 0x83, 0xEC, 0x20,
            0x48, 0xB8
        };
        const uint8_t rest[] = {
            0xFF, 0xD0,
            0x48, 0x83, 0xC4, 0x20,
            0x41, 0x5B, 0x41, 0x5A, 0x41, 0x59, 0x41, 0x58, 0x5A, 0x59, 0x58
        };
        sb.emit(save);
        sb.emitU64(reinterpret_cast<uint64_t>(&OnStartGameSyncCheck));
        sb.emit(rest);
        sb.emitBytes(textBase + rt.offsets[kStartGameSyncCheck], 11);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kStartGameSyncCheck] + 11);
    }

    // 8) OnSyncComplete read-only timing/stats observer (10 stolen bytes)
    sb.align16();
    const uint8_t* stubSyncComplete = sb.cur();
    {
        const uint8_t save[] = {
            0x50, 0x51, 0x52, 0x41, 0x50, 0x41, 0x51, 0x41, 0x52, 0x41, 0x53,
            0x48, 0x83, 0xEC, 0x20,
            0x48, 0xB8
        };
        const uint8_t rest[] = {
            0xFF, 0xD0,
            0x48, 0x83, 0xC4, 0x20,
            0x41, 0x5B, 0x41, 0x5A, 0x41, 0x59, 0x41, 0x58, 0x5A, 0x59, 0x58
        };
        sb.emit(save);
        sb.emitU64(reinterpret_cast<uint64_t>(&OnSyncCompleteHook));
        sb.emit(rest);
        sb.emitBytes(textBase + rt.offsets[kOnSyncComplete], 10);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kOnSyncComplete] + 10);
    }

    // 9) OnSnapshotLoaded observer (6 stolen bytes: `push rbx; sub rsp, 0x60`; rsp%16==8 at entry)
    sb.align16();
    const uint8_t* stubOnSnapshotLoaded = sb.cur();
    {
        const uint8_t save[] = {
            0x50, 0x51, 0x52, 0x41, 0x50, 0x41, 0x51, 0x41, 0x52, 0x41, 0x53,
            0x48, 0x83, 0xEC, 0x20,
            0x48, 0xB8
        };
        const uint8_t rest[] = {
            0xFF, 0xD0,
            0x48, 0x83, 0xC4, 0x20,
            0x41, 0x5B, 0x41, 0x5A, 0x41, 0x59, 0x41, 0x58, 0x5A, 0x59, 0x58
        };
        sb.emit(save);
        sb.emitU64(reinterpret_cast<uint64_t>(&OnSnapshotLoadedHook));
        sb.emit(rest);
        sb.emitBytes(textBase + rt.offsets[kOnSnapshotLoaded], 6);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kOnSnapshotLoaded] + 6);
    }

    // 10) GameCoreController::OnLoadComplete stale TurnSynchronizationBarrier guard
    //     Replaces 11 bytes (`41 80 7F 08 00  0F 85 <rel32>`). At this site rsp%16==0.
    sb.align16();
    const uint8_t* stubOnLoadCompleteBarrier = sb.cur();
    {
        const uint8_t* site = textBase + rt.offsets[kOnLoadCompleteBarrier];
        const uint8_t* skipTarget = site + 11 + ReadRel32(site + 7);
        const uint8_t save[] = {
            0x51, 0x52, 0x41, 0x50, 0x41, 0x51, 0x41, 0x52, 0x41, 0x53,  // push rcx,rdx,r8..r11 (48B, aligned)
            0x48, 0x83, 0xEC, 0x20,                                      // sub rsp, 0x20 (32B shadow, rsp%16==0)
            0x48, 0xB8                                                   // mov rax, &ShouldSkipEndOfTurnBarrier
        };
        const uint8_t rest[] = {
            0xFF, 0xD0,                                                  // call rax -> al = shouldSkip
            0x48, 0x83, 0xC4, 0x20,                                      // add rsp, 0x20
            0x41, 0x5B, 0x41, 0x5A, 0x41, 0x59, 0x41, 0x58, 0x5A, 0x59,  // pop r11..r8,rdx,rcx
            0x84, 0xC0                                                   // test al, al
        };
        const uint8_t origCmp[] = {0x41, 0x80, 0x7F, 0x08, 0x00};        // cmp BYTE PTR [r15+0x8], 0x0
        sb.emit(save);
        sb.emitU64(reinterpret_cast<uint64_t>(&ShouldSkipEndOfTurnBarrier));
        sb.emit(rest);
        sb.emitRel32Jcc(0x85, skipTarget);                               // jne skipTarget (skip stale barrier!)
        sb.emit(origCmp);
        sb.emitRel32Jcc(0x85, skipTarget);                               // jne skipTarget (original branch)
        sb.emitRel32Branch(0xE9, site + 11);                             // jmp back to IsEndOfGameTurn()
    }

    // 11) HandleNetSyncSnapshot universal desynced AutoVariable logger
    //     Replaces 8 bytes (`mov rbx, [rbp-0x10]; cmp rbx, [rbp-0x08]`) at site+3 so all incoming
    //     branches (both non-empty fallthrough and empty-vars jmp) land cleanly on our jmp; rsp%16==0.
    //     The trailing `jmp rel32` back to `je` preserves EFLAGS set by the stolen `cmp`.
    sb.align16();
    const uint8_t* stubDeltaDesyncVars = sb.cur();
    {
        const uint8_t saveAndArgs[] = {
            0x50, 0x51, 0x52, 0x41, 0x50, 0x41, 0x51, 0x41, 0x52, 0x41, 0x53,  // push rax,rcx,rdx,r8..r11 (56B)
            0x48, 0x83, 0xEC, 0x28,                                            // sub rsp, 0x28 (40B -> rsp%16==0)
            0x48, 0x8B, 0x4F, 0x08,                                            // mov rcx, [rdi+0x08] (archive)
            0x48, 0x8B, 0x55, 0xF0,                                            // mov rdx, [rbp-0x10] (begin)
            0x4C, 0x8B, 0x45, 0xF8,                                            // mov r8,  [rbp-0x08] (end)
            0x48, 0xB8                                                         // mov rax, &OnDeltaDesyncVars
        };
        const uint8_t rest[] = {
            0xFF, 0xD0,                                                        // call rax
            0x48, 0x83, 0xC4, 0x28,                                            // add rsp, 0x28
            0x41, 0x5B, 0x41, 0x5A, 0x41, 0x59, 0x41, 0x58, 0x5A, 0x59, 0x58   // pop r11..r8,rdx,rcx,rax
        };
        sb.emit(saveAndArgs);
        sb.emitU64(reinterpret_cast<uint64_t>(&OnDeltaDesyncVars));
        sb.emit(rest);
        sb.emitBytes(textBase + rt.offsets[kDeltaDesyncVars], 8);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kDeltaDesyncVars] + 8);
    }

    // 12) RequestFullSnapshot read-only telemetry observer (10 stolen bytes; rsp%16==8 at entry)
    sb.align16();
    const uint8_t* stubRequestFullSnapshot = sb.cur();
    {
        const uint8_t save[] = {
            0x50, 0x51, 0x52, 0x41, 0x50, 0x41, 0x51, 0x41, 0x52, 0x41, 0x53,
            0x48, 0x83, 0xEC, 0x20,
            0x48, 0xB8
        };
        const uint8_t rest[] = {
            0xFF, 0xD0,
            0x48, 0x83, 0xC4, 0x20,
            0x41, 0x5B, 0x41, 0x5A, 0x41, 0x59, 0x41, 0x58, 0x5A, 0x59, 0x58
        };
        sb.emit(save);
        sb.emitU64(reinterpret_cast<uint64_t>(&OnRequestFullSnapshot));
        sb.emit(rest);
        sb.emitBytes(textBase + rt.offsets[kRequestFullSnapshot], 10);
        sb.emitRel32Branch(0xE9, textBase + rt.offsets[kRequestFullSnapshot] + 10);
    }

    // Seal trampoline page as RX (no longer writable).
    DWORD oldStubProt = 0;
    VirtualProtect(stubPage, 0x1000, PAGE_EXECUTE_READ, &oldStubProt);
    FlushInstructionCache(GetCurrentProcess(), stubPage, 0x1000);

    // --- Phase 2: Apply verified patches ---
    bool ok = true;

    // A) 12 in-place REX.W flips on m_AutoArchives lookups + 1 on SendSync_Lookup1
    const int adjacentPair6[] = {
        kAutoArchHelperLookup, kRegisterLookup1, kRegisterLookup2, kUnregisterLookup,
        kHandleSnap1Lookup1, kHandleSnap1Lookup2, kHandleSnap1Lookup3, kHandleSnap1Lookup4,
        kHandleSnap2Lookup1, kHandleSnap2Lookup2, kSendSyncLookup1
    };
    for (int idx : adjacentPair6) {
        uint8_t* p = textBase + rt.offsets[idx];
        ok &= WriteByteAt(p + 0, 0x49);  // 41 -> 49 (REX.WB: 64-bit mov)
        ok &= WriteByteAt(p + 3, 0x49);  // 41 -> 49 (REX.WB: 64-bit div)
    }

    // Register_Lookup3: +0 (41 8B C0) and +7 (41 F7 F1)
    {
        uint8_t* p = textBase + rt.offsets[kRegisterLookup3];
        ok &= WriteByteAt(p + 0, 0x49);
        ok &= WriteByteAt(p + 7, 0x49);
    }

    // HandleSnap2_Lookup3: +0 (41 8B 04 24) and +12 (41 F7 F0)
    {
        uint8_t* p = textBase + rt.offsets[kHandleSnap2Lookup3];
        ok &= WriteByteAt(p + 0, 0x49);
        ok &= WriteByteAt(p + 12, 0x49);
    }

    // DeserSync_Lookup: +0 (44 8B 65 EF -> 4C), +10 (41 8B C4 -> 49), +13 (41 F7 F0 -> 49)
    {
        uint8_t* p = textBase + rt.offsets[kDeserSyncLookup];
        ok &= WriteByteAt(p + 0, 0x4C);
        ok &= WriteByteAt(p + 10, 0x49);
        ok &= WriteByteAt(p + 13, 0x49);
    }

    // CopySyncMap_Insert_Lookup: +0 (45 8B 30 -> 4D), +12 (41 8B C6 -> 49), +18 (41 F7 F2 -> 49)
    {
        uint8_t* p = textBase + rt.offsets[kCopySyncMapInsertLookup];
        ok &= WriteByteAt(p + 0, 0x4D);
        ok &= WriteByteAt(p + 12, 0x49);
        ok &= WriteByteAt(p + 18, 0x49);
    }

    // B) Redirect the 3 m_AutoArchives Insert24 call sites to InsertAutoArchive64
    //    and the CopySyncMap_Rehash call site to Rehash48_64
    ok &= PatchCallRel32(textBase + rt.offsets[kAutoArchHelperCallIns], stubInsert24);
    ok &= PatchCallRel32(textBase + rt.offsets[kRegisterCallIns1], stubInsert24);
    ok &= PatchCallRel32(textBase + rt.offsets[kRegisterCallIns2], stubInsert24);
    ok &= PatchCallRel32(textBase + rt.offsets[kCopySyncMapRehash], stubRehash48);

    // C) Install 6 near-trampoline 64-bit lookup/rehash patches
    ok &= PatchJmpRel32(textBase + rt.offsets[kProcessRecvLoop1], 14, stubProcLoop1);
    ok &= PatchJmpRel32(textBase + rt.offsets[kProcessRecvLoop2], 15, stubProcLoop2);
    ok &= PatchJmpRel32(textBase + rt.offsets[kSendSyncLookup2], 18, stubSendLookup2);
    ok &= PatchJmpRel32(textBase + rt.offsets[kSendSyncRehash], 30, stubSendRehash);
    ok &= PatchJmpRel32(textBase + rt.offsets[kDeserSyncRehash], 22, stubDeserRehash);
    ok &= PatchJmpRel32(textBase + rt.offsets[kCopySyncMapInsertRehash], 23, stubCopyInsertRehash);

    // D) Install sync timing/stats observers + post-reload stale-barrier guard + desync telemetry hooks
    ok &= PatchJmpRel32(textBase + rt.offsets[kStartGameSyncCheck], 11, stubStartSync);
    ok &= PatchJmpRel32(textBase + rt.offsets[kOnSyncComplete], 10, stubSyncComplete);
    ok &= PatchJmpRel32(textBase + rt.offsets[kOnSnapshotLoaded], 6, stubOnSnapshotLoaded);
    ok &= PatchJmpRel32(textBase + rt.offsets[kOnLoadCompleteBarrier], 11, stubOnLoadCompleteBarrier);
    ok &= PatchJmpRel32(textBase + rt.offsets[kDeltaDesyncVars], 8, stubDeltaDesyncVars);
    ok &= PatchJmpRel32(textBase + rt.offsets[kRequestFullSnapshot], 10, stubRequestFullSnapshot);

    if (ok) {
        SetEnvironmentVariableW(L"CIV6_SYNCFIX_V1_ACTIVE", L"1");
        g_installSucceeded.store(true);
        LogInfo("SUCCESS: All %d EXE patches installed (trampolines=%zu bytes @ %p). Ready!",
                kPatternCount, sb.pos, stubPage);
        InstallGameCoreFixesIfNeeded();
    } else {
        LogInfo("ERROR: One or more VirtualProtect/patch writes failed!");
    }
    return ok;
}

}  // namespace civ6syncfix
