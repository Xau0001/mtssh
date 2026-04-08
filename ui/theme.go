package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// ThemeName identifies a built-in theme
type ThemeName string

const (
	ThemeDark      ThemeName = "Dark"
	ThemeLight     ThemeName = "Light"
	ThemeSolarized ThemeName = "Solarized"
	ThemeNord      ThemeName = "Nord"
)

// AllThemes lists all available theme names
var AllThemes = []ThemeName{ThemeDark, ThemeLight, ThemeSolarized, ThemeNord}

// mtputtyTheme is a custom Fyne theme
type mtputtyTheme struct {
	base       fyne.Theme
	bg         color.Color
	fg         color.Color
	primary    color.Color
	inputBg    color.Color
	buttonBg   color.Color
	disabledFg color.Color
}

func (t *mtputtyTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return t.bg
	case theme.ColorNameForeground:
		return t.fg
	case theme.ColorNamePrimary:
		return t.primary
	case theme.ColorNameInputBackground:
		return t.inputBg
	case theme.ColorNameButton:
		return t.buttonBg
	case theme.ColorNameDisabled:
		return t.disabledFg
	}
	return t.base.Color(name, variant)
}

func (t *mtputtyTheme) Font(style fyne.TextStyle) fyne.Resource {
	return t.base.Font(style)
}

func (t *mtputtyTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return t.base.Icon(name)
}

func (t *mtputtyTheme) Size(name fyne.ThemeSizeName) float32 {
	return t.base.Size(name)
}

// NewTheme returns a fyne.Theme for the given ThemeName
func NewTheme(name ThemeName) fyne.Theme {
	base := theme.DefaultTheme()
	switch name {
	case ThemeLight:
		return &mtputtyTheme{
			base:       base,
			bg:         rgb(0xF5, 0xF5, 0xF5),
			fg:         rgb(0x21, 0x21, 0x21),
			primary:    rgb(0x00, 0x78, 0xD4),
			inputBg:    rgb(0xFF, 0xFF, 0xFF),
			buttonBg:   rgb(0xE0, 0xE0, 0xE0),
			disabledFg: rgb(0xA0, 0xA0, 0xA0),
		}
	case ThemeSolarized:
		// Solarized Dark palette
		return &mtputtyTheme{
			base:       base,
			bg:         rgb(0x00, 0x2B, 0x36),
			fg:         rgb(0x83, 0x94, 0x96),
			primary:    rgb(0x26, 0x8B, 0xD2),
			inputBg:    rgb(0x07, 0x36, 0x42),
			buttonBg:   rgb(0x07, 0x36, 0x42),
			disabledFg: rgb(0x58, 0x6E, 0x75),
		}
	case ThemeNord:
		// Nord palette
		return &mtputtyTheme{
			base:       base,
			bg:         rgb(0x2E, 0x34, 0x40),
			fg:         rgb(0xEC, 0xEF, 0xF4),
			primary:    rgb(0x88, 0xC0, 0xD0),
			inputBg:    rgb(0x3B, 0x42, 0x52),
			buttonBg:   rgb(0x43, 0x4C, 0x5E),
			disabledFg: rgb(0x61, 0x67, 0x78),
		}
	default: // ThemeDark
		return &mtputtyTheme{
			base:       base,
			bg:         rgb(0x1E, 0x1E, 0x1E),
			fg:         rgb(0xD4, 0xD4, 0xD4),
			primary:    rgb(0x00, 0x7A, 0xCC),
			inputBg:    rgb(0x25, 0x25, 0x26),
			buttonBg:   rgb(0x2D, 0x2D, 0x2D),
			disabledFg: rgb(0x6A, 0x6A, 0x6A),
		}
	}
}

func rgb(r, g, b uint8) color.Color {
	return color.RGBA{R: r, G: g, B: b, A: 0xFF}
}
