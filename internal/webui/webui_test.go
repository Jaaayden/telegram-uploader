package webui

import (
	"strings"
	"testing"
)

func TestPageIsSelfContained(t *testing.T) {
	page := Page()
	for _, external := range []string{`href="app.css"`, `src="app.js"`, "http://", "https://"} {
		if strings.Contains(page, external) {
			t.Errorf("page still references %q; it must not load anything", external)
		}
	}
	if !strings.Contains(page, "<style>") || !strings.Contains(page, "window.__tvuReceive") {
		t.Fatal("stylesheet or script was not inlined")
	}
	// An inlined script ends at the first closing tag, wherever it appears.
	// Two scripts: the small error reporter and the application.
	if strings.Count(page, "</script>") != 2 {
		t.Fatalf("page contains %d closing script tags, want exactly 2", strings.Count(page, "</script>"))
	}
	// The web view loads the page from a string limited to about 2 MB on
	// Windows; stay far below that.
	if len(page) > 256<<10 {
		t.Fatalf("page is %d bytes; keep it small", len(page))
	}
}
