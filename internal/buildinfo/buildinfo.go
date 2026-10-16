// Package buildinfo carries values injected at link time.
package buildinfo

// Version is the application version. Release builds set it with
//
//	-ldflags "-X github.com/jayden/telegram-video-uploader/internal/buildinfo.Version=1.2.0"
//
// A plain `go build` reports "dev".
var Version = "dev"
