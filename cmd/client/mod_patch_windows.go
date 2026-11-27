//go:build windows

package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type modPatchFileSpec struct {
	RelPath         string
	ExpectedOrigMD5 string
	PatchedContent  string
}

type modPatchSpec struct {
	WorkshopID  string
	Name        string
	Description string
	Files       []modPatchFileSpec
}

type modPatchFileState struct {
	RelPath         string
	Exists          bool
	ActualMD5       string
	ExpectedOrigMD5 string
	PatchedMD5      string
	MatchesOriginal bool
	MatchesPatched  bool
}

type modPatchStateKind uint8

const (
	modPatchNotFound modPatchStateKind = iota
	modPatchApplied
	modPatchReady
	modPatchMismatch
)

type modPatchStatus struct {
	Spec           modPatchSpec
	WorkshopDir    string
	WorkshopFound  bool
	State          modPatchStateKind
	HasBackup      bool
	BackupTime     string
	Files          []modPatchFileState
	LastError      string
}

type modPatchBackupManifest struct {
	WorkshopID string            `json:"workshop_id"`
	ModName    string            `json:"mod_name"`
	CreatedAt  string            `json:"created_at"`
	OrigMD5    map[string]string `json:"orig_md5"`
}

func modPatchSpecs() []modPatchSpec {
	return []modPatchSpec{
		{
			WorkshopID:  "3475048047",
			Name:        "自动部署与警戒 (Auto Deploy and Alert)",
			Description: "过滤非本地玩家代发 FORTIFY/ALERT 指令，将升级标记改为同步属性并固定遍历顺序，解决单位位置与属性失步。",
			Files: []modPatchFileSpec{
				{
					RelPath:         "RUIVO_UI_AUTO_DEPLOYMENT.lua",
					ExpectedOrigMD5: "f53b230b53b48a139af8886a4905f019",
					PatchedContent:  patchedRuivoUIAutoDeploymentLua,
				},
				{
					RelPath:         "RUIVO_GP_AUTO_DEPLOYMENT.lua",
					ExpectedOrigMD5: "75e4e5f5fabae2f3a5a4eceaee5d9e55",
					PatchedContent:  patchedRuivoGPAutoDeploymentLua,
				},
			},
		},
		{
			WorkshopID:  "3536386676",
			Name:        "士气与围城 (Morale and Siege)",
			Description: "单位阵亡加减士气改用真实坐标（修复客户端重载后全局坐标归零 Bug），移除本地玩家分支并固定围城列表排序。",
			Files: []modPatchFileSpec{
				{
					RelPath:         "APMS_GP.lua",
					ExpectedOrigMD5: "24687c778a7c1d5a291b8bd7e9dd6e06",
					PatchedContent:  patchedAPMSGPLua,
				},
			},
		},
		{
			WorkshopID:  "3676624054",
			Name:        "存储生产力 (Store Production)",
			Description: "将界面刷新跨上下文直改点数改为仅由本地玩家通过 EXECUTE_SCRIPT 同步队列提交，消除时间差失步。",
			Files: []modPatchFileSpec{
				{
					RelPath:         "AWASE_MOD_0227_UI.lua",
					ExpectedOrigMD5: "4ae230ff1143374364c885041990b271",
					PatchedContent:  patchedAwaseUI0227Lua,
				},
				{
					RelPath:         "AWASE_MOD_0227_Gameplay.lua",
					ExpectedOrigMD5: "36f375178afec06b9ff762be8afb9c35",
					PatchedContent:  patchedAwaseGameplay0227Lua,
				},
			},
		},
	}
}

func modPatchBackupRoot() string {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" {
		if cfgDir, err := os.UserConfigDir(); err == nil {
			base = cfgDir
		}
	}
	return filepath.Join(base, "Civ6Relay", "modpatch_backups")
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func inspectModPatch(spec modPatchSpec, libraryRoots []string, backupRoot string) modPatchStatus {
	st := modPatchStatus{
		Spec:  spec,
		State: modPatchNotFound,
	}
	wsDir, ok := findWorkshopModDir(libraryRoots, civ6SteamAppID, spec.WorkshopID)
	if !ok {
		return st
	}
	st.WorkshopDir = wsDir
	st.WorkshopFound = true

	manifestPath := filepath.Join(backupRoot, spec.WorkshopID, "manifest.json")
	if raw, err := os.ReadFile(manifestPath); err == nil {
		var mf modPatchBackupManifest
		if json.Unmarshal(raw, &mf) == nil && len(mf.OrigMD5) > 0 {
			st.HasBackup = true
			st.BackupTime = mf.CreatedAt
		}
	}

	allExist := true
	allOrig := true
	allPatched := true
	st.Files = make([]modPatchFileState, 0, len(spec.Files))
	for _, f := range spec.Files {
		patchedMD5 := md5Hex([]byte(f.PatchedContent))
		fs := modPatchFileState{
			RelPath:         f.RelPath,
			ExpectedOrigMD5: strings.ToLower(f.ExpectedOrigMD5),
			PatchedMD5:      patchedMD5,
		}
		targetPath := filepath.Join(wsDir, filepath.FromSlash(f.RelPath))
		raw, err := os.ReadFile(targetPath)
		if err != nil {
			allExist = false
			allOrig = false
			allPatched = false
		} else {
			fs.Exists = true
			fs.ActualMD5 = md5Hex(raw)
			fs.MatchesOriginal = strings.EqualFold(fs.ActualMD5, fs.ExpectedOrigMD5)
			fs.MatchesPatched = strings.EqualFold(fs.ActualMD5, fs.PatchedMD5)
			if !fs.MatchesOriginal {
				allOrig = false
			}
			if !fs.MatchesPatched {
				allPatched = false
			}
		}
		st.Files = append(st.Files, fs)
	}

	switch {
	case !allExist:
		st.State = modPatchNotFound
	case allPatched:
		st.State = modPatchApplied
	case allOrig:
		st.State = modPatchReady
	default:
		st.State = modPatchMismatch
	}
	return st
}

