package company_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// testDatabaseURL returns the DATABASE_URL to run integration tests
// against, skipping the test entirely if it isn't set. These tests hit a
// real PostgreSQL instance (docker compose up -d db) rather than a mock,
// per CLAUDE.md's "Integration tests: PostgreSQL, migrations,
// repositories, upserts, transactions, constraints."
//
// resetSchema calls MigrateDown, which drops every table. To make it
// structurally impossible to point this at a real dev/prod database by
// copy-pasting the wrong URL, the database name must end in "_test".
// Same guard, verbatim, as internal/database's own tests.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; run `docker compose up -d db` and export TEST_DATABASE_URL to run this test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse as a URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("TEST_DATABASE_URL points at database %q; these tests drop tables and must run "+
			"against a database whose name ends in \"_test\" (see docker-compose.yml's aggregator_test)", dbName)
	}
	return raw
}

func dbConfig(url string) config.DatabaseConfig {
	return config.DatabaseConfig{
		URL:             url,
		MaxConns:        5,
		MinConns:        0,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	}
}

// resetSchema drops and re-applies migrations so each test starts from a
// clean, known schema regardless of what earlier test runs left behind.
// A real failure here (e.g. a dirty schema_migrations row left by a
// crashed previous run) is not something tests should silently paper
// over, so every error fails the test rather than being discarded.
//
// This package and internal/database both reset the SAME database, and
// `go test ./...` runs package binaries in parallel by default — which
// means the two drop each other's tables mid-test and both fail with
// "relation does not exist". Integration runs must therefore be
// serialized: `go test -p 1 ./...`. Verified: without -p 1 the two
// packages fail each other reproducibly; with it, both pass.
func resetSchema(t *testing.T, url string) {
	t.Helper()
	if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("resetSchema: MigrateDown() failed: %v", err)
	}
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("resetSchema: MigrateUp() failed: %v", err)
	}
	t.Cleanup(func() {
		if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateDown() failed: %v", err)
		}
	})
}

// newDB resets the schema and returns an open pool for the test,
// registering its own cleanup. Every test in this package starts this
// way, so it is one helper rather than fifteen copies of the same six
// lines.
func newDB(t *testing.T) *database.DB {
	t.Helper()
	url := testDatabaseURL(t)
	resetSchema(t, url)

	db, err := database.New(context.Background(), dbConfig(url))
	if err != nil {
		t.Fatalf("database.New() failed: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func countCompanies(t *testing.T, ctx context.Context, db *database.DB) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM companies`).Scan(&n); err != nil {
		t.Fatalf("counting companies: %v", err)
	}
	return n
}

func TestStoreUpsert_CreatesCompany(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	c, err := store.Upsert(ctx, company.UpsertParams{
		Name:        "Acme",
		Website:     "https://acme.example",
		Description: "Anvils",
	})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	if c.ID <= 0 {
		t.Errorf("ID = %d, want a positive generated identity", c.ID)
	}
	if c.Name != "Acme" {
		t.Errorf("Name = %q, want %q", c.Name, "Acme")
	}
	if c.Website != "https://acme.example" {
		t.Errorf("Website = %q, want %q", c.Website, "https://acme.example")
	}
	if c.Description != "Anvils" {
		t.Errorf("Description = %q, want %q", c.Description, "Anvils")
	}
	if c.Status != company.StatusActive {
		t.Errorf("Status = %q, want %q (the column default)", c.Status, company.StatusActive)
	}
	if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		t.Errorf("timestamps not populated: created_at=%s updated_at=%s", c.CreatedAt, c.UpdatedAt)
	}
}

