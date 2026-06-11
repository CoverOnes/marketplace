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

// testServiceToken is a 32-char placeholder token used in tests only — not a real secret.
const testServiceToken = "abcdefghijklmnopqrstuvwxyz012345"

// testHMACSecret is a 32-char placeholder HMAC secret used in tests only — not a real secret.
const testHMACSecret = "0123456789abcdef0123456789abcdef"

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
		{
			name: "error: schema name with leading digit rejected (invalid PG identifier)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_SCHEMA":    "1marketplace",
			},
			wantErr: true,
		},
		{
			name: "happy path: explicit db pool sizing",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_MAX_CONNS": "3",
				"MARKETPLACE_DB_MIN_CONNS": "1",
			},
			wantErr: false,
		},
		{
			name: "happy path: zero db pool sizing uses defaults",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
				"MARKETPLACE_DB_MAX_CONNS": "0",
				"MARKETPLACE_DB_MIN_CONNS": "0",
			},
			wantErr: false,
		},
		{
			// FIX 2 (M-2): production MUST configure the workspace service or the
			// S2S contract-create path silently fails open. Missing base URL fails.
			name: "error: production without workspace base URL",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "production",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
			},
			wantErr: true,
		},
		{
			name: "error: production without workspace service token",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":       testDSN,
				"MARKETPLACE_PORT":               "8081",
				"MARKETPLACE_LOG_LEVEL":          "INFO",
				"MARKETPLACE_ENV":                "production",
				"MARKETPLACE_WORKSPACE_BASE_URL": "http://workspace:8082",
			},
			wantErr: true,
		},
		{
			name: "error: production with short workspace service token",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "production",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "http://workspace:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": "tooshort",
			},
			wantErr: true,
		},
		{
			name: "happy path: production with workspace fully configured (https)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "production",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "https://workspace:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
				"MARKETPLACE_GATEWAY_HMAC_SECRET":     testHMACSecret,
			},
			wantErr: false,
		},
		{
			// Security guard: non-dev with plain http:// (not localhost) must be rejected.
			name: "error: production with http:// workspace URL (token would be cleartext)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "production",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "http://workspace:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
				"MARKETPLACE_GATEWAY_HMAC_SECRET":     testHMACSecret,
			},
			wantErr: true,
		},
		{
			// C-1 security fix: "http://localhostevil.com" must NOT pass the localhost
			// exception — a prefix check would accept it, but the url.Parse hostname
			// check correctly rejects it (hostname is "localhostevil.com", not "localhost").
			name: "error: staging with http://localhostevil.com rejected (localhost prefix bypass)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "staging",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "http://localhostevil.com:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
				"MARKETPLACE_GATEWAY_HMAC_SECRET":     testHMACSecret,
			},
			wantErr: true,
		},
		{
			// Fail-closed env posture: an unset MARKETPLACE_ENV must be a boot
			// error, never a silent default to production/development.
			name: "error: empty env is rejected (no silent default)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				// MARKETPLACE_ENV intentionally unset.
			},
			wantErr: true,
		},
		{
			name: "error: unknown env value is rejected",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "prod",
			},
			wantErr: true,
		},
		{
			name: "happy path: staging is a valid explicit env",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "staging",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "https://workspace:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
				"MARKETPLACE_GATEWAY_HMAC_SECRET":     testHMACSecret,
			},
			wantErr: false,
		},
		{
			// §24.1 fail-closed: non-dev MUST have a gateway HMAC secret or boot fails.
			name: "error: production without gateway HMAC secret",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "production",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "http://workspace:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
				// MARKETPLACE_GATEWAY_HMAC_SECRET intentionally unset.
			},
			wantErr: true,
		},
		{
			name: "error: production with short gateway HMAC secret",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "production",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "http://workspace:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
				"MARKETPLACE_GATEWAY_HMAC_SECRET":     "tooshort",
			},
			wantErr: true,
		},
		{
			// Dev exempt: empty gateway secret is allowed (verification disabled).
			name: "happy path: development without gateway HMAC secret (exempt)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
			},
			wantErr: false,
		},
		{
			// Even in dev, a provided-but-too-short secret is rejected so it can't
			// masquerade as valid.
			name: "error: development with short gateway HMAC secret",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":        testDSN,
				"MARKETPLACE_PORT":                "8081",
				"MARKETPLACE_LOG_LEVEL":           "INFO",
				"MARKETPLACE_ENV":                 "development",
				"MARKETPLACE_GATEWAY_HMAC_SECRET": "tooshort",
			},
			wantErr: true,
		},
		{
			// Dev is exempt from the production workspace guard: an unset base URL
			// is allowed so local dev can run without the workspace service.
			name: "happy path: development without workspace config (exempt)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN": testDSN,
				"MARKETPLACE_PORT":         "8081",
				"MARKETPLACE_LOG_LEVEL":    "INFO",
				"MARKETPLACE_ENV":          "development",
			},
			wantErr: false,
		},
		{
			// Development allows http://localhost for integration-test convenience.
			name: "happy path: development with http://localhost workspace URL",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "development",
				"MARKETPLACE_WORKSPACE_BASE_URL":      "http://localhost:8082",
				"MARKETPLACE_WORKSPACE_SERVICE_TOKEN": testServiceToken,
			},
			wantErr: false,
		},
		{
			// validateUserRateLimit: negative perMin is rejected.
			name: "error: negative user rate limit per-min",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "development",
				"MARKETPLACE_USER_RATE_LIMIT_PER_MIN": "-1",
				"MARKETPLACE_USER_RATE_LIMIT_BURST":   "10",
			},
			wantErr: true,
		},
		{
			// validateUserRateLimit: perMin>0 but burst<=0 is rejected (every request
			// would be immediately denied by the token bucket).
			name: "error: positive per-min with zero burst",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "development",
				"MARKETPLACE_USER_RATE_LIMIT_PER_MIN": "60",
				"MARKETPLACE_USER_RATE_LIMIT_BURST":   "0",
			},
			wantErr: true,
		},
		{
			// validateUserRateLimit: perMin=0 disables rate limiting entirely —
			// burst value is irrelevant and must not be checked.
			name: "happy path: per-min=0 disables rate limiting (burst ignored)",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "development",
				"MARKETPLACE_USER_RATE_LIMIT_PER_MIN": "0",
				"MARKETPLACE_USER_RATE_LIMIT_BURST":   "0",
			},
			wantErr: false,
		},
		{
			// validateUserRateLimit: valid perMin and burst — happy path for the limiter.
			name: "happy path: valid user rate limit config",
			envVars: map[string]string{
				"MARKETPLACE_POSTGRES_DSN":            testDSN,
				"MARKETPLACE_PORT":                    "8081",
				"MARKETPLACE_LOG_LEVEL":               "INFO",
				"MARKETPLACE_ENV":                     "development",
				"MARKETPLACE_USER_RATE_LIMIT_PER_MIN": "120",
				"MARKETPLACE_USER_RATE_LIMIT_BURST":   "20",
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// NOTE: t.Parallel() is intentionally omitted; t.Setenv requires sequential subtests.

			// Clear known env vars first to avoid cross-test pollution.
			allKnownVars := []string{
				"MARKETPLACE_POSTGRES_DSN", "MARKETPLACE_PORT", "MARKETPLACE_LOG_LEVEL",
				"MARKETPLACE_ENV", "MARKETPLACE_DB_SCHEMA", "MARKETPLACE_REDIS_URL",
				"MARKETPLACE_DB_MAX_CONNS", "MARKETPLACE_DB_MIN_CONNS",
				"MARKETPLACE_WORKSPACE_BASE_URL", "MARKETPLACE_WORKSPACE_SERVICE_TOKEN",
				"MARKETPLACE_GATEWAY_HMAC_SECRET",
				"MARKETPLACE_USER_RATE_LIMIT_PER_MIN", "MARKETPLACE_USER_RATE_LIMIT_BURST",
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