func inspectAllModPatches(libraryRoots []string, backupRoot string) []modPatchStatus {
	specs := modPatchSpecs()
	out := make([]modPatchStatus, 0, len(specs))
	for _, spec := range specs {
		out = append(out, inspectModPatch(spec, libraryRoots, backupRoot))
	}
	return out
}

func applyModPatch(spec modPatchSpec, libraryRoots []string, backupRoot string, force bool) (modPatchStatus, error) {
	st := inspectModPatch(spec, libraryRoots, backupRoot)
	if !st.WorkshopFound || st.State == modPatchNotFound {
		return st, fmt.Errorf("未找到工坊 Mod %s 的目标文件", spec.WorkshopID)
	}
	if st.State == modPatchApplied {
		return st, nil
	}
	if !force && st.State != modPatchReady {
		return st, fmt.Errorf("文件 MD5 与预期原版不一致（可点击“强制打补丁”覆盖同 ID Mod）")
	}

	if err := ensureModPatchBackup(st, backupRoot); err != nil {
		return st, fmt.Errorf("备份原文件失败: %w", err)
	}

	for _, f := range spec.Files {
		targetPath := filepath.Join(st.WorkshopDir, filepath.FromSlash(f.RelPath))
		if err := os.WriteFile(targetPath, []byte(f.PatchedContent), 0o644); err != nil {
			return inspectModPatch(spec, libraryRoots, backupRoot), fmt.Errorf("写入 %s 失败（若文明 VI 正在运行请先退出游戏）: %w", f.RelPath, err)
		}
	}
	return inspectModPatch(spec, libraryRoots, backupRoot), nil
}

func ensureModPatchBackup(st modPatchStatus, backupRoot string) error {
	dir := filepath.Join(backupRoot, st.Spec.WorkshopID)
	manifestPath := filepath.Join(dir, "manifest.json")
	if raw, err := os.ReadFile(manifestPath); err == nil {
		var mf modPatchBackupManifest
		if json.Unmarshal(raw, &mf) == nil && len(mf.OrigMD5) == len(st.Spec.Files) {
			allBacked := true
			for _, f := range st.Spec.Files {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.RelPath))); err != nil {
					allBacked = false
					break
				}
			}
			if allBacked {
				return nil
			}
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mf := modPatchBackupManifest{
		WorkshopID: st.Spec.WorkshopID,
		ModName:    st.Spec.Name,
		CreatedAt:  time.Now().Format("2006-01-02 15:04:05"),
		OrigMD5:    make(map[string]string, len(st.Spec.Files)),
	}
	for _, f := range st.Spec.Files {
		src := filepath.Join(st.WorkshopDir, filepath.FromSlash(f.RelPath))
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(f.RelPath))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, raw, 0o644); err != nil {
			return err
		}
		mf.OrigMD5[f.RelPath] = md5Hex(raw)
	}
	metaBytes, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, metaBytes, 0o644)
}

func restoreModPatch(spec modPatchSpec, libraryRoots []string, backupRoot string) (modPatchStatus, error) {
	st := inspectModPatch(spec, libraryRoots, backupRoot)
	if !st.WorkshopFound {
		return st, fmt.Errorf("未找到工坊 Mod %s 目录", spec.WorkshopID)
	}
	if !st.HasBackup {
		return st, errors.New("未找到原始文件备份，无法自动还原")
	}
	dir := filepath.Join(backupRoot, spec.WorkshopID)
	for _, f := range spec.Files {
		backupPath := filepath.Join(dir, filepath.FromSlash(f.RelPath))
		raw, err := os.ReadFile(backupPath)
		if err != nil {
			return st, fmt.Errorf("读取备份文件 %s 失败: %w", f.RelPath, err)
		}
		targetPath := filepath.Join(st.WorkshopDir, filepath.FromSlash(f.RelPath))
		if err := os.WriteFile(targetPath, raw, 0o644); err != nil {
			return st, fmt.Errorf("还原文件 %s 失败（若文明 VI 正在运行请先退出游戏）: %w", f.RelPath, err)
		}
	}
	_ = os.RemoveAll(dir)
	return inspectModPatch(spec, libraryRoots, backupRoot), nil
}

