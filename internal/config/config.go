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

	// Environment: development | production
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
	}

	for key, envKey := range bindings {
		if err := v.BindEnv(key, envKey); err != nil {
			return nil, fmt.Errorf("config bind %q: %w", key, err)
		}
	}

	v.SetDefault("port", 8081)
	v.SetDefault("log_level", "INFO")
	v.SetDefault("env", "development")
	v.SetDefault("db_max_conns", 10)
	v.SetDefault("db_min_conns", 2)

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

	validEnvs := map[string]bool{"development": true, "production": true, "test": true}
	if !validEnvs[strings.ToLower(c.Env)] {
		errs = append(errs, "MARKETPLACE_ENV must be development|production|test")
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

	return errs
}

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}
