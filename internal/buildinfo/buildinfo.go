// Package buildinfo contains the identity and release metadata embedded in
// the application.
package buildinfo

const (
	// Name is the human-readable application name.
	Name = "Telegram Video Uploader"

	// Build is the monotonically increasing package/build number.
	Build = 5

	// RepositoryURL points to the source repository for this application.
	RepositoryURL = "https://github.com/Jaaayden/telegram-uploader"
)

// Version is the semantic application version. The default matches
// FyneApp.toml (verified by scripts/check-version.sh and the buildinfo tests);
// release and CI builds override it with
//
//	-ldflags "-X github.com/Jaaayden/telegram-uploader/internal/buildinfo.Version=<version>"
var Version = "1.2.2"
