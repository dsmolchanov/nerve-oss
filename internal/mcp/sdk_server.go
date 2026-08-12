package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/release"
)

const oauthClientCredentialsExtension = "io.modelcontextprotocol/oauth-client-credentials"

const (
	maxModernDecodedAttachmentBytes = 10 << 20
	maxModernRequestMemoryBytes     = 2*maxMCPBodyBytes + maxModernDecodedAttachmentBytes
)

func NewSDKHandler(runtime *Server, jsonResponse bool) http.Handler {
	sdkHandler := sdkmcp.NewStreamableHTTPHandler(func(request *http.Request) *sdkmcp.Server {
		return newSDKServer(request.Context(), runtime)
	}, &sdkmcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 jsonResponse,
		MaxRequestBodyBytes:          maxMCPBodyBytes,
		PropagateRequestCancellation: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.ContentLength > maxMCPBodyBytes {
			_ = request.Body.Close()
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if runtime == nil || runtime.MemoryBudget == nil {
			_ = request.Body.Close()
			writeMemoryBudgetError(w)
			return
		}
		// The SDK may retain two wire copies while tool decoding materializes up
		// to 10 MiB of attachment content. Reserve that worst case before
		// dispatch so chunked and concurrent requests cannot evade the shared
		// process budget by growing incrementally.
		release, err := runtime.MemoryBudget.Acquire(request.Context(), maxModernRequestMemoryBytes)
		if err != nil {
			_ = request.Body.Close()
			writeMemoryBudgetError(w)
			return
		}
		defer release()
		request.Body = http.MaxBytesReader(w, request.Body, maxMCPBodyBytes)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(body))
		if !validateModernRequestContract(w, request, body) {
			return
		}
		sdkHandler.ServeHTTP(w, request)
	})
}

type modernWireRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func validateModernRequestContract(w http.ResponseWriter, request *http.Request, body []byte) bool {
	var wire modernWireRequest
	if err := json.Unmarshal(body, &wire); err != nil || wire.Method == "" {
		// The SDK owns ordinary JSON-RPC parse and shape errors. They cannot
		// dispatch because no method was decoded.
		return true
	}
	if values := request.Header.Values("Mcp-Method"); len(values) != 1 || values[0] != wire.Method {
		writeHeaderMismatch(w, wire.ID, "Mcp-Method header must occur once and match the body method")
		return false
	}
	if requiredName, present, ok := modernRequestName(wire.Method, wire.Params); present {
		values := request.Header.Values("Mcp-Name")
		if !ok || len(values) != 1 || values[0] != requiredName {
			writeHeaderMismatch(w, wire.ID, "Mcp-Name header must occur once and match the body name")
			return false
		}
	}

	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(wire.Params, &params); err != nil || params.Meta == nil {
		writeHeaderMismatch(w, wire.ID, "modern request params._meta is required")
		return false
	}
	var protocolVersion string
	protocolRaw, present := params.Meta[sdkmcp.MetaKeyProtocolVersion]
	if !present || json.Unmarshal(protocolRaw, &protocolVersion) != nil || protocolVersion == "" {
		writeHeaderMismatch(w, wire.ID, "modern request protocolVersion metadata is required")
		return false
	}
	if protocolVersion != ModernProtocolVersion {
		writeProtocolError(w, wire.ID, sdkmcp.CodeUnsupportedProtocolVersion, "unsupported protocol version", sdkmcp.UnsupportedProtocolVersionData{
			Supported: []string{ModernProtocolVersion}, Requested: protocolVersion,
		})
		return false
	}

	capabilitiesRaw, present := params.Meta[sdkmcp.MetaKeyClientCapabilities]
	var capabilities map[string]json.RawMessage
	if !present || !isJSONObject(capabilitiesRaw) || json.Unmarshal(capabilitiesRaw, &capabilities) != nil {
		writeMissingCapabilities(w, wire.ID, false)
		return false
	}
	if clientInfoRaw, present := params.Meta[sdkmcp.MetaKeyClientInfo]; present && !validClientInfo(clientInfoRaw) {
		writeHeaderMismatch(w, wire.ID, "clientInfo metadata is malformed or exceeds configured bounds")
		return false
	}
	principal, _ := auth.PrincipalFromContext(request.Context())
	if principal.Kind == auth.PrincipalM2MOnboarding || principal.Kind == auth.PrincipalM2MOrg {
		if !hasOAuthClientCredentialsCapability(capabilities) {
			writeMissingCapabilities(w, wire.ID, true)
			return false
		}
	}
	return true
}

