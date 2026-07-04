package contract

import (
	"path/filepath"
	"runtime"
)

// FixturesDir resolves contracts/fixtures at the repo root relative to this
// source file (control-plane/internal/contract is three levels below the
// root), so tests and the demo binary work from any working directory.
func FixturesDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "fixtures")
}
