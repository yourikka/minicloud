package coverage_test

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	coveragecheck "github.com/yourikka/minicloud/internal/coverage"
)

func TestRepositoryManifest(t *testing.T) {
	t.Parallel()

	specData, err := os.ReadFile("../../MiniCloud-Spec-v1.0.md")
	if err != nil {
		t.Fatalf("read specification: %v", err)
	}

	catalog, err := coveragecheck.ParseSpec(bytes.NewReader(specData))
	if err != nil {
		t.Fatalf("parse specification: %v", err)
	}
	if got, want := len(catalog.Requirements), 261; got != want {
		t.Fatalf("requirement catalog size = %d, want %d", got, want)
	}
	if got, want := len(catalog.Gates), 45; got != want {
		t.Fatalf("gate catalog size = %d, want %d", got, want)
	}

	manifestData, err := os.ReadFile("../../coverage/requirements.json")
	if err != nil {
		t.Fatalf("read coverage manifest: %v", err)
	}

	manifest, err := coveragecheck.LoadManifest(bytes.NewReader(manifestData))
	if err != nil {
		t.Fatalf("load coverage manifest: %v", err)
	}
	if got, want := len(manifest.Requirements), 247; got != want {
		t.Fatalf("manifest size = %d, want %d", got, want)
	}

	priorities := map[string]int{}
	for _, requirement := range manifest.Requirements {
		priorities[requirement.Priority]++
	}
	if got, want := priorities["P0"], 170; got != want {
		t.Errorf("P0 requirement count = %d, want %d", got, want)
	}
	if got, want := priorities["P1"], 77; got != want {
		t.Errorf("P1 requirement count = %d, want %d", got, want)
	}

	if issues := coveragecheck.Validate(manifest, catalog); len(issues) != 0 {
		t.Fatalf("repository manifest validation issues: %v", issues)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*coveragecheck.Manifest)
		wantKinds []string
	}{
		{
			name: "valid planned manifest",
			mutate: func(*coveragecheck.Manifest) {
			},
			wantKinds: []string{},
		},
		{
			name: "duplicate requirement",
			mutate: func(manifest *coveragecheck.Manifest) {
				manifest.Requirements = append(manifest.Requirements, manifest.Requirements[0])
			},
			wantKinds: []string{"duplicate_requirement"},
		},
		{
			name: "unknown requirement",
			mutate: func(manifest *coveragecheck.Manifest) {
				manifest.Requirements = append(manifest.Requirements, plannedRequirement("FN-999", "P0"))
			},
			wantKinds: []string{"unknown_requirement"},
		},
		{
			name: "missing P1 requirement",
			mutate: func(manifest *coveragecheck.Manifest) {
				manifest.Requirements = manifest.Requirements[:1]
			},
			wantKinds: []string{"missing_requirement"},
		},
		{
			name: "P2 requirement in manifest",
			mutate: func(manifest *coveragecheck.Manifest) {
				manifest.Requirements = append(manifest.Requirements, plannedRequirement("ABI-006", "P2"))
			},
			wantKinds: []string{"p2_in_gate"},
		},
		{
			name: "P2 E2E gate referenced",
			mutate: func(manifest *coveragecheck.Manifest) {
				manifest.Requirements[0].TestIDs = append(manifest.Requirements[0].TestIDs, "E2E-023")
			},
			wantKinds: []string{"p2_in_gate"},
		},
		{
			name: "unknown E2E gate referenced",
			mutate: func(manifest *coveragecheck.Manifest) {
				manifest.Requirements[0].TestIDs = append(manifest.Requirements[0].TestIDs, "E2E-999")
			},
			wantKinds: []string{"unknown_gate"},
		},
		{
			name: "priority mismatch",
			mutate: func(manifest *coveragecheck.Manifest) {
				manifest.Requirements[0].Priority = "P1"
			},
			wantKinds: []string{"priority_mismatch"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			manifest := validManifest()
			test.mutate(&manifest)
			issues := coveragecheck.Validate(manifest, testCatalog())
			gotKinds := make([]string, 0, len(issues))
			for _, issue := range issues {
				gotKinds = append(gotKinds, issue.Kind)
			}
			if !slices.Equal(gotKinds, test.wantKinds) {
				t.Fatalf("issue kinds = %v, want %v; issues: %v", gotKinds, test.wantKinds, issues)
			}
		})
	}
}

func TestParseSpec_RejectsDuplicateIDs(t *testing.T) {
	t.Parallel()

	specification := strings.NewReader(`
| FN-001 | P0 | first |
| FN-001 | P1 | duplicate |
| E2E-001 | P0 | gate |
`)
	_, err := coveragecheck.ParseSpec(specification)
	if err == nil || !strings.Contains(err.Error(), "duplicate requirement ID") {
		t.Fatalf("ParseSpec() error = %v, want duplicate requirement error", err)
	}
}

func TestLoadManifest_RejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
	}{
		{
			name: "unknown field",
			json: `{"schema_version":1,"source":"spec","requirements":[],"typo":true}`,
		},
		{
			name: "trailing value",
			json: `{"schema_version":1,"source":"spec","requirements":[]} {}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := coveragecheck.LoadManifest(strings.NewReader(test.json)); err == nil {
				t.Fatal("LoadManifest() error = nil, want error")
			}
		})
	}
}

func validManifest() coveragecheck.Manifest {
	return coveragecheck.Manifest{
		SchemaVersion: coveragecheck.ManifestSchemaVersion,
		Source:        "MiniCloud-Spec-v1.0.md",
		Requirements: []coveragecheck.Requirement{
			plannedRequirement("FN-001", "P0"),
			plannedRequirement("FN-002", "P1"),
		},
	}
}

func plannedRequirement(id, priority string) coveragecheck.Requirement {
	return coveragecheck.Requirement{
		RequirementID: id,
		Priority:      priority,
		Owner:         "test-owner",
		Status:        coveragecheck.StatusPlanned,
		TestIDs:       []string{"E2E-001"},
		Evidence:      []string{"planned: pending evidence"},
	}
}

func testCatalog() coveragecheck.Catalog {
	return coveragecheck.Catalog{
		Requirements: map[string]string{
			"FN-001":  "P0",
			"FN-002":  "P1",
			"ABI-006": "P2",
		},
		Gates: map[string]string{
			"E2E-001": "P0",
			"E2E-023": "P2",
		},
	}
}
