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

// palette holds every color the custom theme overrides.
type palette struct {
	background     color.Color
	overlay        color.Color
	menu           color.Color
	header         color.Color
	input          color.Color
	inputBorder    color.Color
	button         color.Color
	disabledButton color.Color
	foreground     color.Color
	placeholder    color.Color
	disabled       color.Color
	primary        color.Color
	hover          color.Color
	focus          color.Color
	selection      color.Color
	pressed        color.Color
	separator      color.Color
	scrollBar      color.Color
	shadow         color.Color
	hyperlink      color.Color
	success        color.Color
	warning        color.Color
	errorCol       color.Color
}

type mtsshTheme struct {
	base fyne.Theme
	p    palette
}

func (t *mtsshTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	if c := t.lookup(name); c != nil {
		return c
	}
	return t.base.Color(name, variant)
}

func (t *mtsshTheme) lookup(name fyne.ThemeColorName) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return t.p.background
	case theme.ColorNameOverlayBackground:
		return t.p.overlay
	case theme.ColorNameMenuBackground:
		return t.p.menu
	case theme.ColorNameHeaderBackground:
		return t.p.header
	case theme.ColorNameInputBackground:
		return t.p.input
	case theme.ColorNameInputBorder:
		return t.p.inputBorder
	case theme.ColorNameButton:
		return t.p.button
	case theme.ColorNameDisabledButton:
		return t.p.disabledButton
	case theme.ColorNameForeground:
		return t.p.foreground
	case theme.ColorNamePlaceHolder:
		return t.p.placeholder
	case theme.ColorNameDisabled:
		return t.p.disabled
	case theme.ColorNamePrimary:
		return t.p.primary
	case theme.ColorNameHover:
		return t.p.hover
	case theme.ColorNameFocus:
		return t.p.focus
	case theme.ColorNameSelection:
		return t.p.selection
	case theme.ColorNamePressed:
		return t.p.pressed
	case theme.ColorNameSeparator:
		return t.p.separator
	case theme.ColorNameScrollBar:
		return t.p.scrollBar
	case theme.ColorNameShadow:
		return t.p.shadow
	case theme.ColorNameHyperlink:
		return t.p.hyperlink
	case theme.ColorNameSuccess:
		return t.p.success
	case theme.ColorNameWarning:
		return t.p.warning
	case theme.ColorNameError:
		return t.p.errorCol
	}
	return nil
}

func (t *mtsshTheme) Font(style fyne.TextStyle) fyne.Resource    { return t.base.Font(style) }
func (t *mtsshTheme) Icon(name fyne.ThemeIconName) fyne.Resource { return t.base.Icon(name) }
func (t *mtsshTheme) Size(name fyne.ThemeSizeName) float32       { return t.base.Size(name) }

