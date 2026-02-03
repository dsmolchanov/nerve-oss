package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	HTTP struct {
		Addr string `yaml:"addr"`
	} `yaml:"http"`
	Dev struct {
		Mode bool `yaml:"mode"`
	} `yaml:"dev"`
	JMAP struct {
		URL          string        `yaml:"url"`
		SessionURL   string        `yaml:"session_url"`
		AccountID    string        `yaml:"account_id"`
		Username     string        `yaml:"username"`
		Password     string        `yaml:"password"`
		PushSecret   string        `yaml:"push_secret"`
		PollInterval time.Duration `yaml:"poll_interval"`
	} `yaml:"jmap"`
	SMTP struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
		From     string `yaml:"from"`
	} `yaml:"smtp"`
	Database struct {
		DSN string `yaml:"dsn"`
	} `yaml:"database"`
	Qdrant struct {
		URL        string `yaml:"url"`
		Collection string `yaml:"collection"`
		EmbedDim   int    `yaml:"embed_dim"`
	} `yaml:"qdrant"`
	Redis struct {
		URL string `yaml:"url"`
	} `yaml:"redis"`
	ObjectStore struct {
		URL       string `yaml:"url"`
		Bucket    string `yaml:"bucket"`
		AccessKey string `yaml:"access_key"`
		SecretKey string `yaml:"secret_key"`
	} `yaml:"object_store"`
	Embedding struct {
		Provider string `yaml:"provider"`
		Model    string `yaml:"model"`
		Dim      int    `yaml:"dim"`
	} `yaml:"embedding"`
	LLM struct {
		Provider   string `yaml:"provider"`
		Model      string `yaml:"model"`
		OpenAIKey  string `yaml:"openai_key"`
		OllamaURL  string `yaml:"ollama_url"`
		PromptPath string `yaml:"prompt_path"`
	} `yaml:"llm"`
	Policy struct {
		DefaultPath string `yaml:"default_path"`
	} `yaml:"policy"`
	MCP struct {
		ProtocolVersion string   `yaml:"protocol_version"`
		AllowOrigins    []string `yaml:"allow_origins"`
	} `yaml:"mcp"`
	Security struct {
		APIKey                  string `yaml:"api_key"`
		AllowOutbound           bool   `yaml:"allow_outbound"`
		AllowSendWithWarnings   bool   `yaml:"allow_send_with_warnings"`
		OutboundDomainAllowlist []string `yaml:"outbound_domain_allowlist"`
	} `yaml:"security"`
	Log struct {
		Level string `yaml:"level"`
	} `yaml:"log"`
}

