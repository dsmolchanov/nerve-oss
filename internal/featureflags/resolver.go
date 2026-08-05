package featureflags

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"neuralmail/internal/store"
)

const DefaultTTL = 30 * time.Second

type Store interface {
	LookupFeatureFlagForOrg(ctx context.Context, orgID string, flag string) (store.FeatureFlagValues, error)
}

type cacheKey struct {
	flag  string
	orgID string
}

type cacheEntry struct {
	enabled   bool
	expiresAt time.Time
}

// Resolver implements the D8 precedence contract. Database failures and
// invalid overrides are deliberately resolved to the compiled default.
type Resolver struct {
	cloudMode bool
	store     Store
	ttl       time.Duration
	defaults  map[string]bool
	now       func() time.Time
	lookupEnv func(string) (string, bool)
	logger    *log.Logger

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

func New(cloudMode bool, flagStore Store) *Resolver {
	return &Resolver{
		cloudMode: cloudMode,
		store:     flagStore,
		ttl:       DefaultTTL,
		defaults:  map[string]bool{"attachments": false},
		now:       time.Now,
		lookupEnv: os.LookupEnv,
		logger:    log.Default(),
		cache:     make(map[cacheKey]cacheEntry),
	}
}

func (r *Resolver) Enabled(ctx context.Context, flag string, orgID string) (bool, error) {
	flag = strings.ToLower(strings.TrimSpace(flag))
	orgID = strings.TrimSpace(orgID)
	compiledDefault := r.defaults[flag]

	if value, present := r.lookupEnv(featureFlagEnvName(flag)); present {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "force-on":
			return true, nil
		case "force-off":
			return false, nil
		default:
			r.logger.Printf("feature flag warning: invalid %s value; resolving %s off", featureFlagEnvName(flag), flag)
			return compiledDefault, nil
		}
	}

	// Self-hosted OSS deliberately has no database dependency for feature
	// flags. Environment overrides and compiled defaults are its full contract.
	if !r.cloudMode {
		return compiledDefault, nil
	}
	if orgID == "" || r.store == nil {
		return compiledDefault, nil
	}

	key := cacheKey{flag: flag, orgID: orgID}
	now := r.now()
	r.mu.Lock()
	entry, cached := r.cache[key]
	if cached && now.Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.enabled, nil
	}
	r.mu.Unlock()

	values, err := r.store.LookupFeatureFlagForOrg(ctx, orgID, flag)
	enabled := compiledDefault
	if err != nil {
		r.logger.Printf("feature flag warning: lookup %s for org %s failed; resolving off: %v", flag, orgID, err)
	} else if values.Org != nil {
		enabled = *values.Org
	} else if values.Global != nil {
		enabled = *values.Global
	}

	r.mu.Lock()
	r.cache[key] = cacheEntry{enabled: enabled, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return enabled, nil
}

func featureFlagEnvName(flag string) string {
	var name strings.Builder
	name.WriteString("NERVE_FLAG_")
	for _, character := range strings.ToUpper(flag) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			name.WriteRune(character)
		} else {
			name.WriteByte('_')
		}
	}
	return name.String()
}
