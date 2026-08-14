package web

import (
	"io/fs"
	"testing"
)

func TestFS_ReturnsEmbeddedFilesystem(t *testing.T) {
	f, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	// Either the placeholder or a real build output should provide
	// index.html — that's the bundle entrypoint regardless of which
	// is currently in dist/.
	info, err := fs.Stat(f, "index.html")
	if err != nil {
		t.Fatalf("index.html not present in embedded FS: %v", err)
	}
	if info.IsDir() {
		t.Errorf("index.html is a directory; expected a file")
	}
	if info.Size() == 0 {
		t.Errorf("index.html is empty; expected at least placeholder content")
	}
}