func Default() Config {
	var cfg Config
	cfg.HTTP.Addr = ":8088"
	cfg.Dev.Mode = true
	cfg.JMAP.PollInterval = 30 * time.Second
	cfg.SMTP.Host = "localhost"
	cfg.SMTP.Port = 2525
	cfg.SMTP.From = "dev@local.neuralmail"
	cfg.Qdrant.Collection = "messages_v1536"
	cfg.Qdrant.EmbedDim = 1536
	cfg.Embedding.Provider = "noop"
	cfg.Embedding.Dim = 1536
	cfg.LLM.Provider = "noop"
	cfg.LLM.PromptPath = "configs/prompts/v1"
	cfg.Policy.DefaultPath = "configs/policy/support-default-v1.yaml"
	cfg.MCP.ProtocolVersion = "2025-11-25"
	cfg.Log.Level = "info"
	return cfg
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, err
			}
		} else {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, err
			}
		}
	}

	applyEnv(&cfg)

	if cfg.JMAP.URL == "" {
		return cfg, errors.New("missing jmap.url (or NM_JMAP_URL)")
	}

	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("NM_HTTP_ADDR"); v != "" {
		cfg.HTTP.Addr = v
	}
	if v := os.Getenv("NM_DEV_MODE"); v != "" {
		cfg.Dev.Mode = parseBool(v, cfg.Dev.Mode)
	}
	if v := os.Getenv("NM_JMAP_URL"); v != "" {
		cfg.JMAP.URL = v
	}
	if v := os.Getenv("NM_JMAP_SESSION_URL"); v != "" {
		cfg.JMAP.SessionURL = v
	}
	if v := os.Getenv("NM_JMAP_ACCOUNT_ID"); v != "" {
		cfg.JMAP.AccountID = v
	}
	if v := os.Getenv("NM_JMAP_USERNAME"); v != "" {
		cfg.JMAP.Username = v
	}
	if v := os.Getenv("NM_JMAP_PASSWORD"); v != "" {
		cfg.JMAP.Password = v
	}
	if v := os.Getenv("NM_JMAP_PUSH_SECRET"); v != "" {
		cfg.JMAP.PushSecret = v
	}
	if v := os.Getenv("NM_JMAP_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.JMAP.PollInterval = d
		}
	}
	if v := os.Getenv("NM_SMTP_HOST"); v != "" {
		cfg.SMTP.Host = v
	}
	if v := os.Getenv("NM_SMTP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.SMTP.Port = p
		}
	}
	if v := os.Getenv("NM_SMTP_USERNAME"); v != "" {
		cfg.SMTP.Username = v
	}
	if v := os.Getenv("NM_SMTP_PASSWORD"); v != "" {
		cfg.SMTP.Password = v
	}
	if v := os.Getenv("NM_SMTP_FROM"); v != "" {
		cfg.SMTP.From = v
	}
	if v := os.Getenv("NM_DB_DSN"); v != "" {
		cfg.Database.DSN = v
	}
	if v := os.Getenv("NM_QDRANT_URL"); v != "" {
		cfg.Qdrant.URL = v
	}
	if v := os.Getenv("NM_QDRANT_COLLECTION"); v != "" {
		cfg.Qdrant.Collection = v
	}
	if v := os.Getenv("NM_EMBED_DIM"); v != "" {
		if dim, err := strconv.Atoi(v); err == nil {
			cfg.Qdrant.EmbedDim = dim
			cfg.Embedding.Dim = dim
		}
	}
	if v := os.Getenv("NM_REDIS_URL"); v != "" {
		cfg.Redis.URL = v
	}
	if v := os.Getenv("NM_OBJECT_STORE_URL"); v != "" {
		cfg.ObjectStore.URL = v
	}
	if v := os.Getenv("NM_OBJECT_STORE_BUCKET"); v != "" {
		cfg.ObjectStore.Bucket = v
	}
	if v := os.Getenv("NM_OBJECT_STORE_ACCESS_KEY"); v != "" {
		cfg.ObjectStore.AccessKey = v
	}
	if v := os.Getenv("NM_OBJECT_STORE_SECRET_KEY"); v != "" {
		cfg.ObjectStore.SecretKey = v
	}
	if v := os.Getenv("NM_EMBED_PROVIDER"); v != "" {
		cfg.Embedding.Provider = v
	}
	if v := os.Getenv("NM_EMBED_MODEL"); v != "" {
		cfg.Embedding.Model = v
	}
	if v := os.Getenv("NM_LLM_PROVIDER"); v != "" {
		cfg.LLM.Provider = v
	}
	if v := os.Getenv("NM_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("NM_OPENAI_API_KEY"); v != "" {
		cfg.LLM.OpenAIKey = v
	}
	if v := os.Getenv("NM_OLLAMA_URL"); v != "" {
		cfg.LLM.OllamaURL = v
	}
	if v := os.Getenv("NM_LLM_PROMPT_PATH"); v != "" {
		cfg.LLM.PromptPath = v
	}
	if v := os.Getenv("NM_POLICY_PATH"); v != "" {
		cfg.Policy.DefaultPath = v
	}
	if v := os.Getenv("NM_MCP_PROTOCOL_VERSION"); v != "" {
		cfg.MCP.ProtocolVersion = v
	}
	if v := os.Getenv("NM_MCP_ALLOW_ORIGINS"); v != "" {
		cfg.MCP.AllowOrigins = splitCSV(v)
	}
	if v := os.Getenv("NM_API_KEY"); v != "" {
		cfg.Security.APIKey = v
	}
	if v := os.Getenv("NM_ALLOW_OUTBOUND"); v != "" {
		cfg.Security.AllowOutbound = parseBool(v, cfg.Security.AllowOutbound)
	}
	if v := os.Getenv("NM_ALLOW_SEND_WITH_WARNINGS"); v != "" {
		cfg.Security.AllowSendWithWarnings = parseBool(v, cfg.Security.AllowSendWithWarnings)
	}
	if v := os.Getenv("NM_OUTBOUND_DOMAIN_ALLOWLIST"); v != "" {
		cfg.Security.OutboundDomainAllowlist = splitCSV(v)
	}
	if v := os.Getenv("NM_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
}

func parseBool(input string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func splitCSV(input string) []string {
	parts := strings.Split(input, ",")
	var out []string
	for _, part := range parts {
		val := strings.TrimSpace(part)
		if val == "" {
			continue
		}
		out = append(out, val)
	}
	return out
}
