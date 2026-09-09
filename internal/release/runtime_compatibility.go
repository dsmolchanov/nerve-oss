package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"neuralmail/internal/startup"
)

// RuntimeCompatibilityReport proves only compiled metadata and executable bytes.
// Image signatures, release-set membership and actual database compatibility are
// separate required admission checks, not facts this offline command can infer.
type RuntimeCompatibilityReport struct {
	SchemaVersion     int               `json:"schema_version"`
	Kind              string            `json:"kind"`
	Executable        string            `json:"executable"`
	ExecutableSHA256  string            `json:"executable_sha256"`
	Manifest          map[string]string `json:"manifest"`
	DatabaseVerified  bool              `json:"database_verified"`
	AdmissionVerified bool              `json:"admission_verified"`
}

var metadataHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var metadataRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
var metadataVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func compiledRuntimeManifest() (map[string]string, error) {
	if !metadataVersion.MatchString(RuntimeVersion) || RuntimeVersion == "dev" || RuntimeVersion == "unknown" ||
		!metadataVersion.MatchString(OutboundPolicyVersion) || OutboundPolicyVersion == "unknown" ||
		!metadataRevision.MatchString(BuildCommit) {
		return nil, errors.New("runtime release identity is not compiled")
	}
	for _, digest := range []string{MCPContractHash, CoreSchemaHash, OutboundPolicySHA256} {
		if !metadataHash.MatchString(digest) {
			return nil, errors.New("runtime source hashes are not compiled")
		}
	}
	built, err := time.Parse("2006-01-02T15:04:05Z", BuildTime)
	if err != nil || built.Format("2006-01-02T15:04:05Z") != BuildTime {
		return nil, errors.New("runtime build time is not canonical UTC")
	}
	return map[string]string{
		"runtime_version":           RuntimeVersion,
		"mcp_contract_hash":         MCPContractHash,
		"core_schema_hash":          CoreSchemaHash,
		"core_schema_min_required":  strconv.FormatInt(startup.CoreMinRequired, 10),
		"core_schema_max_supported": strconv.FormatInt(startup.CoreMaxSupported, 10),
		"outbound_policy_version":   OutboundPolicyVersion,
		"outbound_policy_sha256":    OutboundPolicySHA256,
		"build_commit":              BuildCommit,
		"build_time":                BuildTime,
	}, nil
}

// HandleRuntimeCompatibility runs before config loading or application creation.
// Only the running executable is hashed; callers cannot substitute an input file.
func HandleRuntimeCompatibility(args []string, output io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "compatibility" {
		return false, nil
	}
	if len(args) != 2 || args[1] != "--json" {
		return true, errors.New("usage: nerve-runtime compatibility --json")
	}
	manifest, err := compiledRuntimeManifest()
	if err != nil {
		return true, err
	}
	executable, err := os.Executable()
	if err != nil {
		return true, errors.New("cannot identify runtime executable")
	}
	stream, err := os.Open(executable)
	if err != nil {
		return true, errors.New("cannot read runtime executable")
	}
	digest := sha256.New()
	_, readErr := io.Copy(digest, stream)
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil {
		return true, errors.New("cannot hash runtime executable")
	}
	report := RuntimeCompatibilityReport{
		SchemaVersion: 1, Kind: "nerve-runtime-compatibility", Executable: filepath.Base(executable),
		ExecutableSHA256: hex.EncodeToString(digest.Sum(nil)), Manifest: manifest,
	}
	return true, json.NewEncoder(output).Encode(report)
}
