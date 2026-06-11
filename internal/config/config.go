// Package config handles environment-first configuration loading for the marketplace service.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

// schemaNameRe validates that a Postgres schema name only contains safe characters
// to prevent SQL injection when the name is interpolated into CREATE SCHEMA.
// First character must be a letter or underscore (leading digits are invalid PG identifiers).
var schemaNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Config holds all configuration for the marketplace service.
type Config struct {
	// Server
	Port int `mapstructure:"port"`

	// Postgres
	PostgresDSN string `mapstructure:"postgres_dsn"`

	// PostgresSchema is the optional Postgres schema to use (default: "" = public).
	// Set to "marketplace" when sharing one Aiven database across multiple services
	// so each service is isolated by schema rather than by database.
	// Only alphanumeric characters and underscores are allowed ([a-zA-Z0-9_]+).
	PostgresSchema string `mapstructure:"postgres_schema"`

	// DBMaxConns is the maximum number of connections in the pgxpool (default: 10).
	// Lower this when running multiple services against a small Aiven plan.
	// Env: MARKETPLACE_DB_MAX_CONNS
	DBMaxConns int `mapstructure:"db_max_conns"`

	// DBMinConns is the minimum number of idle connections in the pgxpool (default: 2).
	// Env: MARKETPLACE_DB_MIN_CONNS
	DBMinConns int `mapstructure:"db_min_conns"`

	// Redis (optional — nil Redis = event publish no-op + in-process rate limiter)
	RedisURL string `mapstructure:"redis_url"`

	// Log level: DEBUG, INFO, WARN, ERROR
	LogLevel string `mapstructure:"log_level"`

	// Environment: development | staging | production. REQUIRED — there is no
	// safe default. An empty or unknown value is a boot error (fail-closed): a
	// silent default to "production" would mask a misconfigured deploy, and a
	// silent default to "development" would disable gateway-signature
	// verification §24.1 (the forged-identity-header hole). Operators MUST set
	// MARKETPLACE_ENV explicitly.
	Env string `mapstructure:"env"`

	// WorkspaceBaseURL is the base URL of the workspace service, used for the
	// server-to-server contract-create call after AcceptBid.
	// Example: "http://workspace:8082"
	// Optional: if empty, the workspace call is skipped (local dev without workspace).
	// Env: MARKETPLACE_WORKSPACE_BASE_URL
	WorkspaceBaseURL string `mapstructure:"workspace_base_url"`

	// WorkspaceServiceToken is the shared secret sent in X-Service-Token when
	// calling workspace's internal endpoint. Required when WorkspaceBaseURL is set.
	// Must be at least 32 characters.
	// Env: MARKETPLACE_WORKSPACE_SERVICE_TOKEN
	WorkspaceServiceToken string `mapstructure:"workspace_service_token"`

	// GatewayHMACSecret is the shared secret used to verify the gateway-origin
	// identity signature (conventions §24.1). It MUST equal the gateway's
	// GATEWAY_HMAC_SECRET. Non-dev (staging/production) fails fast at boot if
	// empty or shorter than 32 chars; development may omit it (verification
	// disabled, mirroring the gateway which also disables signing in dev).
	// chmod 0600 the file that provides it; prefer the env var as canonical.
	// Env: MARKETPLACE_GATEWAY_HMAC_SECRET
	GatewayHMACSecret string `mapstructure:"gateway_hmac_secret"`

	// UserRateLimitPerMin is the per-authenticated-user request rate limit
	// (requests per minute). Set to 0 to disable per-user rate limiting entirely.
	// Default: 120. Env: MARKETPLACE_USER_RATE_LIMIT_PER_MIN
	UserRateLimitPerMin int `mapstructure:"user_rate_limit_per_min"`

	// UserRateLimitBurst is the token-bucket burst capacity for the per-user
	// rate limiter. MUST be > 0 when UserRateLimitPerMin > 0.
	// Default: 20. Env: MARKETPLACE_USER_RATE_LIMIT_BURST
	UserRateLimitBurst int `mapstructure:"user_rate_limit_burst"`
}

