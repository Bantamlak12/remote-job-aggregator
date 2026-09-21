package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// LoadSeedCandidates reads and validates a JSON array of Candidate from
// path — the "seed companies" discovery mechanism CLAUDE.md describes as
// the first of several discovery mechanisms this package should
// eventually support. Every candidate is validated before any are
// returned: a seed file is edited by hand, so a typo in one entry should
// be caught before Run wastes an HTTP round trip on the other 99.
func LoadSeedCandidates(path string) ([]Candidate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("discovery: opening seed file %s: %w", path, err)
	}
	defer f.Close()

	var candidates []Candidate
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&candidates); err != nil {
		return nil, fmt.Errorf("discovery: parsing seed file %s: %w", path, err)
	}

	var errs []error
	for i, c := range candidates {
		if err := c.validate(); err != nil {
			errs = append(errs, fmt.Errorf("candidate %d (%q): %w", i, c.CompanyName, err))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("discovery: invalid seed file %s: %w", path, errors.Join(errs...))
	}

	return candidates, nil
}
