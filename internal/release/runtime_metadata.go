package release

import "log"

var (
	RuntimeVersion        = "dev"
	MCPContractHash       = "unknown"
	CoreSchemaHash        = "unknown"
	OutboundPolicyVersion = "unknown"
	OutboundPolicySHA256  = "unknown"
	BuildCommit           = "unknown"
	BuildTime             = "unknown"
)

func LogRuntimeBanner(logger *log.Logger, cloudMode bool) {
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(
		"runtime_startup runtime_version=%s core_schema_hash=%s mcp_contract_hash=%s outbound_policy_version=%s outbound_policy_sha256=%s build_commit=%s build_time=%s cloud_mode=%t",
		RuntimeVersion,
		CoreSchemaHash,
		MCPContractHash,
		OutboundPolicyVersion,
		OutboundPolicySHA256,
		BuildCommit,
		BuildTime,
		cloudMode,
	)
}
