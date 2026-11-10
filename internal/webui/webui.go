// Package webui contains the interface: one HTML document, one stylesheet and
// one script, with no build step and no third-party code. Page assembles them
// into a single self-contained document so the web view needs no file or
// network access.
package webui

import (
	_ "embed"
	"strings"
	"sync"
)

var (
	//go:embed index.html
	indexHTML string
	//go:embed app.css
	appCSS string
	//go:embed app.js
	appJS string
)

// Page returns the complete document with the stylesheet and script inlined.
var Page = sync.OnceValue(func() string {
	page := strings.Replace(indexHTML, `<link rel="stylesheet" href="app.css">`, "<style>\n"+appCSS+"</style>", 1)
	return strings.Replace(page, `<script src="app.js"></script>`, "<script>\n"+appJS+"</script>", 1)
})
