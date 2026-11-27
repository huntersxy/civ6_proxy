#include "syncfix_core.h"

#include <windows.h>

#include <cstdlib>

namespace {

struct C6SEPluginObj;

using FnPluginVoid = void (*)(C6SEPluginObj*);
using FnPluginDtor = void (*)(C6SEPluginObj*, uint32_t);
using FnPluginStr  = const char* (*)(C6SEPluginObj*);

// Exact 9-slot MSVC vtable layout expected by C6SE (GameCore_XP2_Loader_FinalRelease.dll).
// Using an explicit C vtable struct avoids MSVC vs MinGW Itanium C++ ABI mismatch.
struct C6SEPluginVTable {
    FnPluginVoid slot0_noop;
    FnPluginDtor slot1_dtor;
    FnPluginVoid slot2_noop;
    FnPluginVoid slot3_noop;
    FnPluginVoid slot4_noop;
    FnPluginStr  slot5_getName;
    FnPluginStr  slot6_getVersion;
    FnPluginVoid slot7_noop;
    FnPluginVoid slot8_noop;
};

struct C6SEPluginObj {
    const C6SEPluginVTable* vtbl;
};

void PluginNoop(C6SEPluginObj*) {}

void PluginDtor(C6SEPluginObj* self, uint32_t flags) {
    if (self && (flags & 1u)) {
        std::free(self);
    }
}

const char* PluginGetName(C6SEPluginObj*) {
    return "Civ6SyncFix";
}

const char* PluginGetVersion(C6SEPluginObj*) {
    return "1.5";
}

const C6SEPluginVTable kPluginVTable = {
    &PluginNoop,
    &PluginDtor,
    &PluginNoop,
    &PluginNoop,
    &PluginNoop,
    &PluginGetName,
    &PluginGetVersion,
    &PluginNoop,
    &PluginNoop,
};

void PinSelfModule(void* addrInModule) {
    HMODULE self = nullptr;
    GetModuleHandleExW(
        GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_PIN,
        reinterpret_cast<LPCWSTR>(addrInModule),
        &self);
}

}  // namespace

extern "C" {

struct C6SEPluginInfoData {
    uint32_t apiVersion;
    uint32_t reserved[3];
};

__declspec(dllexport) extern const C6SEPluginInfoData C6SEPlugin_Info = {1, {0, 0, 0}};

__declspec(dllexport) void* GetPlugin(void* pC6SE) {
    PinSelfModule(reinterpret_cast<void*>(&GetPlugin));
    civ6syncfix::LogInfo("C6SE GetPlugin(pC6SE=%p) invoked.", pC6SE);
    civ6syncfix::InstallSyncFixOnce();
    civ6syncfix::InstallGameCoreFixesIfNeeded();
    auto* obj = static_cast<C6SEPluginObj*>(std::malloc(sizeof(C6SEPluginObj)));
    if (obj) {
        obj->vtbl = &kPluginVTable;
    }
    return obj;
}

__declspec(dllexport) void DestroyPlugin(void* pPlugin) {
    civ6syncfix::LogInfo("C6SE DestroyPlugin(pPlugin=%p) invoked (patches remain pinned).", pPlugin);
    auto* obj = static_cast<C6SEPluginObj*>(pPlugin);
    if (obj && obj->vtbl && obj->vtbl->slot1_dtor) {
        obj->vtbl->slot1_dtor(obj, 1u);
    }
}

}  // extern "C"

BOOL WINAPI DllMain(HINSTANCE hinst, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        DisableThreadLibraryCalls(hinst);
        PinSelfModule(reinterpret_cast<void*>(&DllMain));
    }
    return TRUE;
}
