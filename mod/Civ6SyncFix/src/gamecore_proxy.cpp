#include "syncfix_core.h"

#include <windows.h>

#include <cstdio>
#include <cwchar>

namespace {

HMODULE g_vanillaGameCore = nullptr;

using FnDllCreateGameContext = void* (*)(void*, void*, void*, void*);
using FnDllDestroyGameContext = void (*)(void*, void*, void*, void*);

void PinSelfModule(void* addrInModule) {
    HMODULE self = nullptr;
    GetModuleHandleExW(
        GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_PIN,
        reinterpret_cast<LPCWSTR>(addrInModule),
        &self);
}

HMODULE EnsureVanillaGameCore() {
    if (g_vanillaGameCore) {
        return g_vanillaGameCore;
    }
    HMODULE h = GetModuleHandleW(L"GameCore_XP2_FinalRelease.dll");
    if (!h) {
        h = LoadLibraryW(L"GameCore_XP2_FinalRelease.dll");
    }
    if (!h) {
        // Fallback: resolve relative to CivilizationVI.exe -> ..\..\..\DLC\Expansion2\Binaries\Win64\GameCore_XP2_FinalRelease.dll
        wchar_t exePath[MAX_PATH * 2]{};
        if (GetModuleFileNameW(nullptr, exePath, MAX_PATH * 2) > 0) {
            wchar_t* slash = wcsrchr(exePath, L'\\');
            for (int up = 0; up < 3 && slash; ++up) {
                *slash = L'\0';
                slash = wcsrchr(exePath, L'\\');
            }
            if (slash) {
                *slash = L'\0';
                wchar_t full[MAX_PATH * 2]{};
                _snwprintf(full, MAX_PATH * 2 - 1,
                           L"%s\\DLC\\Expansion2\\Binaries\\Win64\\GameCore_XP2_FinalRelease.dll",
                           exePath);
                h = LoadLibraryW(full);
            }
        }
    }
    if (!h) {
        civ6syncfix::LogInfo("ERROR: Failed to load vanilla GameCore_XP2_FinalRelease.dll (err=%lu)",
                             GetLastError());
    }
    g_vanillaGameCore = h;
    return h;
}

}  // namespace

extern "C" {

__declspec(dllexport) void* DllCreateGameContext(void* a1, void* a2, void* a3, void* a4) {
    PinSelfModule(reinterpret_cast<void*>(&DllCreateGameContext));
    civ6syncfix::InstallSyncFixOnce();
    HMODULE h = EnsureVanillaGameCore();
    if (!h) return nullptr;
    civ6syncfix::InstallGameCoreFixesIfNeeded();
    auto fn = reinterpret_cast<FnDllCreateGameContext>(
        reinterpret_cast<void*>(GetProcAddress(h, "DllCreateGameContext")));
    if (!fn) {
        civ6syncfix::LogInfo("ERROR: DllCreateGameContext not found in vanilla GameCore_XP2_FinalRelease.dll");
        return nullptr;
    }
    return fn(a1, a2, a3, a4);
}

__declspec(dllexport) void DllDestroyGameContext(void* a1, void* a2, void* a3, void* a4) {
    HMODULE h = EnsureVanillaGameCore();
    if (!h) return;
    auto fn = reinterpret_cast<FnDllDestroyGameContext>(
        reinterpret_cast<void*>(GetProcAddress(h, "DllDestroyGameContext")));
    if (fn) {
        fn(a1, a2, a3, a4);
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
