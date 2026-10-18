package ui

import (
	_ "embed"

	"fyne.io/fyne/v2"
)

//go:embed icon.png
var iconData []byte

// appIcon is the window/taskbar/dock icon. The source is packaging/icon/icon.svg;
// regenerate with `go run packaging/icon/gen.go`.
var appIcon = fyne.NewStaticResource("icon.png", iconData)
