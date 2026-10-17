package ui

import (
	_ "embed"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

//go:embed fonts/NotoSansSC-Regular.subset.otf
var cjkRegularData []byte

//go:embed fonts/NotoSansSC-Bold.subset.otf
var cjkBoldData []byte

var (
	cjkRegular = fyne.NewStaticResource("NotoSansSC-Regular.subset.otf", cjkRegularData)
	cjkBold    = fyne.NewStaticResource("NotoSansSC-Bold.subset.otf", cjkBoldData)
)

// appTheme is Fyne's default theme with an embedded Chinese-capable font.
// Without it Fyne falls back to a different system font for each Han
// character, which mixes weights and baselines within one line of text.
type appTheme struct {
	fyne.Theme
}

func newAppTheme() fyne.Theme {
	return appTheme{Theme: theme.DefaultTheme()}
}

func (t appTheme) Font(style fyne.TextStyle) fyne.Resource {
	switch {
	case style.Monospace, style.Symbol:
		return t.Theme.Font(style)
	case style.Bold:
		return cjkBold
	default:
		return cjkRegular
	}
}