// NewTheme returns a fyne.Theme for the given ThemeName
func NewTheme(name ThemeName) fyne.Theme {
	base := theme.DefaultTheme()
	switch name {
	case ThemeLight:
		return &mtsshTheme{
			base: base,
			p: palette{
				background:     rgb(0xF5, 0xF5, 0xF5),
				overlay:        rgb(0xFF, 0xFF, 0xFF),
				menu:           rgb(0xFF, 0xFF, 0xFF),
				header:         rgb(0xEA, 0xEA, 0xEA),
				input:          rgb(0xFF, 0xFF, 0xFF),
				inputBorder:    rgb(0xC8, 0xC8, 0xC8),
				button:         rgb(0xE0, 0xE0, 0xE0),
				disabledButton: rgb(0xEE, 0xEE, 0xEE),
				foreground:     rgb(0x21, 0x21, 0x21),
				placeholder:    rgb(0x8A, 0x8A, 0x8A),
				disabled:       rgb(0xA0, 0xA0, 0xA0),
				primary:        rgb(0x5B, 0x8A, 0x72),
				hover:          rgba(0x5B, 0x8A, 0x72, 0x22),
				focus:          rgba(0x5B, 0x8A, 0x72, 0x55),
				selection:      rgba(0x5B, 0x8A, 0x72, 0x55),
				pressed:        rgba(0x00, 0x00, 0x00, 0x22),
				separator:      rgb(0xD0, 0xD4, 0xDA),
				scrollBar:      rgba(0x00, 0x00, 0x00, 0x55),
				shadow:         rgba(0x00, 0x00, 0x00, 0x33),
				hyperlink:      rgb(0x4A, 0x75, 0x60),
				success:        rgb(0x98, 0xC3, 0x79),
				warning:        rgb(0xE5, 0xC0, 0x7B),
				errorCol:       rgb(0xE0, 0x6C, 0x75),
			},
		}
	case ThemeSolarized:
		return &mtsshTheme{
			base: base,
			p: palette{
				background:     rgb(0x00, 0x2B, 0x36),
				overlay:        rgb(0x07, 0x36, 0x42),
				menu:           rgb(0x07, 0x36, 0x42),
				header:         rgb(0x07, 0x36, 0x42),
				input:          rgb(0x07, 0x36, 0x42),
				inputBorder:    rgb(0x58, 0x6E, 0x75),
				button:         rgb(0x07, 0x36, 0x42),
				disabledButton: rgb(0x00, 0x2B, 0x36),
				foreground:     rgb(0x93, 0xA1, 0xA1),
				placeholder:    rgb(0x58, 0x6E, 0x75),
				disabled:       rgb(0x58, 0x6E, 0x75),
				primary:        rgb(0x26, 0x8B, 0xD2),
				hover:          rgba(0x26, 0x8B, 0xD2, 0x22),
				focus:          rgba(0x26, 0x8B, 0xD2, 0x55),
				selection:      rgba(0x26, 0x8B, 0xD2, 0x55),
				pressed:        rgba(0xFF, 0xFF, 0xFF, 0x11),
				separator:      rgb(0x07, 0x36, 0x42),
				scrollBar:      rgba(0x00, 0x00, 0x00, 0x88),
				shadow:         rgba(0x00, 0x00, 0x00, 0x88),
				hyperlink:      rgb(0x2A, 0xA1, 0x98),
				success:        rgb(0x85, 0x99, 0x00),
				warning:        rgb(0xB5, 0x89, 0x00),
				errorCol:       rgb(0xDC, 0x32, 0x2F),
			},
		}
	case ThemeNord:
		return &mtsshTheme{
			base: base,
			p: palette{
				background:     rgb(0x2E, 0x34, 0x40),
				overlay:        rgb(0x3B, 0x42, 0x52),
				menu:           rgb(0x3B, 0x42, 0x52),
				header:         rgb(0x3B, 0x42, 0x52),
				input:          rgb(0x3B, 0x42, 0x52),
				inputBorder:    rgb(0x4C, 0x56, 0x6A),
				button:         rgb(0x43, 0x4C, 0x5E),
				disabledButton: rgb(0x3B, 0x42, 0x52),
				foreground:     rgb(0xEC, 0xEF, 0xF4),
				placeholder:    rgb(0x81, 0xA1, 0xC1),
				disabled:       rgb(0x61, 0x67, 0x78),
				primary:        rgb(0x88, 0xC0, 0xD0),
				hover:          rgba(0x88, 0xC0, 0xD0, 0x22),
				focus:          rgba(0x88, 0xC0, 0xD0, 0x55),
				selection:      rgba(0x88, 0xC0, 0xD0, 0x55),
				pressed:        rgba(0xFF, 0xFF, 0xFF, 0x11),
				separator:      rgb(0x43, 0x4C, 0x5E),
				scrollBar:      rgba(0x00, 0x00, 0x00, 0x88),
				shadow:         rgba(0x00, 0x00, 0x00, 0x88),
				hyperlink:      rgb(0x88, 0xC0, 0xD0),
				success:        rgb(0xA3, 0xBE, 0x8C),
				warning:        rgb(0xEB, 0xCB, 0x8B),
				errorCol:       rgb(0xBF, 0x61, 0x6A),
			},
		}
	default: // ThemeDark — matches the MTSSH landing page palette
		return &mtsshTheme{
			base: base,
			p: palette{
				background:     rgb(0x15, 0x15, 0x1F), // --bg
				overlay:        rgb(0x1A, 0x1A, 0x28), // --surface
				menu:           rgb(0x1A, 0x1A, 0x28), // --surface
				header:         rgb(0x25, 0x25, 0x37), // --bg-3
				input:          rgb(0x1E, 0x1E, 0x2E), // --bg-2
				inputBorder:    rgb(0x3A, 0x3A, 0x52), // --border-2
				button:         rgb(0x25, 0x25, 0x37), // --bg-3
				disabledButton: rgb(0x1E, 0x1E, 0x2E), // --bg-2
				foreground:     rgb(0xE6, 0xEC, 0xF2), // --text
				placeholder:    rgb(0x8A, 0x93, 0xA6), // --text-dim
				disabled:       rgb(0x5E, 0x64, 0x78), // --text-mute
				primary:        rgb(0x5B, 0x8A, 0x72), // --accent
				hover:          rgba(0x5B, 0x8A, 0x72, 0x22),
				focus:          rgba(0x5B, 0x8A, 0x72, 0x55),
				selection:      rgba(0x5B, 0x8A, 0x72, 0x55),
				pressed:        rgba(0xFF, 0xFF, 0xFF, 0x11),
				separator:      rgb(0x2C, 0x2C, 0x40), // --border
				scrollBar:      rgba(0x3A, 0x3A, 0x52, 0xCC),
				shadow:         rgba(0x00, 0x00, 0x00, 0x99),
				hyperlink:      rgb(0x7A, 0xB1, 0x8F), // --accent-2
				success:        rgb(0x98, 0xC3, 0x79), // --grn
				warning:        rgb(0xE5, 0xC0, 0x7B), // --yel
				errorCol:       rgb(0xE0, 0x6C, 0x75), // --red
			},
		}
	}
}

func rgb(r, g, b uint8) color.Color {
	return color.NRGBA{R: r, G: g, B: b, A: 0xFF}
}

func rgba(r, g, b, a uint8) color.Color {
	return color.NRGBA{R: r, G: g, B: b, A: a}
}
