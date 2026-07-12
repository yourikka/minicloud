package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	coveragecheck "github.com/yourikka/minicloud/internal/coverage"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("coveragecheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	specPath := flags.String("spec", "MiniCloud-Spec-v1.0.md", "path to the MiniCloud specification")
	manifestPath := flags.String("manifest", "coverage/requirements.json", "path to the coverage manifest")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	specData, err := os.ReadFile(*specPath)
	if err != nil {
		fmt.Fprintf(stderr, "coveragecheck: read specification: %v\n", err)
		return 1
	}

	catalog, err := coveragecheck.ParseSpec(bytes.NewReader(specData))
	if err != nil {
		fmt.Fprintf(stderr, "coveragecheck: %v\n", err)
		return 1
	}

	manifestData, err := os.ReadFile(*manifestPath)
	if err != nil {
		fmt.Fprintf(stderr, "coveragecheck: read manifest: %v\n", err)
		return 1
	}

	manifest, err := coveragecheck.LoadManifest(bytes.NewReader(manifestData))
	if err != nil {
		fmt.Fprintf(stderr, "coveragecheck: %v\n", err)
		return 1
	}

	issues := coveragecheck.Validate(manifest, catalog)
	for _, issue := range issues {
		fmt.Fprintf(stderr, "coveragecheck: %s\n", issue)
	}
	if len(issues) > 0 {
		return 1
	}

	return 0
}
