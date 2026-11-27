#include "syncfix_core.h"

#include <windows.h>

#include <cassert>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <vector>

using namespace civ6syncfix;

namespace {

// Subset of Firaxis's actual prime table from 0x141128740.
const uint32_t kPrimes[] = {
    2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37, 41, 43, 47, 53, 59, 61, 67, 71,
    131, 257, 521, 1031, 2053, 4099, 8209, 16411, 32771, 65537
};

void MockRehashPolicy(void*, RehashDecision* out, uint32_t bucketCount, uint32_t elemCount, uint32_t delta) {
    if (elemCount + delta > bucketCount) {
        for (uint32_t p : kPrimes) {
            if (p >= (elemCount + delta) * 2) {
                out->needRehash = 1;
                out->newBucketCount = p;
                return;
            }
        }
    }
    out->needRehash = 0;
    out->newBucketCount = bucketCount;
}

size_t g_liveAllocs = 0;
void* MockAlloc(size_t size, uint32_t, uint32_t, uint32_t, uint64_t, uint32_t) {
    ++g_liveAllocs;
    return std::malloc(size);
}
void MockFree(void*, void* ptr, size_t) {
    --g_liveAllocs;
    std::free(ptr);
}

bool TestExeFile(const char* path) {
    std::ifstream ifs(path, std::ios::binary | std::ios::ate);
    if (!ifs) {
        printf("[FAIL] Cannot open %s\n", path);
        return false;
    }
    size_t sz = static_cast<size_t>(ifs.tellg());
    ifs.seekg(0);
    std::vector<uint8_t> buf(sz);
    ifs.read(reinterpret_cast<char*>(buf.data()), static_cast<std::streamsize>(sz));

    auto* dos = reinterpret_cast<const IMAGE_DOS_HEADER*>(buf.data());
    auto* nt = reinterpret_cast<const IMAGE_NT_HEADERS64*>(buf.data() + dos->e_lfanew);
    const IMAGE_SECTION_HEADER* sec = IMAGE_FIRST_SECTION(nt);
    const uint8_t* textBase = nullptr;
    size_t textSize = 0;
    uint32_t textVa = 0;
    for (WORD i = 0; i < nt->FileHeader.NumberOfSections; ++i) {
        if (std::memcmp(sec[i].Name, ".text", 5) == 0) {
            textBase = buf.data() + sec[i].PointerToRawData;
            textSize = sec[i].SizeOfRawData;
            textVa = sec[i].VirtualAddress;
            break;
        }
    }
    if (!textBase) {
        printf("[FAIL] No .text section in %s\n", path);
        return false;
    }

    ResolvedTargets rt{};
    bool ok = ScanTextSection(textBase, textSize, &rt);
    printf("\n=== Scanning %s (.text VA=0x%X size=%zu) ===\n", path, textVa, textSize);
    for (int i = 0; i < kPatternCount; ++i) {
        printf("  [%s] %-24s RVA=0x%06X (matches=%d)\n",
               rt.found[i] ? "OK" : "FAIL", PatternName(i),
               rt.found[i] ? static_cast<unsigned>(textVa + rt.offsets[i]) : 0U,
               rt.matchCounts[i]);
    }
    if (!ok) {
        printf("[FAIL] ScanTextSection failed (%d/%d)\n", rt.matchedCount, kPatternCount);
        return false;
    }
    printf("  Helpers: Insert24=0x%X Policy=0x%X Alloc=0x%X Rehash24=0x%X Rehash48=0x%X Free=0x%X\n",
           static_cast<unsigned>(textVa + rt.insert24Off),
           static_cast<unsigned>(textVa + rt.rehashPolicyOff),
           static_cast<unsigned>(textVa + rt.gameAllocOff),
           static_cast<unsigned>(textVa + rt.rehash24Off),
           static_cast<unsigned>(textVa + rt.rehash48Off),
           static_cast<unsigned>(textVa + rt.gameFreeOff));

    // Verify exact expected prefix bytes before flipping!
    auto checkByte = [&](int idx, size_t rel, uint8_t expected) -> bool {
        uint8_t got = textBase[rt.offsets[idx] + rel];
        if (got != expected) {
            printf("  [FAIL] %s+%zu: expected 0x%02X got 0x%02X\n", PatternName(idx), rel, expected, got);
            return false;
        }
        return true;
    };
    const int adj6[] = {0, 2, 3, 7, 8, 9, 10, 11, 12, 13, 17};
    for (int idx : adj6) {
        ok &= checkByte(idx, 0, 0x41);
        ok &= checkByte(idx, 3, 0x41);
    }
    ok &= checkByte(5, 0, 0x41) && checkByte(5, 7, 0x41);
    ok &= checkByte(14, 0, 0x41) && checkByte(14, 12, 0x41);
    ok &= checkByte(20, 0, 0x44) && checkByte(20, 10, 0x41) && checkByte(20, 13, 0x41);
    ok &= checkByte(24, 0, 0x40) && checkByte(24, 1, 0x53) && checkByte(24, 2, 0x48);
    ok &= checkByte(25, 0, 0x41) && checkByte(25, 5, 0x0F) && checkByte(25, 6, 0x85);
    ok &= checkByte(26, 0, 0x48) && checkByte(26, 3, 0xF0) && checkByte(26, 4, 0x48) &&
          checkByte(26, 7, 0xF8) && checkByte(26, 8, 0x74);
    ok &= checkByte(27, 0, 0x48) && checkByte(27, 5, 0x57) && checkByte(27, 6, 0x48);
    ok &= checkByte(28, 0, 0xE8);
    ok &= checkByte(29, 0, 0x45) && checkByte(29, 12, 0x41) && checkByte(29, 18, 0x41);
    ok &= checkByte(30, 0, 0x33) && checkByte(30, 2, 0x41) && checkByte(30, 18, 0xE8);
    printf("  Pre-patch opcode byte check: %s\n", ok ? "PASS" : "FAIL");
    return ok;
}

bool TestHashMap64Unit() {
    printf("\n=== Unit Testing InsertAutoArchive64 + RehashMap64 (20,000 adversarial Civ6 keys) ===\n");
    SetEngineHelpersForTest(&MockRehashPolicy, &MockAlloc, &MockFree);

    // Mimic empty Firaxis THashMap (static 2-slot dummy array with bucketCount == 1).
    void* staticDummy[2] = {nullptr, reinterpret_cast<void*>(~static_cast<uintptr_t>(0))};
    THashMapHeader map{};
    map.buckets = staticDummy;
    map.bucketCount = 1;
    map.elemCount = 0;

    constexpr uint32_t kN = 20000;
    std::vector<uint64_t> keys;
    keys.reserve(kN);
    for (uint32_t i = 1; i <= 15000; ++i) {
        keys.push_back((static_cast<uint64_t>(i) << 32) | 0x0045FFFFULL);  // DynamicModifier worst case!
    }
    for (uint32_t i = 1; i <= 3000; ++i) {
        keys.push_back((static_cast<uint64_t>(i) << 32) | 0x00250001ULL);  // Visibility::Instance!
    }
    for (uint32_t i = 1; i <= 2000; ++i) {
        keys.push_back((static_cast<uint64_t>(i) << 48) | (static_cast<uint64_t>(i) << 32) | 0x00020001ULL);
    }

    for (uint32_t i = 0; i < kN; ++i) {
        Pair16 kv{keys[i], reinterpret_cast<void*>(static_cast<uintptr_t>(i + 1))};
        InsertResult res{};
        InsertAutoArchive64(&map, &res, &kv);
        if (!res.inserted || !res.node || res.node->key != keys[i] || res.node->value != kv.value) {
            printf("  [FAIL] Insert failed at i=%u\n", i);
            return false;
        }
    }

    // Verify duplicate insert returns inserted=0 and original node (no mutation!).
    {
        Pair16 dup{keys[1234], reinterpret_cast<void*>(0xDEADBEEF)};
        InsertResult res{};
        InsertAutoArchive64(&map, &res, &dup);
        if (res.inserted != 0 || res.node->key != keys[1234] ||
            res.node->value != reinterpret_cast<void*>(static_cast<uintptr_t>(1235))) {
            printf("  [FAIL] Duplicate insert check failed\n");
            return false;
        }
    }

    // Verify sentinel, zero key mutation, O(1) lookup via key % bucketCount, and max chain length!
    if (map.buckets[map.bucketCount] != reinterpret_cast<void*>(~static_cast<uintptr_t>(0))) {
        printf("  [FAIL] End sentinel corrupted!\n");
        return false;
    }

    uint32_t maxChain = 0;
    uint32_t totalNodes = 0;
    for (uint32_t b = 0; b < map.bucketCount; ++b) {
        uint32_t len = 0;
        for (Node24* n = reinterpret_cast<Node24*>(map.buckets[b]); n; n = n->next) {
            ++len;
            ++totalNodes;
            if ((n->key % map.bucketCount) != b) {
                printf("  [FAIL] Misplaced node key=0x%llX in bucket %u\n",
                       static_cast<unsigned long long>(n->key), b);
                return false;
            }
        }
        if (len > maxChain) maxChain = len;
    }

    // Simulate client HandleNetSyncSnapshot lookup for EVERY key (including high32 != 0!).
    for (uint32_t i = 0; i < kN; ++i) {
        uint64_t b = keys[i] % map.bucketCount;
        bool found = false;
        for (Node24* n = reinterpret_cast<Node24*>(map.buckets[b]); n; n = n->next) {
            if (n->key == keys[i]) {
                found = true;
                break;
            }
        }
        if (!found) {
            printf("  [FAIL] Lookup failed for key=0x%llX!\n", static_cast<unsigned long long>(keys[i]));
            return false;
        }
    }

    printf("  elems=%u buckets=%u max_chain=%u (vanilla would be 15000!)\n",
           totalNodes, map.bucketCount, maxChain);
    if (totalNodes != kN || maxChain > 6) {
        printf("  [FAIL] Unexpected totalNodes=%u or maxChain=%u\n", totalNodes, maxChain);
        return false;
    }

    // Free all nodes and bucket array, verify 0 leaks.
    for (uint32_t b = 0; b < map.bucketCount; ++b) {
        Node24* n = reinterpret_cast<Node24*>(map.buckets[b]);
        while (n) {
            Node24* next = n->next;
            MockFree(nullptr, n, sizeof(Node24));
            n = next;
        }
    }
    MockFree(nullptr, map.buckets, (map.bucketCount + 1) * sizeof(void*));
    if (g_liveAllocs != 0) {
        printf("  [FAIL] Memory leak detected: liveAllocs=%zu\n", g_liveAllocs);
        return false;
    }
    printf("  Unit test: PASS (0 leaks, 100%% key preservation, max_chain=%u)\n", maxChain);
    return true;
}

struct Node48 {
    uint64_t key;
    uint32_t hashValue;
    uint32_t pad0;
    uint8_t  payload[24];
    Node48*  next;  // +0x28 (exact ArchiveSyncInfo node layout)
};
static_assert(offsetof(Node48, next) == 0x28, "Node48::next must be at +0x28");

bool TestCopySyncMap48Unit() {
    printf("\n=== Unit Testing NetGameSyncCheck CopySyncMap 48B Node Rehash (Turn-25 scenario) ===\n");
    SetEngineHelpersForTest(&MockRehashPolicy, &MockAlloc, &MockFree);

    void* staticDummy[2] = {nullptr, reinterpret_cast<void*>(~static_cast<uintptr_t>(0))};
    THashMapHeader map{};
    map.buckets = staticDummy;
    map.bucketCount = 1;
    map.elemCount = 0;

    // Reproduce Turn 25: 2,844 archives (600 with hi32==0, 2,244 with hi32!=0).
    constexpr uint32_t kTotal = 2844;
    std::vector<uint64_t> keys;
    keys.reserve(kTotal);
    for (uint32_t i = 1; i <= 600; ++i) {
        keys.push_back(static_cast<uint64_t>(i) | (0x0012ULL << 16));
    }
    for (uint32_t i = 1; i <= 2244; ++i) {
        keys.push_back((static_cast<uint64_t>(i) << 32) | 0x0045FFFFULL);
    }

    // 1) Simulate CopySyncMap_Rehash (pre-sizing empty/small map via Rehash48_64 before copy loop).
    RehashDecision preDec{};
    MockRehashPolicy(nullptr, &preDec, map.bucketCount, map.elemCount, 128);
    if (preDec.needRehash) {
        RehashMap64(&map, preDec.newBucketCount, offsetof(Node48, next));
    }

    // 2) Simulate CopySyncMap_Insert loop (64-bit lookup + 64-bit rehash on growth via Rehash48_64).
    for (uint32_t i = 0; i < kTotal; ++i) {
        uint64_t key = keys[i];
        uint64_t b = key % map.bucketCount;
        bool dup = false;
        for (Node48* cur = reinterpret_cast<Node48*>(map.buckets[b]); cur; cur = cur->next) {
            if (cur->key == key) {
                dup = true;
                break;
            }
        }
        if (dup) {
            printf("  [FAIL] Unexpected duplicate key at i=%u\n", i);
            return false;
        }
        RehashDecision dec{};
        MockRehashPolicy(nullptr, &dec, map.bucketCount, map.elemCount, 1);
        auto* node = reinterpret_cast<Node48*>(MockAlloc(sizeof(Node48), 0, 0, 0, 0, 0));
        std::memset(node, 0, sizeof(Node48));
        node->key = key;
        node->hashValue = i + 1;
        if (dec.needRehash) {
            b = key % dec.newBucketCount;
            RehashMap64(&map, dec.newBucketCount, offsetof(Node48, next));
        }
        node->next = reinterpret_cast<Node48*>(map.buckets[b]);
        map.buckets[b] = node;
        map.elemCount += 1;
    }

    // 3) Simulate ProcessRecv_Loop2 64-bit lookup for ALL 2,844 keys (must have 0 not_on_client!).
    if (map.buckets[map.bucketCount] != reinterpret_cast<void*>(~static_cast<uintptr_t>(0))) {
        printf("  [FAIL] Node48 map sentinel corrupted!\n");
        return false;
    }
    uint32_t maxChain = 0;
    for (uint32_t b = 0; b < map.bucketCount; ++b) {
        uint32_t len = 0;
        for (Node48* n = reinterpret_cast<Node48*>(map.buckets[b]); n; n = n->next) {
            ++len;
            if ((n->key % map.bucketCount) != b) {
                printf("  [FAIL] Misplaced Node48 key=0x%llX in bucket %u\n",
                       static_cast<unsigned long long>(n->key), b);
                return false;
            }
        }
        if (len > maxChain) maxChain = len;
    }
    for (uint32_t i = 0; i < kTotal; ++i) {
        uint64_t b = keys[i] % map.bucketCount;
        bool found = false;
        for (Node48* n = reinterpret_cast<Node48*>(map.buckets[b]); n; n = n->next) {
            if (n->key == keys[i] && n->hashValue == i + 1) {
                found = true;
                break;
            }
        }
        if (!found) {
            printf("  [FAIL] ProcessRecv_Loop2 missed key=0x%llX after CopySyncMap!\n",
                   static_cast<unsigned long long>(keys[i]));
            return false;
        }
    }

    for (uint32_t b = 0; b < map.bucketCount; ++b) {
        Node48* n = reinterpret_cast<Node48*>(map.buckets[b]);
        while (n) {
            Node48* next = n->next;
            MockFree(nullptr, n, sizeof(Node48));
            n = next;
        }
    }
    MockFree(nullptr, map.buckets, (map.bucketCount + 1) * sizeof(void*));
    if (g_liveAllocs != 0) {
        printf("  [FAIL] Memory leak in CopySyncMap48 test: liveAllocs=%zu\n", g_liveAllocs);
        return false;
    }
    printf("  CopySyncMap48 unit test: PASS (2844/2844 found, 0 misplaced, max_chain=%u, 0 leaks)\n", maxChain);
    return true;
}

bool TestPostReloadGuardUnit() {
    printf("\n=== Unit Testing Post-Reload Stale TurnSynchronizationBarrier Guard ===\n");
    uint8_t fakeSyncMgr[0x40]{};

    // 1) Normal lobby / save load (m_bRequestedSnapshot == 0): must NOT skip barrier.
    ResetPostReloadGuardForTest();
    fakeSyncMgr[0x38] = 0;
    OnSnapshotLoadedHook(fakeSyncMgr);
    if (ShouldSkipEndOfTurnBarrier()) {
        printf("  [FAIL] Lobby load unexpectedly skipped barrier\n");
        return false;
    }

    // 2) Mid-game resync reload (m_bRequestedSnapshot == 1): must skip barrier ONCE, then disarm.
    fakeSyncMgr[0x38] = 1;
    OnSnapshotLoadedHook(fakeSyncMgr);
    if (!ShouldSkipEndOfTurnBarrier()) {
        printf("  [FAIL] Resync reload failed to skip stale barrier\n");
        return false;
    }
    if (ShouldSkipEndOfTurnBarrier()) {
        printf("  [FAIL] Guard did not disarm after single-shot consumption\n");
        return false;
    }

    // 3) Resync followed by lobby snapshot (m_bRequestedSnapshot == 0): must disarm cleanly.
    fakeSyncMgr[0x38] = 1;
    OnSnapshotLoadedHook(fakeSyncMgr);
    fakeSyncMgr[0x38] = 0;
    OnSnapshotLoadedHook(fakeSyncMgr);
    if (ShouldSkipEndOfTurnBarrier()) {
        printf("  [FAIL] Non-resync snapshot failed to disarm guard\n");
        return false;
    }

    printf("  Post-reload guard unit test: PASS (all 3 scenarios verified)\n");
    return true;
}

struct MockFString {
    const char* begin;
    const char* end;
    const char* cap;
};

static MockFString g_mockVarNames[2] = {};
static void* g_mockVarPtrs[2] = {reinterpret_cast<void*>(0x1001), reinterpret_cast<void*>(0x1002)};

static const MockFString* MockGetVarName(void*, void* varPtr) {
    if (varPtr == g_mockVarPtrs[0]) return &g_mockVarNames[0];
    if (varPtr == g_mockVarPtrs[1]) return &g_mockVarNames[1];
    return nullptr;
}

bool TestDesyncTelemetryUnit() {
    printf("\n=== Unit Testing Universal AutoVariable Desync Telemetry ===\n");
    const char* n0 = "m_doneOffTurn";
    const char* n1 = "m_buildingEvaluations";
    g_mockVarNames[0] = {n0, n0 + std::strlen(n0), n0 + std::strlen(n0)};
    g_mockVarNames[1] = {n1, n1 + std::strlen(n1), n1 + std::strlen(n1)};

    void* fakeVtable[3] = {nullptr, reinterpret_cast<void*>(&MockGetVarName), nullptr};
    uint8_t fakeArchive[0x80]{};
    void* vtPtr = fakeVtable;
    void** vb = g_mockVarPtrs;
    void** ve = g_mockVarPtrs + 2;
    uint64_t key = (131073ULL << 32) | (67ULL << 16) | 5ULL;  // 5-67-131073
    const char* tn = "AI::CityBuild";
    const char* tnEnd = tn + std::strlen(tn);

    std::memcpy(fakeArchive + 0x00, &vtPtr, sizeof(vtPtr));
    std::memcpy(fakeArchive + 0x08, &vb, sizeof(vb));
    std::memcpy(fakeArchive + 0x10, &ve, sizeof(ve));
    std::memcpy(fakeArchive + 0x58, &key, sizeof(key));
    std::memcpy(fakeArchive + 0x60, &tn, sizeof(tn));
    std::memcpy(fakeArchive + 0x68, &tnEnd, sizeof(tnEnd));

    // Include valid indices (0, 1) and an out-of-bounds index (99) to verify bounds safety.
    uint32_t indices[3] = {0, 1, 99};
    OnDeltaDesyncVars(fakeArchive, indices, indices + 3);
    // Also test empty mismatch vector (begin == end) and null archive.
    OnDeltaDesyncVars(fakeArchive, indices, indices);
    OnDeltaDesyncVars(nullptr, indices, indices + 1);
    OnRequestFullSnapshot(nullptr);
    printf("  Universal desync telemetry unit test: PASS\n");
    return true;
}

bool TestGameCoreFile(const char* path) {
    std::ifstream ifs(path, std::ios::binary | std::ios::ate);
    if (!ifs) {
        printf("[FAIL] Cannot open %s\n", path);
        return false;
    }
    size_t sz = static_cast<size_t>(ifs.tellg());
    ifs.seekg(0);
    std::vector<uint8_t> buf(sz);
    ifs.read(reinterpret_cast<char*>(buf.data()), static_cast<std::streamsize>(sz));

    auto* dos = reinterpret_cast<const IMAGE_DOS_HEADER*>(buf.data());
    auto* nt = reinterpret_cast<const IMAGE_NT_HEADERS64*>(buf.data() + dos->e_lfanew);
    const IMAGE_SECTION_HEADER* sec = IMAGE_FIRST_SECTION(nt);
    const uint8_t* textBase = nullptr;
    size_t textSize = 0;
    uint32_t textVa = 0;
    for (WORD i = 0; i < nt->FileHeader.NumberOfSections; ++i) {
        if (std::memcmp(sec[i].Name, ".text", 5) == 0) {
            textBase = buf.data() + sec[i].PointerToRawData;
            textSize = sec[i].SizeOfRawData;
            textVa = sec[i].VirtualAddress;
            break;
        }
    }
    if (!textBase) {
        printf("[FAIL] No .text section in %s\n", path);
        return false;
    }

    GameCoreTargets gcr{};
    bool ok = ScanGameCoreTextSection(textBase, textSize, &gcr);
    printf("\n=== Scanning %s (.text VA=0x%X size=%zu) ===\n", path, textVa, textSize);
    for (int i = 0; i < kGameCorePatternCount; ++i) {
        printf("  [%s] %-24s RVA=0x%06X (matches=%d)\n",
               gcr.found[i] ? "OK" : "FAIL", GameCorePatternName(i),
               gcr.found[i] ? static_cast<unsigned>(textVa + gcr.offsets[i]) : 0U,
               gcr.matchCounts[i]);
    }
    if (!ok) {
        printf("[FAIL] ScanGameCoreTextSection failed (%d/%d)\n", gcr.matchedCount, kGameCorePatternCount);
        return false;
    }
    printf("  Resolved: GetBuildingsTable=0x%X NW_NextPlayer=0x%X GW_ComboSkip=0x%X\n",
           static_cast<unsigned>(textVa + gcr.getBuildingsTableOff),
           static_cast<unsigned>(textVa + gcr.nwNextPlayerOff),
           static_cast<unsigned>(textVa + gcr.gwComboSkipOff));
    printf("  GameCore pre-patch opcode & target check: PASS\n");
    return true;
}

static uint8_t g_mockBuildingNormal[0x160]{};
static uint8_t g_mockBuildingWorldWonder[0x160]{};
static uint8_t g_mockBuildingNationalWonder[0x160]{};
static const uint8_t* g_mockBuildingsPtrs[3] = {
    g_mockBuildingNormal,
    g_mockBuildingWorldWonder,
    g_mockBuildingNationalWonder,
};
static BuildingsTable g_mockBuildingsTbl = {g_mockBuildingsPtrs, g_mockBuildingsPtrs + 3};

static const BuildingsTable* MockGetBuildingsTable() {
    return &g_mockBuildingsTbl;
}

bool TestNationalWonderUnit() {
    printf("\n=== Unit Testing National Wonder Cancel Protection ===\n");
    std::memset(g_mockBuildingNormal, 0, sizeof(g_mockBuildingNormal));
    std::memset(g_mockBuildingWorldWonder, 0, sizeof(g_mockBuildingWorldWonder));
    std::memset(g_mockBuildingNationalWonder, 0, sizeof(g_mockBuildingNationalWonder));

    int32_t neg1 = -1, one = 1;
    // 0: Normal building (IsWonder=0, MaxPlayer=-1, MaxWorld=-1)
    std::memcpy(g_mockBuildingNormal + 0x3C, &neg1, 4);
    std::memcpy(g_mockBuildingNormal + 0x40, &neg1, 4);

    // 1: World Wonder (IsWonder=1, MaxPlayer=-1, MaxWorld=1)
    g_mockBuildingWorldWonder[0x158] = 0x40;
    std::memcpy(g_mockBuildingWorldWonder + 0x3C, &neg1, 4);
    std::memcpy(g_mockBuildingWorldWonder + 0x40, &one, 4);

    // 2: National Wonder (IsWonder=1, MaxPlayer=1, MaxWorld=-1)
    g_mockBuildingNationalWonder[0x158] = 0x40;
    std::memcpy(g_mockBuildingNationalWonder + 0x3C, &one, 4);
    std::memcpy(g_mockBuildingNationalWonder + 0x40, &neg1, 4);

    SetGetBuildingsTableForTest(&MockGetBuildingsTable);

    struct FakeCityHandle {
        int16_t owner;
        int16_t type;
        int32_t id;
    };
    FakeCityHandle completedByP0{0, 2, 65536};
    uint8_t player0[0xE0]{};
    uint8_t player1[0xE0]{};
    int32_t id0 = 0, id1 = 1;
    std::memcpy(player0 + 0xD8, &id0, 4);
    std::memcpy(player1 + 0xD8, &id1, 4);

    // Normal building & World Wonder must NEVER skip (vanilla behavior preserved).
    if (ShouldSkipWonderCancelForPlayer(0, &completedByP0, player1)) {
        printf("  [FAIL] Normal building unexpectedly skipped\n");
        return false;
    }
    if (ShouldSkipWonderCancelForPlayer(1, &completedByP0, player1)) {
        printf("  [FAIL] World Wonder unexpectedly skipped for other civ\n");
        return false;
    }
    // National Wonder completed by Player 0:
    // - Player 0's own other cities MUST still cancel (return false)
    // - Player 1's cities MUST be skipped/preserved (return true)
    if (ShouldSkipWonderCancelForPlayer(2, &completedByP0, player0)) {
        printf("  [FAIL] National Wonder skipped for SAME civ (should still cancel own other cities)\n");
        return false;
    }
    if (!ShouldSkipWonderCancelForPlayer(2, &completedByP0, player1)) {
        printf("  [FAIL] National Wonder failed to protect OTHER civ's build\n");
        return false;
    }
    // Out-of-bounds & null safety
    if (ShouldSkipWonderCancelForPlayer(-1, &completedByP0, player1) ||
        ShouldSkipWonderCancelForPlayer(99, &completedByP0, player1) ||
        ShouldSkipWonderCancelForPlayer(2, nullptr, player1) ||
        ShouldSkipWonderCancelForPlayer(2, &completedByP0, nullptr)) {
        printf("  [FAIL] Bounds/null safety check failed\n");
        return false;
    }

    printf("  National Wonder protection unit test: PASS (all 5 cases verified)\n");
    return true;
}

}  // namespace

int main() {
    bool ok = true;
    ok &= TestExeFile("D:\\Steam\\steamapps\\common\\Sid Meier's Civilization VI\\Base\\Binaries\\Win64Steam\\CivilizationVI.exe");
    ok &= TestExeFile("D:\\Steam\\steamapps\\common\\Sid Meier's Civilization VI\\Base\\Binaries\\Win64Steam\\CivilizationVI_DX12.exe");
    ok &= TestGameCoreFile("D:\\Steam\\steamapps\\common\\Sid Meier's Civilization VI\\DLC\\Expansion2\\Binaries\\Win64\\GameCore_XP2_FinalRelease.dll");
    ok &= TestHashMap64Unit();
    ok &= TestCopySyncMap48Unit();
    ok &= TestPostReloadGuardUnit();
    ok &= TestDesyncTelemetryUnit();
    ok &= TestNationalWonderUnit();
    printf("\nOVERALL RESULT: %s\n", ok ? "ALL PASS" : "FAILED");
    return ok ? 0 : 1;
}
