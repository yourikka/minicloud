//go:build integration

package localcontroller

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/validator"
)

func TestControllerAdmissionWithRealValidator(t *testing.T) {
	root := repositoryRoot(t)
	binDirectory := t.TempDir()
	validatorPath := filepath.Join(binDirectory, executableName("minicloud-validator"))
	build(t, root, nil, validatorPath, "./cmd/minicloud-validator")
	wasmPath := filepath.Join(binDirectory, "echo.wasm")
	build(t, root, []string{"GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0"}, wasmPath, "./test/fixtures/wasm/echo")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", wasmPath, err)
	}

	store, err := artifact.Open(artifact.Config{Root: t.TempDir(), MaxArtifactBytes: int64(len(wasm))})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	validatorClient, err := validator.New(validator.Config{
		Command:          validatorPath,
		TempRoot:         filepath.Join(t.TempDir(), "validator-temp"),
		Deadline:         10 * time.Second,
		MaxConcurrent:    1,
		MaxArtifactBytes: int64(len(wasm)),
	})
	if err != nil {
		t.Fatalf("validator.New() error = %v", err)
	}
	controller, err := New(Config{Artifacts: store, Validator: validatorClient})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	createdFunction, err := controller.CreateFunction(context.Background(), CreateFunctionInput{
		Name:       "real-validator",
		AuthPolicy: controlplane.AuthPolicyPublic,
	})
	if err != nil {
		t.Fatalf("CreateFunction() error = %v", err)
	}

	validDigest := digest.Sum(wasm)
	if _, err := controller.PutArtifact(context.Background(), validDigest, bytes.NewReader(wasm)); err != nil {
		t.Fatalf("PutArtifact(valid) error = %v", err)
	}
	ready, err := controller.CreateVersion(context.Background(), testVersionInput(createdFunction.Function.ID, validDigest))
	if err != nil {
		t.Fatalf("CreateVersion(valid) error = %v", err)
	}
	if ready.Version.State != model.VersionReady || ready.Deployment == nil {
		t.Fatalf("CreateVersion(valid) = %+v, want ready Version and Deployment", ready)
	}
	route, updatedFunction, err := controller.PublishRoute(context.Background(), PublishRouteInput{
		FunctionID:                  createdFunction.Function.ID,
		VersionID:                   ready.Version.VersionID,
		ExpectedActiveRouteRevision: 0,
	})
	if err != nil {
		t.Fatalf("PublishRoute() error = %v", err)
	}
	if !route.Enabled || route.RouteRevision != 1 || len(route.Targets) != 1 ||
		route.Targets[0].VersionID != ready.Version.VersionID ||
		route.Targets[0].WeightBasisPoints != model.TotalRouteWeightBasisPoints ||
		updatedFunction.ActiveRouteRevision != 1 {
		t.Fatalf("PublishRoute() = (%+v, %+v), want published ready target", route, updatedFunction)
	}

	invalid := append([]byte(nil), wasm...)
	invalid[0] = 0xff
	invalidDigest := digest.Sum(invalid)
	if _, err := controller.PutArtifact(context.Background(), invalidDigest, bytes.NewReader(invalid)); err != nil {
		t.Fatalf("PutArtifact(invalid) error = %v", err)
	}
	failed, err := controller.CreateVersion(context.Background(), testVersionInput(createdFunction.Function.ID, invalidDigest))
	if err != nil {
		t.Fatalf("CreateVersion(invalid) error = %v", err)
	}
	if failed.Version.State != model.VersionFailed || failed.Deployment != nil || failed.Version.ValidationError == nil {
		t.Fatalf("CreateVersion(invalid) = %+v, want failed Version without Deployment", failed)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	return root
}

func build(t *testing.T, root string, extraEnvironment []string, output, packagePath string) {
	t.Helper()
	command := exec.Command("go", "build", "-o", output, packagePath)
	command.Dir = root
	command.Env = append(os.Environ(), extraEnvironment...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build %s error = %v\n%s", packagePath, err, output)
	}
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
