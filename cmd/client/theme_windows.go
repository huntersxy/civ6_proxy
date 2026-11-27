//go:build windows

package main

import (
	"github.com/go-gui-org/go-gui/gui"
)

const (
	uiFontFamily         = "Microsoft YaHei UI"
	uiFontSize   float32 = 15
)

func civ6ThemeCfg(baseName, name string) gui.ThemeCfg {
	base, ok := gui.ThemeGet(baseName)
	if !ok {
		base = gui.CurrentTheme()
	}
	cfg := base.Cfg
	cfg.Name = name
	cfg.TextStyleDef.Family = uiFontFamily
	cfg.TextStyleDef.Size = uiFontSize
	// Clear preset ladder rungs so ThemeMaker derives every text role
	// (Title, Body, Caption, CodeSmall) proportionally from uiFontSize.
	cfg.SizeTextTiny = 0
	cfg.SizeTextXSmall = 0
	cfg.SizeTextSmall = 0
	cfg.SizeTextMedium = 0
	cfg.SizeTextLarge = 0
	cfg.SizeTextXLarge = 0
	return cfg
}

// civ6Themes builds the light and dark WinUI-based themes accented with
// Civilization VI warm bronze/gold tones and unified Microsoft YaHei UI font.
func civ6Themes() (light, dark gui.Theme) {
	lightCfg := civ6ThemeCfg("windows", "civ6-light")
	light = gui.ThemeMaker(lightCfg).WithColors(gui.ColorOverrides{
		ColorBackground:  gui.Hex(0xF4F2EE),
		ColorPanel:       gui.Hex(0xFFFFFF),
		ColorInterior:    gui.Hex(0xFAF8F5),
		ColorHover:       gui.Hex(0xEDE9E0),
		ColorFocus:       gui.Hex(0xFFFFFF),
		ColorActive:      gui.Hex(0xE3DDD1),
		ColorBorder:      gui.Hex(0xDDD7CC),
		ColorSeparator:   gui.Hex(0xE5E0D7),
		ColorSelect:      gui.Hex(0x9B6B1E),
		ColorAccent:      gui.Hex(0x9B6B1E),
		ColorBorderFocus: gui.Hex(0x9B6B1E),
		ColorSuccess:     gui.Hex(0x16794B),
		ColorWarning:     gui.Hex(0xB45309),
		ColorError:       gui.Hex(0xC42B1C),
	})
	light.Name = "civ6-light"

	darkCfg := civ6ThemeCfg("windows-dark", "civ6-dark")
	dark = gui.ThemeMaker(darkCfg).WithColors(gui.ColorOverrides{
		ColorBackground:  gui.Hex(0x1A1C20),
		ColorPanel:       gui.Hex(0x23262C),
		ColorInterior:    gui.Hex(0x2C3038),
		ColorHover:       gui.Hex(0x30343E),
		ColorFocus:       gui.Hex(0x2C3038),
		ColorActive:      gui.Hex(0x3A3F4B),
		ColorBorder:      gui.Hex(0x3A3F4B),
		ColorSeparator:   gui.Hex(0x30343E),
		ColorSelect:      gui.Hex(0xD4A548),
		ColorAccent:      gui.Hex(0xD4A548),
		ColorBorderFocus: gui.Hex(0xD4A548),
		ColorSuccess:     gui.Hex(0x4ADE80),
		ColorWarning:     gui.Hex(0xFBBF24),
		ColorError:       gui.Hex(0xF87171),
	})
	dark.Name = "civ6-dark"

	return light, dark
}

func cardView(id string, sizing gui.Sizing, content ...gui.View) gui.View {
	t := gui.CurrentTheme()
	return gui.Column(gui.ContainerCfg{
		ID:          id,
		Sizing:      sizing,
		Color:       t.ColorPanel,
		ColorBorder: t.ColorBorder,
		SizeBorder:  gui.BorderThin,
		Radius:      gui.RadiusLarge,
		Padding:     gui.NewPadding(14, 16, 14, 16),
		Spacing:     gui.SpacingMedium,
		Content:     content,
	})
}

