package freshness

import "os"

// osStatImpl wraps os.Stat in a private indirection so the
// public Check API stays free of os imports and tests can swap
// statter via CheckOpts.Statter.
func osStatImpl(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
