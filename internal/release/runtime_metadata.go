package release

import "log"

var (
	RuntimeVersion  = "dev"
	MCPContractHash = "unknown"
	CoreSchemaHash  = "unknown"
	BuildCommit     = "unknown"
	BuildTime       = "unknown"
)

func LogRuntimeBanner(logger *log.Logger, cloudMode bool) {
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(
		"runtime_startup runtime_version=%s core_schema_hash=%s mcp_contract_hash=%s build_commit=%s build_time=%s cloud_mode=%t",
		RuntimeVersion,
		CoreSchemaHash,
		MCPContractHash,
		BuildCommit,
		BuildTime,
		cloudMode,
	)
}