const patchedRuivoUIAutoDeploymentLua = "--自动驻扎 (Civ6Relay MP Hotfix)\r\n" +
	"function ruivo_AutoGarrison(playerID, unitID, iX, iY)\r\n" +
	"    if playerID ~= Game.GetLocalPlayer() then return end\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    if not pPlayer or not pPlayer:IsHuman() then return end\r\n" +
	"    local pUnit = UnitManager.GetUnit(playerID, unitID)\r\n" +
	"    if not pUnit then return end\r\n" +
	"    local thisUnit = GameInfo.Units[pUnit:GetType()]\r\n" +
	"    if not thisUnit or thisUnit.PromotionClass == \"PROMOTION_CLASS_RECON\" then return end\r\n" +
	"    if pUnit:GetProperty(\"ruivoAutoGarrison\") then\r\n" +
	"        UnitManager.RequestOperation(pUnit, UnitOperationTypes.FORTIFY)\r\n" +
	"        UnitManager.RequestOperation(pUnit, UnitOperationTypes.ALERT)\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"Events.UnitTeleported.Add(ruivo_AutoGarrison)\r\n"

const patchedRuivoGPAutoDeploymentLua = "--城市生产单位时，自动分配到城市空的单元格上，如果没有空单元格，则跳过（就正常的在市中心生产）\r\n" +
	"--必须是陆地战斗单位（后期可考虑建造者）\r\n" +
	"--不能在战争状态下触发（蛮族与自由城市除外）\r\n" +
	"--分配后自动驻扎（告诉UI部分）\r\n" +
	"--玩家必须是人类\r\n" +
	"-- Civ6Relay MP Hotfix: sync upgrade flag on player property & deterministic iteration.\r\n" +
	"\r\n" +
	"function ruivo_OnUnitAddedToMap(playerID:number, unitID:number, iX:number, iY:number)\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    if not pPlayer or not pPlayer:IsHuman() then return end\r\n" +
	"    if pPlayer:GetProperty(\"ruivoUnitUpgraded\") then return end\r\n" +
	"\r\n" +
	"    --与其他文明处于战争状态则不触发（蛮族和自由城市除外）\r\n" +
	"    for _, i in ipairs(PlayerManager.GetAliveIDs()) do\r\n" +
	"        local otherPlayer = Players[i]\r\n" +
	"        if i ~= playerID and otherPlayer and otherPlayer:IsAlive() then\r\n" +
	"            local cfg = PlayerConfigurations[i]\r\n" +
	"            local civType = cfg and cfg:GetCivilizationTypeName()\r\n" +
	"            local civLevel = civType and GameInfo.Civilizations[civType] and GameInfo.Civilizations[civType].StartingCivilizationLevelType\r\n" +
	"            if civLevel ~= \"CIVILIZATION_LEVEL_FREE_CITIES\" and civLevel ~= \"CIVILIZATION_LEVEL_TRIBE\" then\r\n" +
	"                if pPlayer:GetDiplomacy():IsAtWarWith(i) then\r\n" +
	"                    return\r\n" +
	"                end\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"\r\n" +
	"    --必须是自己的单元格\r\n" +
	"    local plot = Map.GetPlot(iX, iY)\r\n" +
	"    if not plot or plot:GetOwner() ~= playerID then return end\r\n" +
	"\r\n" +
	"    --必须是陆地战斗单位\r\n" +
	"    local pUnit = UnitManager.GetUnit(playerID, unitID)\r\n" +
	"    if not pUnit then return end\r\n" +
	"    local thisUnit = GameInfo.Units[pUnit:GetType()]\r\n" +
	"    if not thisUnit or thisUnit.Domain ~= \"DOMAIN_LAND\" then return end\r\n" +
	"    if thisUnit.FormationClass ~= \"FORMATION_CLASS_LAND_COMBAT\" then return end\r\n" +
	"\r\n" +
	"    --开始放置\r\n" +
	"    local pCity = Cities.GetPlotPurchaseCity(plot)\r\n" +
	"    if pCity then\r\n" +
	"        local pCityPlots = pCity:GetOwnedPlots()\r\n" +
	"        for _, pPlot in ipairs(pCityPlots) do\r\n" +
	"            --不是水域、可通行\r\n" +
	"            if pPlot\r\n" +
	"                and not pPlot:IsWater()\r\n" +
	"                and not pPlot:IsImpassable()\r\n" +
	"                then\r\n" +
	"                -- 检查是否已有陆地近战单位或者非自己的单位\r\n" +
	"                local canPlaceUnit = true\r\n" +
	"                local aUnits = Units.GetUnitsInPlot(pPlot)\r\n" +
	"                for _, plotUnit in ipairs(aUnits) do\r\n" +
	"                    if plotUnit:GetOwner() ~= playerID then canPlaceUnit = false break end\r\n" +
	"                    local unitDef = GameInfo.Units[plotUnit:GetType()]\r\n" +
	"                    if unitDef and unitDef.FormationClass == \"FORMATION_CLASS_LAND_COMBAT\" then\r\n" +
	"                        canPlaceUnit = false\r\n" +
	"                        break\r\n" +
	"                    end\r\n" +
	"                end\r\n" +
	"                if canPlaceUnit then\r\n" +
	"                    pUnit:SetProperty(\"ruivoAutoGarrison\", true)\r\n" +
	"                    UnitManager.PlaceUnit(pUnit, pPlot:GetX(), pPlot:GetY())\r\n" +
	"                    UnitManager.RestoreMovementToFormation(pUnit)\r\n" +
	"                    return\r\n" +
	"                end\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function Initialize()\r\n" +
	"    Events.UnitAddedToMap.Add(ruivo_OnUnitAddedToMap)\r\n" +
	"end\r\n" +
	"Events.LoadGameViewStateDone.Add(Initialize)\r\n" +
	"\r\n" +
	"--广度搜索：以城市中心为起点，一层层往外扫描，收集属于该城市的单元格\r\n" +
	"function ruivoGetCityPlots(pCity, maxRing)\r\n" +
	"    maxRing = tonumber(maxRing)\r\n" +
	"    local CityPlots = {}\r\n" +
	"    local Visited = {}\r\n" +
	"    local Queue = {}\r\n" +
	"\r\n" +
	"    local centerX, centerY = pCity:GetX(), pCity:GetY()\r\n" +
	"    local centerPlot = Map.GetPlot(centerX, centerY)\r\n" +
	"    if not centerPlot then return CityPlots end\r\n" +
	"\r\n" +
	"    Queue[1] = centerPlot\r\n" +
	"    Visited[centerPlot] = true\r\n" +
	"    table.insert(CityPlots, centerPlot)\r\n" +
	"\r\n" +
	"    while #Queue > 0 do\r\n" +
	"        local currentPlot = table.remove(Queue, 1)\r\n" +
	"        local distance = Map.GetPlotDistance(centerX, centerY, currentPlot:GetX(), currentPlot:GetY())\r\n" +
	"        if distance < maxRing then\r\n" +
	"            local adjPlots = Map.GetAdjacentPlots(currentPlot:GetX(), currentPlot:GetY())\r\n" +
	"            for _, adjPlot in ipairs(adjPlots) do\r\n" +
	"                if adjPlot and not Visited[adjPlot] then\r\n" +
	"                    Visited[adjPlot] = true\r\n" +
	"                    table.insert(Queue, adjPlot)\r\n" +
	"                    if Cities.GetPlotPurchaseCity(adjPlot) == pCity then\r\n" +
	"                        table.insert(CityPlots, adjPlot)\r\n" +
	"                    end\r\n" +
	"                end\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"    return CityPlots\r\n" +
	"end\r\n" +
	"\r\n" +
	"--触发警戒/驻扎后关闭自动驻扎标记\r\n" +
	"function OnUnitOperationStarted(playerID, unitID, operationID)\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    if not pPlayer or not pPlayer:IsHuman() then return end\r\n" +
	"    local pUnit = UnitManager.GetUnit(playerID, unitID)\r\n" +
	"    if not pUnit then return end\r\n" +
	"    local opInfo = GameInfo.UnitOperations[operationID]\r\n" +
	"    if not opInfo then return end\r\n" +
	"    local opType = opInfo.OperationType\r\n" +
	"    if opType == \"UNITOPERATION_FORTIFY\" or opType == \"UNITOPERATION_ALERT\" then\r\n" +
	"        if pUnit:GetProperty(\"ruivoAutoGarrison\") then\r\n" +
	"            pUnit:SetProperty(\"ruivoAutoGarrison\", false)\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"Events.UnitOperationStarted.Add(OnUnitOperationStarted)\r\n" +
	"\r\n" +
	"local UPGRADE_COMMAND = \"UNITCOMMAND_UPGRADE\"\r\n" +
	"\r\n" +
	"function OnUnitCommandStarted_UpgradeDebug(playerID, unitID, commandType, data1)\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    if not pPlayer or not pPlayer:IsHuman() then return end\r\n" +
	"    local unit = UnitManager.GetUnit(playerID, unitID)\r\n" +
	"    if not unit then return end\r\n" +
	"    local cmdInfo = GameInfo.Types[commandType]\r\n" +
	"    if cmdInfo and cmdInfo.Type == UPGRADE_COMMAND then\r\n" +
	"        pPlayer:SetProperty(\"ruivoUnitUpgraded\", not pPlayer:GetProperty(\"ruivoUnitUpgraded\"))\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"Events.UnitCommandStarted.Add(OnUnitCommandStarted_UpgradeDebug)\r\n"

