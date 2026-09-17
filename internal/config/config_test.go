package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// clearAll resets every env var Load reads to "unset" (via t.Setenv, which
// restores the original value after the test) so tests never depend on
// whatever happens to be in the surrounding shell environment.
func clearAll(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"APP_ENV", "DATABASE_URL", "DB_MAX_OPEN_CONNS", "DB_MIN_CONNS",
		"DB_CONN_MAX_LIFETIME", "DB_CONN_MAX_IDLE_TIME", "LOG_LEVEL",
		"LOG_FORMAT", "SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_MissingRequiredDatabaseURL(t *testing.T) {
	clearAll(t)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DATABASE_URL, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Errorf("error = %q, want it to mention DATABASE_URL is required", err.Error())
	}
}

func TestLoad_DefaultsAppliedWhenOnlyRequiredFieldSet(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.AppEnv != EnvDevelopment {
		t.Errorf("AppEnv = %q, want %q", cfg.AppEnv, EnvDevelopment)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("Database.MaxConns = %d, want 10", cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns != 2 {
		t.Errorf("Database.MinConns = %d, want 2", cfg.Database.MinConns)
	}
	if cfg.Database.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("Database.ConnMaxLifetime = %s, want 30m", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Database.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("Database.ConnMaxIdleTime = %s, want 5m", cfg.Database.ConnMaxIdleTime)
	}
	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("Log.Level = %v, want info", cfg.Log.Level)
	}
	if cfg.Log.Format != LogFormatJSON {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, LogFormatJSON)
	}
	if cfg.Shutdown.Timeout != 15*time.Second {
		t.Errorf("Shutdown.Timeout = %s, want 15s", cfg.Shutdown.Timeout)
	}
}

func TestLoad_OverridesRespected(t *testing.T) {
	clearAll(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgresql://user:pass@db:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "25")
	t.Setenv("DB_MIN_CONNS", "10")
	t.Setenv("DB_CONN_MAX_LIFETIME", "1h")
	t.Setenv("DB_CONN_MAX_IDLE_TIME", "10m")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("SHUTDOWN_TIMEOUT", "30s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.AppEnv != EnvProduction {
		t.Errorf("AppEnv = %q, want %q", cfg.AppEnv, EnvProduction)
	}
	if cfg.Database.MaxConns != 25 {
		t.Errorf("Database.MaxConns = %d, want 25", cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns != 10 {
		t.Errorf("Database.MinConns = %d, want 10", cfg.Database.MinConns)
	}
	if cfg.Database.ConnMaxLifetime != time.Hour {
		t.Errorf("Database.ConnMaxLifetime = %s, want 1h", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("Log.Level = %v, want debug", cfg.Log.Level)
	}
	if cfg.Log.Format != LogFormatText {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, LogFormatText)
	}
	if cfg.Shutdown.Timeout != 30*time.Second {
		t.Errorf("Shutdown.Timeout = %s, want 30s", cfg.Shutdown.Timeout)
	}
}

func TestLoad_InvalidAppEnv(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("APP_ENV", "prooduction")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid APP_ENV, got nil")
	}
	if !strings.Contains(err.Error(), "APP_ENV must be one of") {
		t.Errorf("error = %q, want it to mention APP_ENV must be one of", err.Error())
	}
}

func TestLoad_InvalidDatabaseURLScheme(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "mysql://user:pass@localhost:3306/jobs")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for non-postgres DATABASE_URL, got nil")
	}
	if !strings.Contains(err.Error(), "must be a valid postgres") {
		t.Errorf("error = %q, want it to mention postgres scheme requirement", err.Error())
	}
}

// Regression test: an invalid DATABASE_URL's error message must never
// contain the raw credential, in any of the shapes a plausible operator
// typo produces — not just a wrong-but-well-formed scheme. A URL with an
// unparseable port fails url.Parse entirely; a URL missing its scheme
// entirely (a forgotten "postgres://") parses successfully but with the
// password landing in an opaque, unrecognized position. Both used to
// leak the raw value verbatim because the old code fell back to
// echoing it whenever it couldn't confidently redact.
func TestLoad_InvalidDatabaseURLErrorNeverContainsThePassword(t *testing.T) {
	cases := map[string]string{
		"wrong scheme, well-formed": "mysql://user:supersecret@localhost:3306/jobs",
		"unparseable port":          "postgres://user:supersecret@localhost:port/jobs",
		"missing scheme entirely":   "user:supersecret@localhost:5432/jobs",
	}
	for name, dbURL := range cases {
		t.Run(name, func(t *testing.T) {
			clearAll(t)
			t.Setenv("DATABASE_URL", dbURL)

			_, err := Load()
			if err == nil {
				t.Fatal("expected error for invalid DATABASE_URL, got nil")
			}
			if strings.Contains(err.Error(), "supersecret") {
				t.Errorf("error leaked the password: %q", err.Error())
			}
		})
	}
}

