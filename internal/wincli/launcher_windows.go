//go:build windows

package wincli

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

// assets includes a README so ordinary go test/build works before packaging.
// Release builds generate the launcher before embedding the main executable.
//
//go:embed assets
var assets embed.FS

// Ensure also migrates installed copies updated from an older console build.
// It writes only the small stable CLI launcher; the current GUI exe stays the
// single product binary updated by internal/update.
func Ensure(dir string) error {
	data, err := assets.ReadFile("assets/remotai.com")
	if err != nil {
		return fmt.Errorf("CLI launcher not built: %w", err)
	}
	dest := filepath.Join(dir, "remotai.com")
	if current, err := os.ReadFile(dest); err == nil && bytes.Equal(current, data) {
		return nil
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, dest)
}
