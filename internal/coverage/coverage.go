// Package coverage validates the implementation coverage manifest against the
// requirements and acceptance gates declared by the MiniCloud specification.
package coverage

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

const (
	ManifestSchemaVersion = 1
	StatusPlanned         = "planned"
	StatusCovered         = "covered"
)

var (
	requirementRowPattern = regexp.MustCompile(`^\|\s*((?:FN|ART|ABI|RUN|CAP|WRK|SCH|SCL|DSC|RTE|SYN|ASY|TRG|RFT|API|RPC|OBS|CHA|NFR-(?:AVL|PERF|SEC|MNT))-\d+)\s*\|\s*(P[012])\s*\|`)
	gateRowPattern        = regexp.MustCompile(`^\|\s*(E2E-\d+)\s*\|\s*(P[012])\s*\|`)
	e2eIDPattern          = regexp.MustCompile(`^E2E-\d+$`)
)

// Manifest is the machine-readable implementation coverage declaration.
type Manifest struct {
	SchemaVersion int           `json:"schema_version"`
	Source        string        `json:"source"`
	Requirements  []Requirement `json:"requirements"`
}

// Requirement records the planned or completed evidence for one requirement.
type Requirement struct {
	RequirementID string   `json:"requirement_id"`
	Priority      string   `json:"priority"`
	Owner         string   `json:"owner"`
	Status        string   `json:"status"`
	TestIDs       []string `json:"test_ids"`
	Evidence      []string `json:"evidence"`
}

// Catalog is the authoritative set of requirements and acceptance gates read
// from the specification.
type Catalog struct {
	Requirements map[string]string
	Gates        map[string]string
}

// Issue is one manifest validation failure.
type Issue struct {
	Kind    string
	Message string
}

func (i Issue) String() string {
	return i.Kind + ": " + i.Message
}

// LoadManifest decodes one manifest and rejects unknown JSON fields or trailing
// JSON values so typographical errors cannot silently weaken coverage checks.
func LoadManifest(r io.Reader) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode coverage manifest: %w", err)
	}

	var trailing json.RawMessage
	err := decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("decode coverage manifest: multiple JSON values")
		}
		return Manifest{}, fmt.Errorf("decode coverage manifest trailing data: %w", err)
	}

	return manifest, nil
}

