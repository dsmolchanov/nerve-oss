package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/release"
)

const oauthClientCredentialsExtension = "io.modelcontextprotocol/oauth-client-credentials"

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
		// The SDK materializes the complete request body. Reserve the wire cap
		// before dispatch so chunked and concurrent requests cannot evade the
		// shared process budget by growing incrementally.
		release, err := runtime.MemoryBudget.Acquire(request.Context(), maxMCPBodyBytes)
		if err != nil {
			_ = request.Body.Close()
			writeMemoryBudgetError(w)
			return
		}
		defer release()
		request.Body = http.MaxBytesReader(w, request.Body, maxMCPBodyBytes)
		sdkHandler.ServeHTTP(w, request)
	})
}

func newSDKServer(requestContext context.Context, runtime *Server) *sdkmcp.Server {
	capabilities := &sdkmcp.ServerCapabilities{}
	capabilities.AddExtension(oauthClientCredentialsExtension, map[string]any{})
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "nerve-runtime", Version: release.RuntimeVersion}, &sdkmcp.ServerOptions{
		Capabilities: capabilities,
	})
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			result, err := next(ctx, method, request)
			if err == nil {
				setPrivateListCache(result)
			}
			return result, err
		}
	})
	principal, hasPrincipal := auth.PrincipalFromContext(requestContext)
	for _, descriptor := range modernToolCatalog(requestContext, runtime, principal) {
		descriptor := descriptor
		server.AddTool(sdkTool(descriptor), func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
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
				}, nil
			}
			return &sdkmcp.CallToolResult{
				StructuredContent: result,
				Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: rawJSON(result)}},
			}, nil
		})
	}
	if canReadModernResources(runtime, principal) {
		registerSDKResources(server, runtime, principal, hasPrincipal)
	}
	return server
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
