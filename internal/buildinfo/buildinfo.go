// Package buildinfo contains the identity and release metadata embedded in
// the application.
package buildinfo

const (
	// Name is the human-readable application name.
	Name = "Telegram Video Uploader"

	// Repository is the GitHub "owner/name" slug releases are published to.
	Repository = "Jaaayden/telegram-uploader"

	// RepositoryURL points to the source repository for this application.
	RepositoryURL = "https://github.com/" + Repository
)

// Version is the semantic application version. The default matches the VERSION
// file at the repository root (verified by the buildinfo tests); release and CI
// builds override it with
//
//	-ldflags "-X github.com/Jaaayden/telegram-uploader/internal/buildinfo.Version=<version>"
var Version = "2.3.0"

// Commit is the source revision of a release or CI build. It is empty for
// plain "go build" and "go run".
var Commit = ""
