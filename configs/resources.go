package configs

import (
	"embed"
	"io/fs"
	"os"
	"strings"
)

// Files contains runtime defaults; deployment configuration and credentials
// are deliberately excluded.
//
//go:embed policy/*.yaml prompts/*/*.md meters/tool_costs.yaml schemas/*.json
var Files embed.FS

// ReadFile prefers an existing filesystem override. Only canonical bundled
// paths fall back on ENOENT; invalid or unreadable overrides remain errors.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil || !os.IsNotExist(err) {
		return data, err
	}
	if !strings.HasPrefix(path, "configs/") {
		return nil, err
	}
	name := strings.TrimPrefix(path, "configs/")
	if !fs.ValidPath(name) {
		return nil, err
	}
	return Files.ReadFile(name)
}
