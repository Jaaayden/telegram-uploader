//go:build ignore

// gen rasterizes packaging/icon/icon.svg into the PNG/ICO assets used by the
// application. Run from the repository root:
//
//	go run packaging/icon/gen.go
//
// It writes the 512 px window icon, a 256 px README image, the Windows .ico and
// the macOS iconset directory (convert it with `iconutil -c icns`, see
// packaging/icon/README.md).
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

func render(icon *oksvg.SvgIcon, size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	icon.SetTarget(0, 0, float64(size), float64(size))
	icon.Draw(rasterx.NewDasher(size, size, rasterx.NewScannerGV(size, size, img, img.Bounds())), 1)
	return img
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
	fmt.Println("wrote", path)
}

// ico builds a PNG-compressed .ico (supported since Windows Vista).
func ico(sizes []int, icon *oksvg.SvgIcon) []byte {
	var images [][]byte
	for _, s := range sizes {
		images = append(images, encodePNG(render(icon, s)))
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := byte(s)
		if s >= 256 {
			dim = 0
		}
		out.Write([]byte{dim, dim, 0, 0})
		binary.Write(&out, binary.LittleEndian, [2]uint16{1, 32})
		binary.Write(&out, binary.LittleEndian, uint32(len(images[i])))
		binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(images[i])
	}
	for _, data := range images {
		out.Write(data)
	}
	return out.Bytes()
}

func main() {
	icon, err := oksvg.ReadIcon("packaging/icon/icon.svg", oksvg.StrictErrorMode)
	if err != nil {
		panic(err)
	}
	write("internal/ui/icon.png", encodePNG(render(icon, 512)))
	write("packaging/icon/icon-256.png", encodePNG(render(icon, 256)))
	write("packaging/windows/icon.ico", ico([]int{16, 24, 32, 48, 64, 128, 256}, icon))

	// macOS iconset names; `iconutil -c icns` turns the directory into AppIcon.icns.
	for _, e := range []struct {
		name string
		size int
	}{
		{"icon_16x16.png", 16}, {"icon_16x16@2x.png", 32},
		{"icon_32x32.png", 32}, {"icon_32x32@2x.png", 64},
		{"icon_128x128.png", 128}, {"icon_128x128@2x.png", 256},
		{"icon_256x256.png", 256}, {"icon_256x256@2x.png", 512},
		{"icon_512x512.png", 512}, {"icon_512x512@2x.png", 1024},
	} {
		write(filepath.Join(os.Getenv("ICONSET_DIR"), e.name), encodePNG(render(icon, e.size)))
	}
}
