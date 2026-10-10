// Package config loads process configuration from the environment and
// builds shared dependencies (store, auth, object storage, execution pools).
// Production mode refuses insecure development shortcuts (AUTH-001,
// SEC-012): static tokens, the unisolated local provider, filesystem
// artifact storage and disabled TLS verification.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deepakppatil/astrion/internal/auth"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/objectstore"
	"github.com/deepakppatil/astrion/internal/provider"
	"github.com/deepakppatil/astrion/internal/provider/kubernetes"
	"github.com/deepakppatil/astrion/internal/provider/local"
)

type Config struct {
	Production        bool
	DatabaseURL       string
	HTTPAddr          string
	TemporalAddress   string
	TemporalNamespace string
	TenantID          string
	SeedDemo          bool
	DemoRepoURL       string
	WebDir            string
	MaxArtifactBytes  int64
	// Execution environment settings (worker).
	ExecAPIURL        string
	SupervisorCommand []string
	DefaultImage      string
	PoolsFile         string
	DataDir           string
	LogLevel          slog.Level
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func Load() (*Config, error) {
	c := &Config{
		Production:        env("ASTRION_ENV", "development") == "production",
		DatabaseURL:       env("ASTRION_DATABASE_URL", ""),
		HTTPAddr:          env("ASTRION_HTTP_ADDR", ":8080"),
		TemporalAddress:   env("ASTRION_TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		TemporalNamespace: env("ASTRION_TEMPORAL_NAMESPACE", "default"),
		TenantID:          env("ASTRION_TENANT_ID", "ten_default"),
		DemoRepoURL:       env("ASTRION_DEMO_REPO_URL", ""),
		WebDir:            env("ASTRION_WEB_DIR", ""),
		ExecAPIURL:        env("ASTRION_EXEC_API_URL", "http://127.0.0.1:8080"),
		DefaultImage:      env("ASTRION_DEFAULT_IMAGE", "ghcr.io/deepakppatil/astrion-runtime:dev"),
		PoolsFile:         env("ASTRION_POOLS_FILE", ""),
		DataDir:           env("ASTRION_DATA_DIR", ".astrion"),
	}
	c.SeedDemo = env("ASTRION_SEED_DEMO", strconv.FormatBool(!c.Production)) == "true"
	if c.DatabaseURL == "" {
		return nil, errors.New("ASTRION_DATABASE_URL is required")
	}
	n, err := strconv.ParseInt(env("ASTRION_MAX_ARTIFACT_BYTES", "52428800"), 10, 64)
	if err != nil || n <= 0 {
		return nil, errors.New("ASTRION_MAX_ARTIFACT_BYTES must be a positive integer")
	}
	c.MaxArtifactBytes = n
	if cmd := env("ASTRION_SUPERVISOR_COMMAND", ""); cmd != "" {
		// A path to an existing file is used verbatim so that paths with
		// spaces (common on Windows and macOS) work; otherwise the value is a
		// whitespace-separated command line.
		if st, err := os.Stat(cmd); err == nil && !st.IsDir() {
			c.SupervisorCommand = []string{cmd}
		} else {
			c.SupervisorCommand = strings.Fields(cmd)
		}
	} else {
		c.SupervisorCommand = []string{"astrion-supervisor"}
	}
	switch strings.ToLower(env("ASTRION_LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		c.LogLevel = slog.LevelInfo
	}
	if c.Production && c.SeedDemo {
		return nil, errors.New("ASTRION_SEED_DEMO must not be enabled in production")
	}
	return c, nil
}

func (c *Config) Logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: c.LogLevel}))
}

// Authenticator builds the user authenticator (OIDC, or dev tokens outside
// production).
func (c *Config) Authenticator() (auth.Authenticator, error) {
	switch mode := env("ASTRION_AUTH_MODE", map[bool]string{true: "oidc", false: "dev"}[c.Production]); mode {
	case "oidc":
		cfg := auth.OIDCConfig{Issuer: env("ASTRION_OIDC_ISSUER", ""), Audience: env("ASTRION_OIDC_AUDIENCE", ""),
			JWKSURL: env("ASTRION_OIDC_JWKS_URL", ""), TenantClaim: env("ASTRION_OIDC_TENANT_CLAIM", ""),
			DefaultTenant: c.TenantID, GroupsClaim: env("ASTRION_OIDC_GROUPS_CLAIM", "groups"), Leeway: time.Minute}
		if m := env("ASTRION_OIDC_GROUP_ROLES", ""); m != "" {
			// JSON object: {"idp-group": ["developer", "qa_engineer"]}
			if err := json.Unmarshal([]byte(m), &cfg.GroupRoles); err != nil {
				return nil, fmt.Errorf("ASTRION_OIDC_GROUP_ROLES: %w", err)
			}
			for g, roles := range cfg.GroupRoles {
				for _, r := range roles {
					if !r.Valid() {
						return nil, fmt.Errorf("ASTRION_OIDC_GROUP_ROLES: unknown role %q for group %q", r, g)
					}
				}
			}
		}
		return auth.NewOIDC(cfg)
	case "dev":
		spec := env("ASTRION_DEV_USERS", "dev-admin-token:admin:platform_admin;dev-developer-token:dana:developer|qa_engineer;"+
			"dev-analyst-token:bea:business_analyst;dev-architect-token:arch:architect;dev-release-token:rel:release_engineer")
		users, err := auth.ParseDevUsers(spec)
		if err != nil {
			return nil, err
		}
		return auth.NewDev(c.Production, c.TenantID, users)
	default:
		return nil, fmt.Errorf("unknown ASTRION_AUTH_MODE %q", mode)
	}
}