// ParseSpec extracts the numbered requirement and E2E gate tables from the
// specification. Duplicate IDs make the specification ambiguous and fail fast.
func ParseSpec(r io.Reader) (Catalog, error) {
	catalog := Catalog{
		Requirements: map[string]string{},
		Gates:        map[string]string{},
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if matches := requirementRowPattern.FindStringSubmatch(line); matches != nil {
			if err := addCatalogItem(catalog.Requirements, matches[1], matches[2], "requirement"); err != nil {
				return Catalog{}, err
			}
			continue
		}
		if matches := gateRowPattern.FindStringSubmatch(line); matches != nil {
			if err := addCatalogItem(catalog.Gates, matches[1], matches[2], "gate"); err != nil {
				return Catalog{}, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Catalog{}, fmt.Errorf("scan specification: %w", err)
	}
	if len(catalog.Requirements) == 0 {
		return Catalog{}, errors.New("specification contains no numbered requirements")
	}
	if len(catalog.Gates) == 0 {
		return Catalog{}, errors.New("specification contains no E2E gates")
	}

	return catalog, nil
}

func addCatalogItem(items map[string]string, id, priority, kind string) error {
	if previous, exists := items[id]; exists {
		return fmt.Errorf("duplicate %s ID %q with priorities %s and %s", kind, id, previous, priority)
	}
	items[id] = priority
	return nil
}

// Validate compares a manifest with the authoritative specification catalog.
// It returns every independent issue in deterministic order.
func Validate(manifest Manifest, catalog Catalog) []Issue {
	issues := make([]Issue, 0)
	if manifest.SchemaVersion != ManifestSchemaVersion {
		issues = append(issues, Issue{
			Kind:    "schema_version",
			Message: fmt.Sprintf("got %d, want %d", manifest.SchemaVersion, ManifestSchemaVersion),
		})
	}
	if strings.TrimSpace(manifest.Source) == "" {
		issues = append(issues, Issue{Kind: "missing_field", Message: "source is required"})
	}
	if manifest.Requirements == nil {
		issues = append(issues, Issue{Kind: "missing_field", Message: "requirements must be a JSON array"})
	}

	seen := make(map[string]struct{}, len(manifest.Requirements))
	for index, requirement := range manifest.Requirements {
		id := strings.TrimSpace(requirement.RequirementID)
		if id == "" {
			issues = append(issues, Issue{
				Kind:    "missing_field",
				Message: fmt.Sprintf("requirements[%d].requirement_id is required", index),
			})
			continue
		}
		if _, exists := seen[id]; exists {
			issues = append(issues, Issue{Kind: "duplicate_requirement", Message: id})
			continue
		}
		seen[id] = struct{}{}

		specPriority, known := catalog.Requirements[id]
		if !known {
			issues = append(issues, Issue{Kind: "unknown_requirement", Message: id})
			validateRequirementFields(requirement, catalog, &issues)
			continue
		}
		if specPriority == "P2" {
			issues = append(issues, Issue{Kind: "p2_in_gate", Message: id + " is a P2 requirement"})
		}
		if requirement.Priority != specPriority {
			issues = append(issues, Issue{
				Kind:    "priority_mismatch",
				Message: fmt.Sprintf("%s declares %s, specification declares %s", id, requirement.Priority, specPriority),
			})
		}
		validateRequirementFields(requirement, catalog, &issues)
	}

	for id, priority := range catalog.Requirements {
		if priority == "P2" {
			continue
		}
		if _, exists := seen[id]; !exists {
			issues = append(issues, Issue{Kind: "missing_requirement", Message: id})
		}
	}

	slices.SortFunc(issues, func(left, right Issue) int {
		if byKind := cmp.Compare(left.Kind, right.Kind); byKind != 0 {
			return byKind
		}
		return cmp.Compare(left.Message, right.Message)
	})
	return issues
}

func validateRequirementFields(requirement Requirement, catalog Catalog, issues *[]Issue) {
	id := strings.TrimSpace(requirement.RequirementID)
	if strings.TrimSpace(requirement.Owner) == "" {
		*issues = append(*issues, Issue{Kind: "missing_field", Message: id + ".owner is required"})
	}
	switch requirement.Status {
	case StatusPlanned:
	case StatusCovered:
		if len(requirement.TestIDs) == 0 {
			*issues = append(*issues, Issue{
				Kind:    "missing_field",
				Message: id + ".test_ids must not be empty when status is covered",
			})
		}
	default:
		*issues = append(*issues, Issue{
			Kind:    "invalid_status",
			Message: fmt.Sprintf("%s has status %q", id, requirement.Status),
		})
	}
	if requirement.TestIDs == nil {
		*issues = append(*issues, Issue{Kind: "missing_field", Message: id + ".test_ids must be a JSON array"})
	}
	if requirement.Evidence == nil {
		*issues = append(*issues, Issue{Kind: "missing_field", Message: id + ".evidence must be a JSON array"})
	} else if len(requirement.Evidence) == 0 {
		*issues = append(*issues, Issue{Kind: "missing_field", Message: id + ".evidence must include a path or planned marker"})
	}

	testIDs := map[string]struct{}{}
	for _, testID := range requirement.TestIDs {
		if _, exists := testIDs[testID]; exists {
			*issues = append(*issues, Issue{
				Kind:    "duplicate_test",
				Message: fmt.Sprintf("%s lists %s more than once", id, testID),
			})
			continue
		}
		testIDs[testID] = struct{}{}
		if !e2eIDPattern.MatchString(testID) {
			continue
		}
		priority, known := catalog.Gates[testID]
		if !known {
			*issues = append(*issues, Issue{
				Kind:    "unknown_gate",
				Message: fmt.Sprintf("%s references %s", id, testID),
			})
			continue
		}
		if priority == "P2" {
			*issues = append(*issues, Issue{
				Kind:    "p2_in_gate",
				Message: fmt.Sprintf("%s references P2 gate %s", id, testID),
			})
		}
	}
}
