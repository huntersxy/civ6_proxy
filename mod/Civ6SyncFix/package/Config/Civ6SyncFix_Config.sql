-- Civ6SyncFix: Standalone GameCore loader registration (LoadOrder=1, yields to C6SE Loader if present)
UPDATE GameCores
SET PackageId = '7f3c8e21-4a9b-4d1e-9c52-8e1a6b4f2091',
    DllPrefix = 'GameCore_XP2_Civ6SyncFix'
WHERE GameCore = 'Expansion2';
