package config_test

import (
	"testing"

	"github.com/CoverOnes/marketplace/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDSN is a placeholder DSN used in tests only — not a real credential.
//
//nolint:gosec // G101: test fixture placeholder, not a real credential; user:pass are inert strings
const testDSN = "postgres://user:pass@localhost/db"

// TestConfig_Load covers config loading and validation paths.
// t.Setenv is not compatible with t.Parallel(); subtests run sequentially.
func TestConfig_Load(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		wantErr bool
	}{
		{
			name: "happy path: minimal valid config",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
			},
			wantErr: false,
		},
		{
			name: "happy path: valid schema name",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_SCHEMA":    "marketplace",
			},
			wantErr: false,
		},
		{
			name: "happy path: empty schema is allowed (public)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_SCHEMA":    "",
			},
			wantErr: false,
		},
		{
			name: "error: missing postgres dsn",
			envVars: map[string]string{
				"MARKETPLACE_PORT": "8081",
			},
			wantErr: true,
		},
		{
			name: "error: invalid log level",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_LOG_LEVEL":    "VERBOSE",
			},
			wantErr: true,
		},
		{
			name: "error: invalid port 0",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "0",
			},
			wantErr: true,
		},
		{
			name: "error: invalid port too high",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "99999",
			},
			wantErr: true,
		},
		{
			name: "error: schema name with hyphen rejected",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_SCHEMA":    "bad-schema",
			},
			wantErr: true,
		},
		{
			name: "error: schema name with space rejected",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_SCHEMA":    "bad schema",
			},
			wantErr: true,
		},
		{
			name: "error: schema name with semicolon rejected (SQL injection attempt)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_SCHEMA":    "s;DROP TABLE listings--",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// NOTE: t.Parallel() is intentionally omitted; t.Setenv requires sequential subtests.

			// Clear known env vars first to avoid cross-test pollution.
			allKnownVars := []string{
				"MARKETPLACE_POSTGRES_DSN", "MARKETPLACE_PORT", "MARKETPLACE_LOG_LEVEL",
				"MARKETPLACE_ENV", "MARKETPLACE_DB_SCHEMA", "MARKETPLACE_REDIS_URL",
			}
			for _, k := range allKnownVars {
				t.Setenv(k, "")
			}

			for k, v := range tc.envVars {
				t.Setenv(k, v)
			}

			_, err := config.Load()

			if tc.wantErr {
				require.Error(t, err, "expected error for case %q", tc.name)
			} else {
				assert.NoError(t, err, "should not get error for valid config in case %q", tc.name)
			}
		})
	}
}
