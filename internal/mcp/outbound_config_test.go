package mcp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"neuralmail/internal/tools"
)

func TestLegacySetupErrorHasLocalRemediation(t *testing.T) {
	for _, code := range []string{"outbound_disabled", "smtp_unconfigured", "recipient_domain_not_allowed", "outbound_transport_unconfigured"} {
		t.Run(code, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server := &Server{}
			server.writeDispatchError(recorder, 1, &tools.OutboundConfigurationError{Code: code, Remediation: "docs/SELF_HOSTING.md#outbound-configuration"})
			var response struct {
				Error struct {
					Message string `json:"message"`
					Data    struct {
						Code        string `json:"code"`
						Retryable   bool   `json:"retryable"`
						Remediation string `json:"remediation"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Message != code || response.Error.Data.Code != code || response.Error.Data.Retryable || response.Error.Data.Remediation == "" {
				t.Fatal(recorder.Body.String())
			}
		})
	}
}
