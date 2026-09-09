package startup

import (
	"context"
	"errors"
	"log"
)

// SchemaQuiescence stops before application construction. It opens no ingress
// listener and initializes no auth, store or provider dependencies. Deployment
// must stop all old writers and provide retryable failure at the external ingress.
func SchemaQuiescence(ctx context.Context, mode string, cloud bool, command, _ string) (bool, error) {
	if mode == "" {
		return false, nil
	}
	if mode != "quiescent" || !cloud {
		return true, errors.New("invalid schema transition mode")
	}
	switch command {
	case "worker", "serve":
		log.Print("schema quiescence active: no listeners")
		<-ctx.Done()
		return true, nil
	default:
		return true, errors.New("command is unavailable during schema quiescence")
	}
}
