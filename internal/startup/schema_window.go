package startup

import (
	"fmt"
	"strconv"
	"strings"

	"neuralmail/internal/store"
)

// CompiledSchemaWindow is set only by the release build. Empty preserves the
// historical runtime contract; successor builds encode version:min:max.
var CompiledSchemaWindow = ""

// EffectiveMigrationWindow is shared by startup, offline reports and migrator.
// Environment variables never supply compatibility claims.
func EffectiveMigrationWindow() (store.MigrationWindow, error) {
	window := runtimeMigrationWindow
	if CompiledSchemaWindow == "" {
		return window, nil
	}
	parts := strings.Split(CompiledSchemaWindow, ":")
	if len(parts) != 3 || parts[0] != "2" {
		return store.MigrationWindow{}, fmt.Errorf("invalid compiled runtime schema window")
	}
	values := [2]int64{}
	for i, raw := range parts[1:] {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < CoreMinRequired || n > 9999 || strconv.FormatInt(n, 10) != raw {
			return store.MigrationWindow{}, fmt.Errorf("invalid compiled runtime schema head")
		}
		values[i] = n
	}
	if values[0] > values[1] {
		return store.MigrationWindow{}, fmt.Errorf("inverted compiled runtime schema window")
	}
	window.CoreMinRequired, window.CoreMaxSupported = values[0], values[1]
	return window, nil
}

// CheckEmbeddedMigration keeps successor migration ownership in nerve-migrate.
func CheckEmbeddedMigration() error {
	if _, err := EffectiveMigrationWindow(); err != nil {
		return err
	}
	if CompiledSchemaWindow != "" {
		return fmt.Errorf("successor migrations require nerve-migrate")
	}
	return nil
}