func TestLoad_MinConnsExceedingMaxConnsRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "5")
	t.Setenv("DB_MIN_CONNS", "20")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when min conns exceed max conns, got nil")
	}
	if !strings.Contains(err.Error(), "cannot exceed DB_MAX_OPEN_CONNS") {
		t.Errorf("error = %q, want it to mention the min/max conflict", err.Error())
	}
}

func TestLoad_MinConnsZeroIsValid(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MIN_CONNS", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error for DB_MIN_CONNS=0: %v", err)
	}
	if cfg.Database.MinConns != 0 {
		t.Errorf("Database.MinConns = %d, want 0", cfg.Database.MinConns)
	}
}

func TestLoad_MaxConnsOutOfInt32RangeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "4294967306")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for DB_MAX_OPEN_CONNS beyond int32 range, got nil")
	}
	if !strings.Contains(err.Error(), "DB_MAX_OPEN_CONNS must be between") {
		t.Errorf("error = %q, want it to mention the valid range", err.Error())
	}
}

func TestLoad_ZeroMaxConnsRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for DB_MAX_OPEN_CONNS=0, got nil")
	}
}

func TestLoad_NegativeConnMaxLifetimeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_CONN_MAX_LIFETIME", "-5m")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative DB_CONN_MAX_LIFETIME, got nil")
	}
	if !strings.Contains(err.Error(), "DB_CONN_MAX_LIFETIME must be positive") {
		t.Errorf("error = %q, want it to mention DB_CONN_MAX_LIFETIME must be positive", err.Error())
	}
}

func TestLoad_NegativeConnMaxIdleTimeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_CONN_MAX_IDLE_TIME", "-1s")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative DB_CONN_MAX_IDLE_TIME, got nil")
	}
	if !strings.Contains(err.Error(), "DB_CONN_MAX_IDLE_TIME must be positive") {
		t.Errorf("error = %q, want it to mention DB_CONN_MAX_IDLE_TIME must be positive", err.Error())
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid LOG_LEVEL, got nil")
	}
	if !strings.Contains(err.Error(), "LOG_LEVEL must be one of") {
		t.Errorf("error = %q, want it to mention LOG_LEVEL must be one of", err.Error())
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_CONN_MAX_LIFETIME", "not-a-duration")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
	if !strings.Contains(err.Error(), "DB_CONN_MAX_LIFETIME must be a valid duration") {
		t.Errorf("error = %q, want it to mention DB_CONN_MAX_LIFETIME", err.Error())
	}
}

func TestLoad_MultipleFailuresAreAllReported(t *testing.T) {
	clearAll(t)
	t.Setenv("APP_ENV", "bogus")
	t.Setenv("LOG_LEVEL", "bogus")
	// DATABASE_URL intentionally left unset.

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{"APP_ENV must be one of", "LOG_LEVEL must be one of", "DATABASE_URL is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to also contain %q", err.Error(), want)
		}
	}
}

func TestDatabaseConfig_LogValueRedactsUserinfoPassword(t *testing.T) {
	d := DatabaseConfig{URL: "postgres://user:supersecret@localhost:5432/jobs"}

	got := d.LogValue().String()
	if strings.Contains(got, "supersecret") {
		t.Errorf("LogValue() leaked the password: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("LogValue() = %q, want it to contain REDACTED", got)
	}
}

// Regression test: pgx/libpq also accept a "password" query parameter as
// an alternative to userinfo. redactURLCredentials must catch both forms,
// not just postgres://user:pass@host.
func TestDatabaseConfig_LogValueRedactsQueryStringPassword(t *testing.T) {
	d := DatabaseConfig{URL: "postgres://user@localhost:5432/jobs?password=supersecret&sslmode=disable"}

	got := d.LogValue().String()
	if strings.Contains(got, "supersecret") {
		t.Errorf("LogValue() leaked the query-string password: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("LogValue() = %q, want it to contain REDACTED", got)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("LogValue() = %q, want it to preserve non-secret query params", got)
	}
}

func TestDatabaseConfig_LogValuePassesThroughURLWithoutCredentials(t *testing.T) {
	d := DatabaseConfig{URL: "postgres://localhost:5432/jobs"}

	got := d.LogValue().String()
	if !strings.Contains(got, "localhost:5432/jobs") {
		t.Errorf("LogValue() = %q, want it to preserve a credential-free URL", got)
	}
}