const patchedAPMSGPLua = "-- Civ6Relay MP Hotfix: deterministic unit location, nil-guards, and sorted siege list.\r\n" +
	"local Sieging_City_List = {}\r\n" +
	"local Sieged_City_List = {}\r\n" +
	"\r\n" +
	"local APMS_Lite_Mode = 0\r\n" +
	"local APMS_Switch = 1\r\n" +
	"\r\n" +
	"local Gain_Morale_From_Advantage_Attack = 1\r\n" +
	"local Gain_Morale_From_Unit_Killed = 1\r\n" +
	"local Turn_Multiple_For_Ajust_Morale = 2\r\n" +
	"local Max_Morale = 10\r\n" +
	"\r\n" +
	"local function APMS_AnyHumanAtWar()\r\n" +
	"    local players = Game.GetPlayers{Alive = true}\r\n" +
	"    for _, p1 in ipairs(players) do\r\n" +
	"        if p1:IsHuman() then\r\n" +
	"            for _, p2 in ipairs(players) do\r\n" +
	"                if p2:GetID() < 62 and p2:GetID() ~= p1:GetID() and p1:GetDiplomacy():IsAtWarWith(p2:GetID()) then\r\n" +
	"                    return true\r\n" +
	"                end\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"    return false\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_Switch_OnDiplomacyDeclareWar(firstPlayerID, secondPlayerID)\r\n" +
	"    if APMS_AnyHumanAtWar() then\r\n" +
	"        APMS_Switch = 1\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_Switch_OnDiplomacyMakePeace(firstPlayerID, secondPlayerID)\r\n" +
	"    if APMS_AnyHumanAtWar() then\r\n" +
	"        APMS_Switch = 1\r\n" +
	"    else\r\n" +
	"        APMS_Switch = 0\r\n" +
	"        local players = Game.GetPlayers{Alive = true}\r\n" +
	"        for _, player in ipairs(players) do\r\n" +
	"            for _, pUnit in player:GetUnits():Members() do\r\n" +
	"                APMS_PropertySet(pUnit:GetOwner(), pUnit:GetID(), 0)\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function Initialize_APMS_Switch()\r\n" +
	"    APMS_Switch = APMS_AnyHumanAtWar() and 1 or 0\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_GP_OnUnitKilledInCombat(killedPlayerID, killedUnitID, playerID, unitID)\r\n" +
	"    if APMS_Switch ~= 1 then return end\r\n" +
	"    local pUnit = UnitManager.GetUnit(playerID, unitID)\r\n" +
	"    if pUnit == nil then return end\r\n" +
	"    local location = pUnit:GetLocation()\r\n" +
	"    if location == nil then return end\r\n" +
	"    local pPlots = Map.GetNeighborPlots(location.x, location.y, 2)\r\n" +
	"    for _, pPlot in ipairs(pPlots) do\r\n" +
	"        local pUnits = Units.GetUnitsInPlot(pPlot)\r\n" +
	"        for _, iUnit in ipairs(pUnits) do\r\n" +
	"            local iPlayerId = iUnit:GetOwner()\r\n" +
	"            if iPlayerId == killedPlayerID then\r\n" +
	"                local Former_Pro = iUnit:GetProperty('PRO_APMS') or 0\r\n" +
	"                APMS_PropertySet(iPlayerId, iUnit:GetID(), Former_Pro - Gain_Morale_From_Unit_Killed)\r\n" +
	"            elseif iPlayerId == playerID then\r\n" +
	"                local Former_Pro = iUnit:GetProperty('PRO_APMS') or 0\r\n" +
	"                APMS_PropertySet(iPlayerId, iUnit:GetID(), Former_Pro + Gain_Morale_From_Unit_Killed)\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_GP_OnGameTurnStarted(playerID)\r\n" +
	"    if APMS_Switch ~= 1 then return end\r\n" +
	"    local currentTurn = Game.GetCurrentGameTurn()\r\n" +
	"    if currentTurn % Turn_Multiple_For_Ajust_Morale == 1 then\r\n" +
	"        local players = Game.GetPlayers{Alive = true}\r\n" +
	"        for _, player in ipairs(players) do\r\n" +
	"            for _, pUnit in player:GetUnits():Members() do\r\n" +
	"                local Former_Pro = pUnit:GetProperty('PRO_APMS') or 0\r\n" +
	"                if Former_Pro > 0 then\r\n" +
	"                    APMS_PropertySet(pUnit:GetOwner(), pUnit:GetID(), Former_Pro - 1)\r\n" +
	"                elseif Former_Pro < 0 then\r\n" +
	"                    APMS_PropertySet(pUnit:GetOwner(), pUnit:GetID(), Former_Pro + 1)\r\n" +
	"                end\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_GP_OnCombat(m_combatResults)\r\n" +
	"    if APMS_Switch ~= 1 or m_combatResults == nil then return end\r\n" +
	"    local attacker = m_combatResults[CombatResultParameters.ATTACKER]\r\n" +
	"    local defender = m_combatResults[CombatResultParameters.DEFENDER]\r\n" +
	"    if attacker == nil or defender == nil then return end\r\n" +
	"    local atkDmg = attacker[CombatResultParameters.DAMAGE_TO]\r\n" +
	"    local defDmg = defender[CombatResultParameters.DAMAGE_TO]\r\n" +
	"    if atkDmg ~= nil and defDmg ~= nil and atkDmg < defDmg and atkDmg ~= 0 then\r\n" +
	"        local info = attacker[CombatResultParameters.ID]\r\n" +
	"        if info ~= nil then\r\n" +
	"            local pUnit = UnitManager.GetUnit(info.player, info.id)\r\n" +
	"            if pUnit ~= nil then\r\n" +
	"                APMS_PropertySet(info.player, info.id, (pUnit:GetProperty('PRO_APMS') or 0) + Gain_Morale_From_Advantage_Attack)\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"local function APMS_SortCityList(list)\r\n" +
	"    table.sort(list, function(a, b)\r\n" +
	"        if a[1] ~= b[1] then return a[1] < b[1] end\r\n" +
	"        return a[2] < b[2]\r\n" +
	"    end)\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_GP_OnCitySiegeStatusChanged(playerID, cityID, bIsBesieged)\r\n" +
	"    local pCity = CityManager.GetCity(playerID, cityID)\r\n" +
	"    if pCity == nil then return end\r\n" +
	"    local pPlot = Map.GetPlot(pCity:GetX(), pCity:GetY())\r\n" +
	"    if pPlot == nil then return end\r\n" +
	"    local Former_Pro_City = pPlot:GetProperty('PRO_APMS_CITY') or 0\r\n" +
	"\r\n" +
	"    for i = #Sieging_City_List, 1, -1 do\r\n" +
	"        local v = Sieging_City_List[i]\r\n" +
	"        if v[1] == playerID and v[2] == cityID then\r\n" +
	"            table.remove(Sieging_City_List, i)\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"    if bIsBesieged == true then\r\n" +
	"        if Former_Pro_City < 1 then\r\n" +
	"            APMS_SIEGE_PropertySet(pCity:GetX(), pCity:GetY(), 1)\r\n" +
	"        end\r\n" +
	"        table.insert(Sieging_City_List, { playerID, cityID })\r\n" +
	"        APMS_SortCityList(Sieging_City_List)\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_GP_OnTurnEnd()\r\n" +
	"    local Sieging_Index = {}\r\n" +
	"    APMS_SortCityList(Sieging_City_List)\r\n" +
	"\r\n" +
	"    for _, v in ipairs(Sieging_City_List) do\r\n" +
	"        local playerID = v[1]\r\n" +
	"        local cityID = v[2]\r\n" +
	"        Sieging_Index[playerID .. \":\" .. cityID] = true\r\n" +
	"        local pCity = CityManager.GetCity(playerID, cityID)\r\n" +
	"        if pCity ~= nil then\r\n" +
	"            local pPlot = Map.GetPlot(pCity:GetX(), pCity:GetY())\r\n" +
	"            if pPlot ~= nil then\r\n" +
	"                local Former_Pro_City = pPlot:GetProperty('PRO_APMS_CITY') or 0\r\n" +
	"                APMS_SIEGE_PropertySet(pCity:GetX(), pCity:GetY(), Former_Pro_City + 1)\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"\r\n" +
	"    APMS_SortCityList(Sieged_City_List)\r\n" +
	"    for i = #Sieged_City_List, 1, -1 do\r\n" +
	"        local v = Sieged_City_List[i]\r\n" +
	"        local playerID = v[1]\r\n" +
	"        local cityID = v[2]\r\n" +
	"        if not Sieging_Index[playerID .. \":\" .. cityID] then\r\n" +
	"            local pCity = CityManager.GetCity(playerID, cityID)\r\n" +
	"            if pCity ~= nil then\r\n" +
	"                APMS_SIEGE_PropertySet(pCity:GetX(), pCity:GetY(), 0)\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"\r\n" +
	"    Sieged_City_List = {}\r\n" +
	"    for _, v in ipairs(Sieging_City_List) do\r\n" +
	"        table.insert(Sieged_City_List, v)\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_GP_Initialize_SIEGE()\r\n" +
	"    Sieging_City_List = {}\r\n" +
	"    Sieged_City_List = {}\r\n" +
	"    local players = Game.GetPlayers{Alive = true}\r\n" +
	"    for _, player in ipairs(players) do\r\n" +
	"        for _, pCity in player:GetCities():Members() do\r\n" +
	"            local pPlot = Map.GetPlot(pCity:GetX(), pCity:GetY())\r\n" +
	"            if pPlot ~= nil then\r\n" +
	"                local Former_Pro_City = pPlot:GetProperty('PRO_APMS_CITY') or 0\r\n" +
	"                if Former_Pro_City > 0 then\r\n" +
	"                    table.insert(Sieging_City_List, { pCity:GetOwner(), pCity:GetID() })\r\n" +
	"                    table.insert(Sieged_City_List, { pCity:GetOwner(), pCity:GetID() })\r\n" +
	"                end\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"    APMS_SortCityList(Sieging_City_List)\r\n" +
	"    APMS_SortCityList(Sieged_City_List)\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_PropertySet(PlayerID, UnitID, Value)\r\n" +
	"    if Value > Max_Morale or Value < -Max_Morale then return end\r\n" +
	"    local pUnit = UnitManager.GetUnit(PlayerID, UnitID)\r\n" +
	"    if pUnit ~= nil then\r\n" +
	"        pUnit:SetProperty('PRO_APMS', Value)\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function APMS_SIEGE_PropertySet(iX, iY, Value)\r\n" +
	"    local pPlot = Map.GetPlot(iX, iY)\r\n" +
	"    if pPlot == nil then return end\r\n" +
	"    pPlot:SetProperty('PRO_APMS_CITY', Value)\r\n" +
	"    pPlot:SetProperty('PRO_APMS_CITY_1', Value % 2)\r\n" +
	"    pPlot:SetProperty('PRO_APMS_CITY_2', (math.floor(Value / 2)) % 2)\r\n" +
	"    pPlot:SetProperty('PRO_APMS_CITY_4', (math.floor(Value / 4)) % 2)\r\n" +
	"    pPlot:SetProperty('PRO_APMS_CITY_8', (math.floor(Value / 8)) % 2)\r\n" +
	"    pPlot:SetProperty('PRO_APMS_CITY_16', (math.floor(Value / 16)) % 2)\r\n" +
	"end\r\n" +
	"\r\n" +
	"function Initialize()\r\n" +
	"    APMS_Lite_Mode = GameInfo.APMS_DATA[\"APMS_Lite_Mode\"].Value or 0\r\n" +
	"    if APMS_Lite_Mode == 1 then\r\n" +
	"        APMS_Switch = 0\r\n" +
	"        Events.DiplomacyDeclareWar.Add(APMS_Switch_OnDiplomacyDeclareWar)\r\n" +
	"        Events.DiplomacyMakePeace.Add(APMS_Switch_OnDiplomacyMakePeace)\r\n" +
	"        Initialize_APMS_Switch()\r\n" +
	"    end\r\n" +
	"\r\n" +
	"    Gain_Morale_From_Advantage_Attack = GameInfo.APMS_DATA[\"Gain_Morale_From_Advantage_Attack\"].Value\r\n" +
	"    Gain_Morale_From_Unit_Killed      = GameInfo.APMS_DATA[\"Gain_Morale_From_Unit_Killed\"].Value\r\n" +
	"    Turn_Multiple_For_Ajust_Morale    = GameInfo.APMS_DATA[\"Turn_Multiple_For_Ajust_Morale\"].Value\r\n" +
	"    Max_Morale                        = GameInfo.APMS_DATA[\"Max_Morale\"].Value\r\n" +
	"\r\n" +
	"    Events.UnitKilledInCombat.Add(APMS_GP_OnUnitKilledInCombat)\r\n" +
	"    GameEvents.OnGameTurnStarted.Add(APMS_GP_OnGameTurnStarted)\r\n" +
	"    Events.Combat.Add(APMS_GP_OnCombat)\r\n" +
	"    Events.CitySiegeStatusChanged.Add(APMS_GP_OnCitySiegeStatusChanged)\r\n" +
	"    Events.TurnEnd.Add(APMS_GP_OnTurnEnd)\r\n" +
	"    Events.LoadGameViewStateDone.Add(APMS_GP_Initialize_SIEGE)\r\n" +
	"end\r\n" +
	"\r\n" +
	"Initialize()\r\n"