func subtlePanelView(id string, sizing gui.Sizing, content ...gui.View) gui.View {
	t := gui.CurrentTheme()
	return gui.Column(gui.ContainerCfg{
		ID:          id,
		Sizing:      sizing,
		Color:       t.ColorBackground,
		ColorBorder: t.ColorBorder,
		SizeBorder:  gui.BorderThin,
		Radius:      gui.RadiusMedium,
		Padding:     gui.NewPadding(10, 12, 10, 12),
		Spacing:     gui.SpacingSmall,
		Content:     content,
	})
}

func kpiTileView(label, value string, valueStyle gui.TextStyle) gui.View {
	t := gui.CurrentTheme()
	if valueStyle == (gui.TextStyle{}) {
		valueStyle = t.TextStyleTitleSmall
	}
	return gui.Column(gui.ContainerCfg{
		Sizing:      gui.FillFit,
		Color:       t.ColorBackground,
		ColorBorder: t.ColorBorder,
		SizeBorder:  gui.BorderThin,
		Radius:      gui.RadiusMedium,
		Padding:     gui.NewPadding(10, 12, 10, 12),
		Spacing:     gui.SpacingPx(4),
		Content: []gui.View{
			gui.Text(gui.TextCfg{
				Text:      label,
				TextStyle: t.TextStyleSecondary,
			}),
			gui.Text(gui.TextCfg{
				Text:      value,
				TextStyle: valueStyle,
			}),
		},
	})
}

type tableColSpec struct {
	title string
	width float32 // 0 = FillFit
}

// flexTableView renders a full-width responsive data table with a subtle
// header bar and hairline horizontal dividers.
func flexTableView(id string, sizing gui.Sizing, scrollable bool, cols []tableColSpec, rows [][]gui.View) gui.View {
	t := gui.CurrentTheme()

	cellWrap := func(col tableColSpec, child gui.View) gui.View {
		cfg := gui.ContainerCfg{
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			VAlign:     gui.VAlignMiddle,
			Content:    []gui.View{child},
		}
		if col.width > 0 {
			cfg.Width = col.width
			cfg.Sizing = gui.FixedFit
		} else {
			cfg.Sizing = gui.FillFit
		}
		return gui.Row(cfg)
	}

	headerCells := make([]gui.View, len(cols))
	for i, col := range cols {
		headerCells[i] = cellWrap(col, gui.Text(gui.TextCfg{
			Text:      col.title,
			TextStyle: t.TextStyleLabel,
		}))
	}

	headerRow := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Color:      t.ColorBackground,
		Radius:     gui.RadiusMedium,
		Padding:    gui.NewPadding(8, 12, 8, 12),
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		VAlign:     gui.VAlignMiddle,
		Content:    headerCells,
	})

	bodyRows := make([]gui.View, 0, len(rows)*2)
	for rIdx, cells := range rows {
		if rIdx > 0 {
			bodyRows = append(bodyRows, gui.Separator(gui.SeparatorCfg{}))
		}
		wrapped := make([]gui.View, len(cols))
		for cIdx, col := range cols {
			var cell gui.View
			if cIdx < len(cells) {
				cell = cells[cIdx]
			} else {
				cell = gui.Text(gui.TextCfg{Text: "", TextStyle: t.TextStyleBody})
			}
			wrapped[cIdx] = cellWrap(col, cell)
		}
		bodyRows = append(bodyRows, gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NewPadding(9, 12, 9, 12),
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingMedium,
			VAlign:     gui.VAlignMiddle,
			Hover:      gui.HoverStyle{Color: t.ColorHover},
			Radius:     gui.RadiusSmall,
			Content:    wrapped,
		}))
	}

	bodyCfg := gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingPx(0),
		Content:    bodyRows,
	}
	if scrollable {
		bodyCfg.ID = id + "-scroll"
		bodyCfg.Sizing = gui.FillFill
		bodyCfg.Scrollable = true
		bodyCfg.ScrollMode = gui.ScrollVerticalOnly
	}

	return gui.Column(gui.ContainerCfg{
		ID:         id,
		Sizing:     sizing,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingPx(2),
		Content: []gui.View{
			headerRow,
			gui.Column(bodyCfg),
		},
	})
}

func textStyleWithColor(base gui.TextStyle, color gui.Color) gui.TextStyle {
	base.Color = color
	return base
}