// Load reads configuration from environment variables (prefix MARKETPLACE_).
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("MARKETPLACE")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":                    "MARKETPLACE_PORT",
		"postgres_dsn":            "MARKETPLACE_POSTGRES_DSN",
		"postgres_schema":         "MARKETPLACE_DB_SCHEMA",
		"redis_url":               "MARKETPLACE_REDIS_URL",
		"log_level":               "MARKETPLACE_LOG_LEVEL",
		"env":                     "MARKETPLACE_ENV",
		"db_max_conns":            "MARKETPLACE_DB_MAX_CONNS",
		"db_min_conns":            "MARKETPLACE_DB_MIN_CONNS",
		"workspace_base_url":      "MARKETPLACE_WORKSPACE_BASE_URL",
		"workspace_service_token": "MARKETPLACE_WORKSPACE_SERVICE_TOKEN",
		"gateway_hmac_secret":     "MARKETPLACE_GATEWAY_HMAC_SECRET",
		"user_rate_limit_per_min": "MARKETPLACE_USER_RATE_LIMIT_PER_MIN",
		"user_rate_limit_burst":   "MARKETPLACE_USER_RATE_LIMIT_BURST",
	}

	for key, envKey := range bindings {
		if err := v.BindEnv(key, envKey); err != nil {
			return nil, fmt.Errorf("config bind %q: %w", key, err)
		}
	}

	v.SetDefault("port", 8081)
	v.SetDefault("log_level", "INFO")
	// NOTE: MARKETPLACE_ENV has NO default — it is required and validated
	// explicitly below. A silent default to "production" would mask a
	// misconfigured deploy; a default to "development" would disable
	// gateway-signature verification §24.1. Fail-closed: empty env → boot error.
	v.SetDefault("db_max_conns", 10)
	v.SetDefault("db_min_conns", 2)
	v.SetDefault("user_rate_limit_per_min", 120)
	v.SetDefault("user_rate_limit_burst", 20)

	var cfg Config

	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []string

	if c.PostgresDSN == "" {
		errs = append(errs, "MARKETPLACE_POSTGRES_DSN is required")
	}

	if c.Port <= 0 || c.Port > 65535 {
		errs = append(errs, "MARKETPLACE_PORT must be 1-65535")
	}

	validLogLevels := map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
	if !validLogLevels[strings.ToUpper(c.LogLevel)] {
		errs = append(errs, "MARKETPLACE_LOG_LEVEL must be DEBUG|INFO|WARN|ERROR")
	}

	// Fail-closed env posture: MARKETPLACE_ENV MUST be one of the three known
	// values. Empty (unset) or any unknown string is a boot error — no silent
	// default. This guards §24.1: a misconfigured env must never silently land
	// in a posture that disables gateway-signature verification.
	validEnvs := map[string]bool{"development": true, "staging": true, "production": true}
	if !validEnvs[strings.ToLower(c.Env)] {
		errs = append(errs, "MARKETPLACE_ENV must be explicitly set to development|staging|production")
	}

	if c.PostgresSchema != "" && !schemaNameRe.MatchString(c.PostgresSchema) {
		errs = append(errs, "MARKETPLACE_DB_SCHEMA must start with a letter or underscore and contain only [a-zA-Z0-9_] characters")
	}

	if c.DBMaxConns < 0 || c.DBMaxConns > 65535 {
		errs = append(errs, "MARKETPLACE_DB_MAX_CONNS must be 0-65535 (0 = use default of 10)")
	}

	if c.DBMinConns < 0 || c.DBMinConns > 65535 {
		errs = append(errs, "MARKETPLACE_DB_MIN_CONNS must be 0-65535 (0 = use default of 2)")
	}

	errs = append(errs, c.validateWorkspace()...)
	errs = append(errs, c.validateGatewayHMAC()...)
	errs = append(errs, c.validateUserRateLimit()...)

	if len(errs) > 0 {
		return errors.New("config validation failed: " + strings.Join(errs, "; "))
	}

	return nil
}

// minServiceTokenLen is the minimum length of the workspace service token.
const minServiceTokenLen = 32