const patchedAwaseUI0227Lua = "-- AWASE_MOD_0227_UI (Civ6Relay MP Hotfix)\r\n" +
	"local PROJECT_HASH = DB.MakeHash(\"PROJECT_AWASE_MOD_0227\")\r\n" +
	"\r\n" +
	"function OnCityProductionUpdated(playerID, cityID, objectID, productionID)\r\n" +
	"    if playerID ~= Game.GetLocalPlayer() then return end\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    if pPlayer == nil or not pPlayer:IsHuman() then return end\r\n" +
	"\r\n" +
	"    local pCity = CityManager.GetCity(playerID, cityID)\r\n" +
	"    if pCity == nil then return end\r\n" +
	"\r\n" +
	"    local pBuildQueue = pCity:GetBuildQueue()\r\n" +
	"    local currentProductionHash = pBuildQueue:GetCurrentProductionTypeHash()\r\n" +
	"    if currentProductionHash == 0 then\r\n" +
	"        currentProductionHash = pBuildQueue:GetPreviousProductionTypeHash()\r\n" +
	"    end\r\n" +
	"\r\n" +
	"    if currentProductionHash == PROJECT_HASH then\r\n" +
	"        local projectInfo = GameInfo.Projects[PROJECT_HASH]\r\n" +
	"        if projectInfo then\r\n" +
	"            local currentProgress = pBuildQueue:GetProjectProgress(projectInfo.Index)\r\n" +
	"            UI.RequestPlayerOperation(playerID, PlayerOperations.EXECUTE_SCRIPT, {\r\n" +
	"                CityID = cityID,\r\n" +
	"                NewProgress = currentProgress,\r\n" +
	"                OnStart = \"AM0227SyncProjectProgress\"\r\n" +
	"            })\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"\r\n" +
	"function OnGameLoadComplete()\r\n" +
	"    Events.CityProductionUpdated.Add(OnCityProductionUpdated)\r\n" +
	"end\r\n" +
	"\r\n" +
	"Events.LoadGameViewStateDone.Add(OnGameLoadComplete)\r\n"