// ObjectStore builds artifact storage: S3-compatible, or the local
// filesystem outside production.
func (c *Config) ObjectStore() (objectstore.Store, error) {
	switch kind := env("ASTRION_OBJECT_STORE", map[bool]string{true: "s3", false: "fs"}[c.Production]); kind {
	case "s3":
		bucket := env("ASTRION_S3_BUCKET", "")
		if bucket == "" {
			return nil, errors.New("ASTRION_S3_BUCKET is required for the s3 object store")
		}
		return objectstore.NewS3FromEnv(env("ASTRION_S3_ENDPOINT", "https://s3.amazonaws.com"), env("ASTRION_S3_REGION", "us-east-1"), bucket), nil
	case "fs":
		if c.Production {
			return nil, errors.New("filesystem artifact storage is not permitted in production")
		}
		return objectstore.NewFS(filepath.Join(c.DataDir, "artifacts"))
	default:
		return nil, fmt.Errorf("unknown ASTRION_OBJECT_STORE %q", kind)
	}
}

// PoolConfig is one entry of the execution pools file.
type PoolConfig struct {
	Name              string                    `json:"name"`
	Type              string                    `json:"type"` // kubernetes | local
	Network           string                    `json:"network"`
	Region            string                    `json:"region"`
	MaxClassification domain.DataClassification `json:"maxClassification"`
	Tools             []string                  `json:"tools"`
	CostWeight        int                       `json:"costWeight"`
	Enabled           *bool                     `json:"enabled"`
	Kubernetes        *kubernetes.Config        `json:"kubernetes,omitempty"`
	Local             *struct {
		BaseDir       string `json:"baseDir"`
		MaxConcurrent int    `json:"maxConcurrent"`
	} `json:"local,omitempty"`
}

// Pools builds the execution pool registry. Without a pools file, a single
// local pool is configured in development.
func (c *Config) Pools() (*provider.Registry, error) {
	var cfgs []PoolConfig
	if c.PoolsFile != "" {
		b, err := os.ReadFile(c.PoolsFile)
		if err != nil {
			return nil, fmt.Errorf("read pools file: %w", err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfgs); err != nil {
			return nil, fmt.Errorf("parse pools file: %w", err)
		}
	} else if !c.Production {
		cfgs = []PoolConfig{{Name: "local", Type: "local", Network: "PRIVATE", MaxClassification: domain.ClassRestricted}}
	}
	if len(cfgs) == 0 {
		return nil, errors.New("no execution pools configured (set ASTRION_POOLS_FILE)")
	}
	reg := provider.NewRegistry()
	for _, pc := range cfgs {
		if pc.Name == "" || pc.Name == "AUTO" {
			return nil, fmt.Errorf("pool name %q is invalid", pc.Name)
		}
		switch pc.Network {
		case "PRIVATE", "PUBLIC":
		default:
			return nil, fmt.Errorf("pool %s: network must be PRIVATE or PUBLIC", pc.Name)
		}
		if pc.MaxClassification == "" {
			pc.MaxClassification = domain.ClassInternal
		}
		pool := &provider.Pool{Name: pc.Name, Type: pc.Type, Network: pc.Network, Region: pc.Region,
			MaxClassification: pc.MaxClassification, Tools: pc.Tools, CostWeight: pc.CostWeight, Enabled: pc.Enabled == nil || *pc.Enabled}
		switch pc.Type {
		case "local":
			if c.Production {
				return nil, fmt.Errorf("pool %s: the local provider offers no isolation and is refused in production", pc.Name)
			}
			dir := filepath.Join(c.DataDir, "envs", pc.Name)
			if pc.Local != nil && pc.Local.BaseDir != "" {
				dir = pc.Local.BaseDir
			}
			p, err := local.New(pc.Name, dir)
			if err != nil {
				return nil, err
			}
			if pc.Local != nil && pc.Local.MaxConcurrent > 0 {
				p.MaxConcurrent = pc.Local.MaxConcurrent
			}
			pool.Provider = p
		case "kubernetes":
			if pc.Kubernetes == nil {
				return nil, fmt.Errorf("pool %s: kubernetes settings required", pc.Name)
			}
			if c.Production && pc.Kubernetes.InsecureSkipVerify {
				return nil, fmt.Errorf("pool %s: insecureSkipVerify is refused in production", pc.Name)
			}
			kc := *pc.Kubernetes
			kc.Name = pc.Name
			p, err := kubernetes.New(kc)
			if err != nil {
				return nil, fmt.Errorf("pool %s: %w", pc.Name, err)
			}
			pool.Provider = p
		default:
			return nil, fmt.Errorf("pool %s: unknown type %q", pc.Name, pc.Type)
		}
		reg.Add(pool)
	}
	return reg, nil
}
