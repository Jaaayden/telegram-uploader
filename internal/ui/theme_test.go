package ui

import (
	"bytes"
	"image/color"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/software"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/go-text/typesetting/font"
)

func TestAppThemeFontsCoverUIText(t *testing.T) {
	// Representative UI text, file-name characters and punctuation.
	const sample = "已就绪，可以开始或定时上传。Telegram 连接已建立 队列条目详情：视频文件名示例（第10集）.mp4 “暂停”—…"
	for name, res := range map[string]fyne.Resource{"regular": cjkRegular, "bold": cjkBold} {
		face, err := font.ParseTTF(bytes.NewReader(res.Content()))
		if err != nil {
			t.Fatalf("%s font does not parse: %v", name, err)
		}
		for _, r := range sample {
			if _, ok := face.NominalGlyph(r); !ok {
				t.Errorf("%s font has no glyph for %q (U+%04X)", name, r, r)
			}
		}
	}
}

func TestAppThemeSelectsFontsByStyle(t *testing.T) {
	th := newAppTheme()
	if got := th.Font(fyne.TextStyle{}); got != cjkRegular {
		t.Errorf("regular style = %v", got.Name())
	}
	if got := th.Font(fyne.TextStyle{Bold: true}); got != cjkBold {
		t.Errorf("bold style = %v", got.Name())
	}
	if got := th.Font(fyne.TextStyle{Monospace: true}); got == cjkRegular || got == cjkBold {
		t.Error("monospace must keep Fyne's monospace font")
	}
	if got := th.Font(fyne.TextStyle{Symbol: true}); got == cjkRegular || got == cjkBold {
		t.Error("symbol must keep Fyne's symbol font")
	}
}

func TestAppThemeRendersChineseText(t *testing.T) {
	test.NewApp()
	img := software.Render(widget.NewLabel("已就绪，可以开始或定时上传"), newAppTheme())
	bounds := img.Bounds()
	dark := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if c := (color.Gray16{Y: uint16((r + g + b) / 3)}); c.Y < 0x4000 {
				dark++
			}
		}
	}
	if dark < 200 {
		t.Fatalf("rendered label has only %d dark pixels; Chinese glyphs were not drawn", dark)
	}
}