const patchedAwaseGameplay0227Lua = "-- AWASE_MOD_0227_Gameplay (Civ6Relay MP Hotfix)\r\n" +
	"local RESOURCE_NAME = \"RESOURCE_AWASE_MOD_0227_POINTS\"\r\n" +
	"local PROPERTY_KEY = \"AWASE_MOD_0227_LAST_PROGRESS\"\r\n" +
	"local POINTS_MAX = 999999\r\n" +
	"\r\n" +
	"ExposedMembers.AWASE_MOD_0227 = ExposedMembers.AWASE_MOD_0227 or {}\r\n" +
	"\r\n" +
	"function SyncProjectProgress(playerID, cityID, newProgress)\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    local pCity = CityManager.GetCity(playerID, cityID)\r\n" +
	"    if not pCity or not pPlayer then return end\r\n" +
	"\r\n" +
	"    local lastProgress = pCity:GetProperty(PROPERTY_KEY) or 0\r\n" +
	"    local amount = newProgress - lastProgress\r\n" +
	"    if amount > 0 then\r\n" +
	"        local resIdx = GameInfo.Resources[RESOURCE_NAME].Index\r\n" +
	"        pPlayer:GetResources():ChangeResourceAmount(resIdx, amount)\r\n" +
	"        pCity:SetProperty(PROPERTY_KEY, newProgress)\r\n" +
	"    elseif amount < 0 then\r\n" +
	"        pCity:SetProperty(PROPERTY_KEY, newProgress)\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"ExposedMembers.AWASE_MOD_0227.SyncProjectProgress = SyncProjectProgress\r\n" +
	"\r\n" +
	"GameEvents.AM0227SyncProjectProgress.Add(\"AM0227SyncProjectProgress\", function(playerID, args)\r\n" +
	"    if args == nil or args.CityID == nil or args.NewProgress == nil then return end\r\n" +
	"    SyncProjectProgress(playerID, args.CityID, args.NewProgress)\r\n" +
	"end)\r\n" +
	"\r\n" +
	"GameEvents.AM0227ExecutePurchase.Add(\"AM0227ExecutePurchase\", function(playerID, args)\r\n" +
	"    local pCity = CityManager.GetCity(playerID, args.CityID)\r\n" +
	"    if not pCity then return end\r\n" +
	"    pCity:GetBuildQueue():AddProgress(args.Cost)\r\n" +
	"    ReportingEvents.SendLuaEvent(\"AM0227_ProgressAdded\", {\r\n" +
	"        playerID = playerID,\r\n" +
	"        cityID = args.CityID\r\n" +
	"    })\r\n" +
	"end)\r\n" +
	"\r\n" +
	"GameEvents.AM0227DeductPoints.Add(\"AM0227DeductPoints\", function(playerID, args)\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    if not pPlayer then return end\r\n" +
	"    local resIdx = GameInfo.Resources[RESOURCE_NAME].Index\r\n" +
	"    pPlayer:GetResources():ChangeResourceAmount(resIdx, -args.Amount)\r\n" +
	"end)\r\n" +
	"\r\n" +
	"function OnPlayerTurnActivated(playerID, isFirstTime)\r\n" +
	"    local pPlayer = Players[playerID]\r\n" +
	"    if not pPlayer or not pPlayer:IsHuman() or not isFirstTime then return end\r\n" +
	"\r\n" +
	"    local playerCulture = pPlayer:GetCulture()\r\n" +
	"    if playerCulture then\r\n" +
	"        local policy = GameInfo.Policies[\"POLICY_AWASE_MOD_0227\"]\r\n" +
	"        if not policy then return end\r\n" +
	"        if playerCulture:IsPolicyActive(policy.Index) then\r\n" +
	"            local resIdx = GameInfo.Resources[RESOURCE_NAME].Index\r\n" +
	"            if not resIdx then return end\r\n" +
	"            local currentAmount = pPlayer:GetResources():GetResourceAmount(resIdx)\r\n" +
	"            if currentAmount > 0 then\r\n" +
	"                local delta = currentAmount * 0.05\r\n" +
	"                if currentAmount + delta > POINTS_MAX then\r\n" +
	"                    delta = POINTS_MAX - currentAmount\r\n" +
	"                end\r\n" +
	"                pPlayer:GetResources():ChangeResourceAmount(resIdx, delta)\r\n" +
	"            end\r\n" +
	"        end\r\n" +
	"    end\r\n" +
	"end\r\n" +
	"Events.PlayerTurnActivated.Add(OnPlayerTurnActivated)\r\n"