func modernRequestName(method string, params json.RawMessage) (string, bool, bool) {
	var field string
	switch method {
	case "tools/call", "prompts/get":
		field = "name"
	case "resources/read":
		field = "uri"
	default:
		return "", false, true
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(params, &values) != nil {
		return "", true, false
	}
	var name string
	if json.Unmarshal(values[field], &name) != nil || name == "" {
		return "", true, false
	}
	return name, true, true
}

func hasOAuthClientCredentialsCapability(capabilities map[string]json.RawMessage) bool {
	extensionsRaw, ok := capabilities["extensions"]
	if !ok || !isJSONObject(extensionsRaw) {
		return false
	}
	var extensions map[string]json.RawMessage
	if json.Unmarshal(extensionsRaw, &extensions) != nil {
		return false
	}
	settings, ok := extensions[oauthClientCredentialsExtension]
	if !ok || !isJSONObject(settings) {
		return false
	}
	var settingsObject map[string]json.RawMessage
	return json.Unmarshal(settings, &settingsObject) == nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func validClientInfo(raw json.RawMessage) bool {
	if !isJSONObject(raw) {
		return false
	}
	var implementation sdkmcp.Implementation
	if json.Unmarshal(raw, &implementation) != nil {
		return false
	}
	return boundedRequiredString(implementation.Name, 128) && boundedRequiredString(implementation.Version, 128) &&
		len(implementation.Title) <= 256 && len(implementation.Description) <= 1_024 && len(implementation.WebsiteURL) <= 2_048
}

func boundedRequiredString(value string, maximum int) bool {
	return value != "" && len(value) <= maximum
}

func writeMissingCapabilities(w http.ResponseWriter, id any, oauthExtension bool) {
	required := &sdkmcp.ClientCapabilities{}
	if oauthExtension {
		required.AddExtension(oauthClientCredentialsExtension, map[string]any{})
	}
	writeProtocolError(w, id, sdkmcp.CodeMissingRequiredClientCapabilities, "missing required client capabilities", sdkmcp.MissingRequiredClientCapabilityData{
		RequiredCapabilities: required,
	})
}

func newSDKServer(requestContext context.Context, runtime *Server) *sdkmcp.Server {
	capabilities := &sdkmcp.ServerCapabilities{}
	capabilities.AddExtension(oauthClientCredentialsExtension, map[string]any{})
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "nerve-runtime", Version: release.RuntimeVersion}, &sdkmcp.ServerOptions{
		Capabilities: capabilities,
	})
	principal, hasPrincipal := auth.PrincipalFromContext(requestContext)
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			result, err := next(ctx, method, request)
			if err == nil {
				filterModernToolList(result, modernToolCatalog(requestContext, runtime, principal))
				setPrivateListCache(result)
			}
			return result, err
		}
	})
	for _, descriptor := range modernToolDescriptors(requestContext, runtime, principal) {
		descriptor := descriptor
		sdkmcp.AddTool[map[string]any, any](server, sdkTool(descriptor), func(ctx context.Context, request *sdkmcp.CallToolRequest, _ map[string]any) (*sdkmcp.CallToolResult, any, error) {
			if hasPrincipal {
				ctx = auth.WithPrincipal(ctx, principal)
			}
			result, err := runtime.Invoker.Invoke(ctx, ToolInvocation{
				Name: request.Params.Name, Arguments: request.Params.Arguments,
			})
			if err != nil {
				translated := translateModernBusinessError(err)
				structured := map[string]any{"error": translated}
				return &sdkmcp.CallToolResult{
					IsError: true, StructuredContent: structured,
					Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: rawJSON(structured)}},
				}, nil, nil
			}
			return nil, result, nil
		})
	}
	if canReadModernResources(runtime, principal) {
		registerSDKResources(server, runtime, principal, hasPrincipal)
	}
	return server
}

func filterModernToolList(result sdkmcp.Result, visible []toolDescriptor) {
	listed, ok := result.(*sdkmcp.ListToolsResult)
	if !ok {
		return
	}
	allowed := make(map[string]struct{}, len(visible))
	for _, descriptor := range visible {
		allowed[descriptor.Name] = struct{}{}
	}
	filtered := listed.Tools[:0]
	for _, tool := range listed.Tools {
		if _, ok := allowed[tool.Name]; ok {
			filtered = append(filtered, tool)
		}
	}
	listed.Tools = filtered
}

func canReadModernResources(runtime *Server, principal auth.Principal) bool {
	if principal.Kind == auth.PrincipalM2MOnboarding {
		return false
	}
	if !runtime.Config.Cloud.Mode {
		return true
	}
	return runtime.Auth != nil && runtime.Auth.ValidateScopes(principal, "nerve:email.read") == nil
}

func setPrivateListCache(result sdkmcp.Result) {
	cache := sdkmcp.Cacheable{TTLMs: 5_000, CacheScope: "private"}
	switch listed := result.(type) {
	case *sdkmcp.ListToolsResult:
		listed.Cacheable = cache
	case *sdkmcp.ListResourcesResult:
		listed.Cacheable = cache
	case *sdkmcp.ListResourceTemplatesResult:
		listed.Cacheable = cache
	}
}

func registerSDKResources(server *sdkmcp.Server, runtime *Server, principal auth.Principal, hasPrincipal bool) {
	handler := func(ctx context.Context, request *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		if hasPrincipal {
			ctx = auth.WithPrincipal(ctx, principal)
		}
		if runtime.Config.Cloud.Mode {
			if runtime.Auth == nil {
				return nil, errors.New("cloud auth not configured")
			}
			if err := runtime.Auth.ValidateScopes(principal, "nerve:email.read"); err != nil {
				return nil, err
			}
		}
		params, _ := json.Marshal(ResourceReadParams{URI: request.Params.URI})
		result, err := runtime.readResource(ctx, Request{Method: "resources/read", Params: params})
		if err != nil {
			if isResourceNotFound(err) {
				return nil, sdkmcp.ResourceNotFoundError(request.Params.URI)
			}
			return nil, err
		}
		return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{
			URI: request.Params.URI, MIMEType: "application/json", Text: rawJSON(result),
		}}}, nil
	}
	server.AddResource(&sdkmcp.Resource{
		URI: "email://inboxes", Name: "inboxes", Description: "List inbox IDs", MIMEType: "application/json",
	}, handler)
	server.AddResourceTemplate(&sdkmcp.ResourceTemplate{
		URITemplate: "email://threads/{thread_id}", Name: "thread", Description: "Fetch one email thread", MIMEType: "application/json",
	}, handler)
	server.AddResourceTemplate(&sdkmcp.ResourceTemplate{
		URITemplate: "email://messages/{message_id}", Name: "message", Description: "Fetch one email message", MIMEType: "application/json",
	}, handler)
}

func isResourceNotFound(err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.HasPrefix(message, "resource not found:") ||
		strings.Contains(message, "does not belong to org")
}
