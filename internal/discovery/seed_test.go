package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSeedFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing test seed file: %v", err)
	}
	return path
}

func TestLoadSeedCandidates_ValidFile(t *testing.T) {
	path := writeSeedFile(t, `[
		{"company_name": "Acme", "website": "https://acme.com", "ats_provider": "greenhouse", "external_board_id": "acme", "board_url": "https://boards.greenhouse.io/acme"},
		{"company_name": "Globex", "ats_provider": "lever", "external_board_id": "globex", "board_url": "https://jobs.lever.co/globex"}
	]`)

	candidates, err := LoadSeedCandidates(path)
	if err != nil {
		t.Fatalf("LoadSeedCandidates() failed: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(candidates))
	}
	if candidates[0].CompanyName != "Acme" || candidates[0].Website != "https://acme.com" {
		t.Errorf("candidates[0] = %+v, unexpected fields", candidates[0])
	}
	if candidates[1].Website != "" {
		t.Errorf("candidates[1].Website = %q, want empty (omitted in JSON)", candidates[1].Website)
	}
}

func TestLoadSeedCandidates_MissingFile(t *testing.T) {
	_, err := LoadSeedCandidates("/nonexistent/path/seed.json")
	if err == nil {
		t.Fatal("expected an error for a nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "opening seed file") {
		t.Errorf("error = %q, want it to mention opening the seed file", err.Error())
	}
}

func TestLoadSeedCandidates_InvalidJSON(t *testing.T) {
	path := writeSeedFile(t, `not valid json`)

	_, err := LoadSeedCandidates(path)
	if err == nil {
		t.Fatal("expected an error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parsing seed file") {
		t.Errorf("error = %q, want it to mention parsing the seed file", err.Error())
	}
}

func TestLoadSeedCandidates_RejectsUnknownField(t *testing.T) {
	path := writeSeedFile(t, `[
		{"company_name": "Acme", "ats_provider": "greenhouse", "external_board_id": "acme", "board_url": "https://boards.greenhouse.io/acme", "typo_field": "oops"}
	]`)

	_, err := LoadSeedCandidates(path)
	if err == nil {
		t.Fatal("expected an error for an unknown field, got nil")
	}
}

func TestLoadSeedCandidates_InvalidCandidateRejectsTheWholeFile(t *testing.T) {
	path := writeSeedFile(t, `[
		{"company_name": "Acme", "ats_provider": "greenhouse", "external_board_id": "acme", "board_url": "https://boards.greenhouse.io/acme"},
		{"company_name": "", "ats_provider": "lever", "external_board_id": "globex", "board_url": "https://jobs.lever.co/globex"}
	]`)

	candidates, err := LoadSeedCandidates(path)
	if err == nil {
		t.Fatal("expected an error for the second (invalid) candidate, got nil")
	}
	if candidates != nil {
		t.Errorf("expected no candidates returned when any are invalid, got %+v", candidates)
	}
	if !strings.Contains(err.Error(), "candidate 1") {
		t.Errorf("error = %q, want it to identify candidate index 1 as the problem", err.Error())
	}
}

func TestLoadSeedCandidates_MultipleInvalidCandidatesAreAllReported(t *testing.T) {
	path := writeSeedFile(t, `[
		{"company_name": "", "ats_provider": "greenhouse", "external_board_id": "acme", "board_url": "https://boards.greenhouse.io/acme"},
		{"company_name": "Globex", "ats_provider": "", "external_board_id": "globex", "board_url": "https://jobs.lever.co/globex"}
	]`)

	_, err := LoadSeedCandidates(path)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "candidate 0") || !strings.Contains(err.Error(), "candidate 1") {
		t.Errorf("error = %q, want it to mention both candidate 0 and candidate 1", err.Error())
	}
}

func TestLoadSeedCandidates_RejectsNonHTTPBoardURL(t *testing.T) {
	path := writeSeedFile(t, `[
		{"company_name": "Acme", "ats_provider": "greenhouse", "external_board_id": "acme", "board_url": "ftp://boards.greenhouse.io/acme"}
	]`)

	_, err := LoadSeedCandidates(path)
	if err == nil {
		t.Fatal("expected an error for a non-http(s) board_url, got nil")
	}
}