// validateWorkspace returns the validation errors for the workspace S2S config.
//
// Production guard (M-2): in non-dev environments the workspace service MUST be
// configured. Without this, an unset base URL leaves workspaceClient == nil and
// AcceptBid silently fails open — the bid is accepted but no workspace contract is
// ever created. Fail loudly at startup instead.
//
// Security guard: in non-dev, WorkspaceBaseURL MUST use https:// to prevent the
// S2S service token from being sent in cleartext. http://localhost is the only
// permitted http:// form (useful for integration tests and local dev).
func (c *Config) validateWorkspace() []string {
	var errs []string

	if !c.IsDev() {
		if c.WorkspaceBaseURL == "" {
			errs = append(errs, "MARKETPLACE_WORKSPACE_BASE_URL must be set in production")
		}

		if len(c.WorkspaceServiceToken) < minServiceTokenLen {
			errs = append(errs, "MARKETPLACE_WORKSPACE_SERVICE_TOKEN must be at least 32 characters in production")
		}
	}

	// WorkspaceServiceToken is required (≥32 chars) whenever WorkspaceBaseURL is set,
	// even in dev — a configured base URL without a valid token is always a misconfig.
	if c.WorkspaceBaseURL != "" && len(c.WorkspaceServiceToken) < minServiceTokenLen {
		errs = append(errs, "MARKETPLACE_WORKSPACE_SERVICE_TOKEN must be at least 32 characters when MARKETPLACE_WORKSPACE_BASE_URL is set")
	}

	// In non-dev, enforce https:// to prevent the S2S token from being sent in
	// cleartext. http://localhost is permitted in all envs (integration tests, local dev).
	if !c.IsDev() && c.WorkspaceBaseURL != "" {
		lower := strings.ToLower(c.WorkspaceBaseURL)
		if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://localhost") {
			errs = append(errs, "MARKETPLACE_WORKSPACE_BASE_URL must use https:// in non-dev environments (http://localhost is the only permitted http:// exception)")
		}
	}

	return errs
}

// minHMACSecretLen is the minimum length of the gateway HMAC secret. It mirrors
// the gateway's GATEWAY_HMAC_SECRET ≥32-char requirement (conventions §24.1).
const minHMACSecretLen = 32

// validateGatewayHMAC enforces the §24.1 fail-closed secret posture:
//   - non-dev (staging/production): secret is REQUIRED and MUST be ≥32 chars —
//     boot fails fast otherwise (mirrors the gateway which fails fast in non-dev).
//   - dev: secret may be empty (verification disabled, mirroring the gateway's
//     dev signing-skip); but if a secret IS provided it must still be ≥32 chars
//     so a too-short dev secret never masquerades as a valid one.
func (c *Config) validateGatewayHMAC() []string {
	var errs []string

	if !c.IsDev() {
		if len(c.GatewayHMACSecret) < minHMACSecretLen {
			errs = append(errs, "MARKETPLACE_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev (staging/production) environments")
		}

		return errs
	}

	// Dev: empty is allowed (verification disabled); non-empty must be ≥32.
	if c.GatewayHMACSecret != "" && len(c.GatewayHMACSecret) < minHMACSecretLen {
		errs = append(errs, "MARKETPLACE_GATEWAY_HMAC_SECRET, when set, must be at least 32 characters")
	}

	return errs
}

// validateUserRateLimit returns validation errors for the per-user rate limit config.
//
// Rules:
//   - UserRateLimitPerMin >= 0 (0 = disabled)
//   - When UserRateLimitPerMin > 0, UserRateLimitBurst MUST be > 0 (a burst of 0
//     would make every request immediately rejected by the token bucket).
func (c *Config) validateUserRateLimit() []string {
	var errs []string

	if c.UserRateLimitPerMin < 0 {
		errs = append(errs, "MARKETPLACE_USER_RATE_LIMIT_PER_MIN must be >= 0 (0 = disabled)")
	}

	if c.UserRateLimitPerMin > 0 && c.UserRateLimitBurst <= 0 {
		errs = append(errs, "MARKETPLACE_USER_RATE_LIMIT_BURST must be > 0 when MARKETPLACE_USER_RATE_LIMIT_PER_MIN > 0")
	}

	return errs
}

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}
