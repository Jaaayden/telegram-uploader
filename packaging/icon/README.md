# 应用图标

`icon.svg` 是图标的唯一源文件：蓝色圆角方块上的白色播放三角代表视频，下方的进度条代表按顺序逐个上传。其余文件都由它生成：

| 文件 | 用途 |
| --- | --- |
| `../../internal/ui/icon.png`（512 px） | 窗口 / 任务栏 / Dock 图标，编译进程序 |
| `../macos/AppIcon.icns` | macOS 应用包图标（`Info.plist` 的 `CFBundleIconFile`） |
| `../windows/icon.ico` | Windows 图标源文件 |
| `../../cmd/tg-video-uploader/rsrc_windows_amd64.syso` | 由 `icon.ico` 生成的 Windows 资源，`go build` 会自动链接进 exe，资源管理器里显示该图标 |
| `icon-256.png` | README 展示用 |

## 重新生成

修改 `icon.svg` 后，在仓库根目录执行（需要 macOS 的 `iconutil`）：

```bash
mkdir -p /tmp/AppIcon.iconset
ICONSET_DIR=/tmp/AppIcon.iconset go run packaging/icon/gen.go
iconutil -c icns /tmp/AppIcon.iconset -o packaging/macos/AppIcon.icns
go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -ico packaging/windows/icon.ico -o cmd/tg-video-uploader/rsrc_windows_amd64.syso
```

`gen.go` 带有 `//go:build ignore`，不会参与 `go build ./...`；SVG 里请只用路径、矩形和渐变，不要用描边或滤镜（用到的 SVG 光栅化库对它们的缩放支持不完整，小尺寸会失真）。