// Omitted optional fields must land as NULL, not as an empty string: a blank website
// would violate the schema's CHECK, and a blank description would make
// "" indistinguishable from a real empty description later.
func TestStoreUpsert_StoresOmittedOptionalFieldsAsNull(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	c, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	if c.Website != "" || c.Description != "" {
		t.Errorf("website=%q description=%q, want both empty", c.Website, c.Description)
	}

	var websiteNull, descriptionNull bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT website IS NULL, description IS NULL FROM companies WHERE id = $1`, c.ID,
	).Scan(&websiteNull, &descriptionNull); err != nil {
		t.Fatalf("checking null-ness: %v", err)
	}
	if !websiteNull || !descriptionNull {
		t.Errorf("website IS NULL = %t, description IS NULL = %t; want both true", websiteNull, descriptionNull)
	}
}

// The unique index is on lower(btrim(name)), so the ON CONFLICT target
// must be that same expression. This is the test that proves the
// expression-based inference actually resolves a real conflict rather
// than merely parsing.
func TestStoreUpsert_MatchesExistingCompanyIgnoringCaseAndWhitespace(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	first, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme"})
	if err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.UpsertParams{Name: "  ACME  "})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("second Upsert() returned id %d, want the existing %d — ON CONFLICT did not match", second.ID, first.ID)
	}
	if n := countCompanies(t, ctx, db); n != 1 {
		t.Errorf("companies row count = %d, want 1 (a duplicate was created)", n)
	}
	// The display name stays as first written; DO UPDATE never touches it.
	if second.Name != "Acme" {
		t.Errorf("Name = %q, want the originally stored %q", second.Name, "Acme")
	}
}

// Names are trimmed before storage, so the stored display name never
// carries whitespace that btrim() would ignore for identity purposes.
func TestStoreUpsert_TrimsStoredName(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	c, err := store.Upsert(ctx, company.UpsertParams{Name: "  Globex  "})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	if c.Name != "Globex" {
		t.Errorf("Name = %q, want %q", c.Name, "Globex")
	}
}

func TestStoreUpsert_FillsNullWebsiteAndDescriptionOnConflict(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	first, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme"})
	if err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.UpsertParams{
		Name:        "acme",
		Website:     "https://acme.example",
		Description: "Anvils",
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("second Upsert() returned id %d, want %d", second.ID, first.ID)
	}
	if second.Website != "https://acme.example" {
		t.Errorf("Website = %q, want the newly supplied %q", second.Website, "https://acme.example")
	}
	if second.Description != "Anvils" {
		t.Errorf("Description = %q, want the newly supplied %q", second.Description, "Anvils")
	}
}

func TestStoreUpsert_DoesNotOverwriteExistingWebsiteOrDescription(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	first, err := store.Upsert(ctx, company.UpsertParams{
		Name:        "Acme",
		Website:     "https://acme.example",
		Description: "Anvils",
	})
	if err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.UpsertParams{
		Name:        "ACME",
		Website:     "https://acme-corp.example",
		Description: "Rockets",
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("second Upsert() returned id %d, want %d", second.ID, first.ID)
	}
	if second.Website != "https://acme.example" {
		t.Errorf("Website = %q, want the original %q to be preserved", second.Website, "https://acme.example")
	}
	if second.Description != "Anvils" {
		t.Errorf("Description = %q, want the original %q to be preserved", second.Description, "Anvils")
	}

	// And the row itself, not just what RETURNING handed back.
	stored, err := store.GetByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if stored.Website != "https://acme.example" || stored.Description != "Anvils" {
		t.Errorf("stored row = (%q, %q), want the original values", stored.Website, stored.Description)
	}
}

// An empty website parameter must not clobber a stored one either: it
// becomes NULL, and COALESCE then keeps what is already there.
func TestStoreUpsert_BlankWebsiteDoesNotClearStoredWebsite(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	if _, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme", Website: "https://acme.example"}); err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme"})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}
	if second.Website != "https://acme.example" {
		t.Errorf("Website = %q, want the stored %q", second.Website, "https://acme.example")
	}
}

// Blank names are rejected by our own validation, before any SQL runs —
// a blank name can only ever produce a CHECK violation, and a caller
// debugging one deserves to read "name is required" rather than a
// constraint name.
func TestStoreUpsert_RejectsBlankName(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	cases := map[string]string{
		"empty":      "",
		"spaces":     "   ",
		"whitespace": "\t\n ",
	}
	for name, value := range cases {
		c, err := store.Upsert(ctx, company.UpsertParams{Name: value})
		if err == nil {
			t.Errorf("%s: Upsert(%q) succeeded, want an error", name, value)
			continue
		}
		if c != nil {
			t.Errorf("%s: Upsert() returned a non-nil company alongside an error", name)
		}
		if !strings.Contains(err.Error(), "company: ") {
			t.Errorf("%s: error = %q, want a \"company: \" prefix", name, err)
		}
		if !strings.Contains(err.Error(), "name is required") {
			t.Errorf("%s: error = %q, want it to name the missing field", name, err)
		}
	}

	if n := countCompanies(t, ctx, db); n != 0 {
		t.Errorf("companies row count = %d, want 0 — validation should have run before any SQL", n)
	}
}

// The ON CONFLICT clause only targets the name index. Two different
// names claiming the same normalized website collide on
// companies_website_idx instead, which is not inferred and must surface
// as a wrapped driver error rather than a panic or a silent success.
func TestStoreUpsert_DuplicateWebsiteUnderDifferentNameReturnsError(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	if _, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme", Website: "https://acme.example"}); err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	// Same website up to normalize_website()'s case-fold and
	// trailing-slash strip, different name.
	c, err := store.Upsert(ctx, company.UpsertParams{Name: "Globex", Website: "HTTPS://ACME.EXAMPLE/"})
	if err == nil {
		t.Fatalf("Upsert() succeeded with a duplicate normalized website, want a unique violation (returned id %d)", c.ID)
	}
	if c != nil {
		t.Errorf("Upsert() returned a non-nil company alongside an error")
	}
	if !strings.HasPrefix(err.Error(), "company: ") {
		t.Errorf("error = %q, want a \"company: \" prefix", err)
	}

	// %w wrapping must survive: callers (and future retry logic) need to
	// reach the SQLSTATE, not just a string.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %q, want it to wrap a *pgconn.PgError", err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("SQLSTATE = %q, want 23505 (unique_violation)", pgErr.Code)
	}
	if pgErr.ConstraintName != "companies_website_idx" {
		t.Errorf("constraint = %q, want companies_website_idx", pgErr.ConstraintName)
	}

	if n := countCompanies(t, ctx, db); n != 1 {
		t.Errorf("companies row count = %d, want 1", n)
	}
}

// Concurrency is the whole reason this is one atomic statement rather
// than a SELECT followed by an INSERT: under -race, every caller must
// get the same row back and exactly one row must exist.
func TestStoreUpsert_ConcurrentCallersConvergeOnOneRow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	const callers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  []int64
		errs []error
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := store.Upsert(ctx, company.UpsertParams{Name: "Initech"})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids = append(ids, c.ID)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent Upsert() failed: %v", err)
	}
	if len(ids) != callers {
		t.Fatalf("got %d successful upserts, want %d", len(ids), callers)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent upserts returned different ids: %v", ids)
		}
	}
	if n := countCompanies(t, ctx, db); n != 1 {
		t.Errorf("companies row count = %d, want 1", n)
	}
}

func TestStoreGetByID_ReturnsStoredCompany(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	created, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme", Website: "https://acme.example"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	got, err := store.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name || got.Website != created.Website {
		t.Errorf("GetByID() = %+v, want %+v", got, created)
	}
	if got.Status != company.StatusActive {
		t.Errorf("Status = %q, want %q", got.Status, company.StatusActive)
	}
}

func TestStoreGetByID_ReturnsErrNotFoundForMissingRow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	c, err := store.GetByID(ctx, 424242)
	if !errors.Is(err, company.ErrNotFound) {
		t.Fatalf("GetByID() error = %v, want it to wrap company.ErrNotFound", err)
	}
	if c != nil {
		t.Errorf("GetByID() returned a non-nil company alongside ErrNotFound")
	}
}

func TestStoreGetByName_IgnoresCaseAndWhitespace(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	created, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	for _, lookup := range []string{"Acme", "acme", "  ACME  ", "aCmE"} {
		got, err := store.GetByName(ctx, lookup)
		if err != nil {
			t.Errorf("GetByName(%q) failed: %v", lookup, err)
			continue
		}
		if got.ID != created.ID {
			t.Errorf("GetByName(%q) = id %d, want %d", lookup, got.ID, created.ID)
		}
	}
}

func TestStoreGetByName_ReturnsErrNotFoundForMissingRow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	if _, err := store.Upsert(ctx, company.UpsertParams{Name: "Acme"}); err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	c, err := store.GetByName(ctx, "Globex")
	if !errors.Is(err, company.ErrNotFound) {
		t.Fatalf("GetByName() error = %v, want it to wrap company.ErrNotFound", err)
	}
	if c != nil {
		t.Errorf("GetByName() returned a non-nil company alongside ErrNotFound")
	}
}
