//go:build !windows && !(darwin && cgo)

package shell

import (
	"fmt"
	"os"
)

// Run is unavailable without a native web view (Linux, or macOS built
// without cgo). The headless packages still build and test everywhere.
func Run(Options) error { return ErrUnsupported }

// PickFolder is unavailable on this platform.
func PickFolder(string) (string, error) { return "", ErrUnsupported }

// ShowError prints the message; there is no window system to show it in.
func ShowError(title, message string) { fmt.Fprintln(os.Stderr, title+": "+message) }
