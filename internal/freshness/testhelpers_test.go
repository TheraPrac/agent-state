package freshness

import (
	"os"
	"time"
)

// chtime sets a file's mtime to t. Used by cache prune tests to age
// entries deterministically.
func chtime(path string, t time.Time) error {
	return os.Chtimes(path, t, t)
}
