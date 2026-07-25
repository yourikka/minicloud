package localcontroller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/validator"
	validatorprotocol "github.com/yourikka/minicloud/internal/validator/protocol"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

const defaultMaxLogBytes = int64(256 << 10)

// CreateVersion records an Uploaded Version, starts its persisted validation
// fence, and synchronously drives the first admission attempt. Infrastructure
// failures return the current Validating Version for later recovery.
func (c *Controller) CreateVersion(ctx context.Context, input CreateVersionInput) (AdmissionResult, error) {
	if err := checkContext(ctx); err != nil {
		return AdmissionResult{}, err
	}
	if c == nil || c.releases == nil {
		return AdmissionResult{}, errors.New("creating version: local controller release store is required")
	}
	if input.ResourceRequest.Timeout%time.Millisecond != 0 {
		return AdmissionResult{}, problem.Invalid("resource_request.timeout", "must use whole milliseconds")
	}
	artifactInfo, err := c.verifyArtifact(ctx, input.ArtifactDigest)
	if err != nil {
		return AdmissionResult{}, err
	}
	versionID, err := c.newID("version")
	if err != nil {
		return AdmissionResult{}, err
	}
	validationID, err := c.newID("validation")
	if err != nil {
		return AdmissionResult{}, err
	}
	createCommand, err := c.nextCommand()
	if err != nil {
		return AdmissionResult{}, err
	}
	startCommand, err := c.nextCommand()
	if err != nil {
		return AdmissionResult{}, err
	}
	version := model.Version{
		Metadata: model.Metadata{
			ID:               versionID,
			Namespace:        model.DefaultNamespace,
			CreatedAt:        createCommand.At,
			UpdatedAt:        createCommand.At,
			CreatedRaftIndex: createCommand.AppliedIndex,
			ResourceRevision: 1,
		},
		FunctionID:            input.FunctionID,
		VersionID:             versionID,
		ArtifactDigest:        input.ArtifactDigest,
		ManifestDigest:        input.ManifestDigest,
		ArtifactSize:          artifactInfo.Size,
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: wasmprofile.FeatureProfile,
		Toolchain:             input.Toolchain,
		AdmissionEpoch:        1,
		ResourceRequest:       input.ResourceRequest,
		RequestedCapabilities: cloneCapabilities(input.RequestedCapabilities),
		State:                 model.VersionUploaded,
	}
	created, err := c.releases.CreateVersion(controlplane.CreateVersionCommand{
		IfNoneMatch:  true,
		AppliedIndex: createCommand.AppliedIndex,
		Version:      version,
	})
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("creating version: %w", err)
	}
	result := AdmissionResult{Version: created}
	if !c.claimValidation(created.VersionID) {
		return result, errors.New("creating version: validation is already active")
	}
	defer c.releaseValidation(created.VersionID)

	started, err := c.releases.StartValidation(controlplane.StartValidationCommand{
		VersionID:                created.VersionID,
		ExpectedResourceRevision: created.ResourceRevision,
		ValidationID:             validationID,
		UpdatedAt:                startCommand.At,
		AppliedIndex:             startCommand.AppliedIndex,
	})
	if err != nil {
		return result, fmt.Errorf("starting version validation: %w", err)
	}
	return c.admit(ctx, started, validationID)
}

// GetVersion returns one Version and the Deployment created with a successful
// Generation 1 admission result.
func (c *Controller) GetVersion(
	ctx context.Context,
	versionID string,
) (model.Version, *model.Deployment, error) {
	if err := checkContext(ctx); err != nil {
		return model.Version{}, nil, err
	}
	if c == nil || c.releases == nil {
		return model.Version{}, nil, errors.New("getting version: local controller release store is required")
	}
	version, deployment, err := c.releases.Get(versionID)
	if err != nil {
		return model.Version{}, nil, fmt.Errorf("getting version: %w", err)
	}
	return version, deployment, nil
}

// ResumePendingValidation retries persisted Validating Version fences without
// generating a new Validation ID. It processes no more than limit entries.
func (c *Controller) ResumePendingValidation(ctx context.Context, limit int) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if c == nil || c.releases == nil {
		return errors.New("resuming validation: local controller release store is required")
	}
	if limit < 1 {
		return problem.Invalid("limit", "must be greater than zero")
	}

	pending := c.releases.Snapshot().PendingValidations
	var failures []error
	processed := 0
	for _, attempt := range pending {
		if processed >= limit {
			break
		}
		if !c.claimValidation(attempt.VersionID) {
			continue
		}
		processed++
		func() {
			defer c.releaseValidation(attempt.VersionID)
			version, _, err := c.releases.Get(attempt.VersionID)
			if err != nil {
				failures = append(failures, fmt.Errorf("loading pending validation: %w", err))
				return
			}
			if version.State != model.VersionValidating {
				return
			}
			if _, err := c.admit(ctx, version, attempt.ValidationID); err != nil {
				failures = append(failures, fmt.Errorf("resuming pending validation: %w", err))
			}
		}()
		if err := ctx.Err(); err != nil {
			failures = append(failures, fmt.Errorf("resuming validation context: %w", err))
			break
		}
	}
	return errors.Join(failures...)
}

func (c *Controller) admit(
	ctx context.Context,
	version model.Version,
	validationID string,
) (AdmissionResult, error) {
	artifactFile, info, err := c.openArtifactForValidation(ctx, version)
	if err != nil {
		return c.currentAdmission(version.VersionID, err)
	}

	request := validationRequest(version, validationID)
	report, err := c.validator.Validate(ctx, request, artifactFile)
	if err != nil {
		if !isValidationLimit(err) {
			return c.currentAdmission(version.VersionID, fmt.Errorf("running validator: %w", err))
		}
		report = validationLimitReport(version, validationID, err)
	}
	if report.ArtifactDigest != info.Digest || report.ArtifactSize != info.Size {
		return c.currentAdmission(
			version.VersionID,
			errors.New("validator report did not match the verified artifact"),
		)
	}
	return c.completeAdmission(version, report)
}

func (c *Controller) completeAdmission(
	version model.Version,
	report validatorprotocol.Report,
) (AdmissionResult, error) {
	var deployment *model.Deployment
	if report.Valid {
		deploymentID, err := c.newID("deployment")
		if err != nil {
			return c.currentAdmission(version.VersionID, err)
		}
		command, err := c.nextCommand()
		if err != nil {
			return c.currentAdmission(version.VersionID, err)
		}
		deployment, err = initialDeployment(version, deploymentID, command)
		if err != nil {
			return c.currentAdmission(version.VersionID, err)
		}
		return c.applyCompletion(version, report, deployment, command)
	}

	command, err := c.nextCommand()
	if err != nil {
		return c.currentAdmission(version.VersionID, err)
	}
	return c.applyCompletion(version, report, nil, command)
}

func (c *Controller) applyCompletion(
	version model.Version,
	report validatorprotocol.Report,
	deployment *model.Deployment,
	command CommandMeta,
) (AdmissionResult, error) {
	completed, createdDeployment, err := c.releases.CompleteValidation(controlplane.CompleteValidationCommand{
		VersionID:                version.VersionID,
		ExpectedResourceRevision: version.ResourceRevision,
		Report:                   report,
		Deployment:               deployment,
		UpdatedAt:                command.At,
		AppliedIndex:             command.AppliedIndex,
	})
	if err != nil {
		return c.currentAdmission(version.VersionID, fmt.Errorf("completing version validation: %w", err))
	}
	return AdmissionResult{Version: completed, Deployment: createdDeployment}, nil
}

func (c *Controller) currentAdmission(versionID string, cause error) (AdmissionResult, error) {
	version, deployment, err := c.releases.Get(versionID)
	if err != nil {
		return AdmissionResult{}, errors.Join(cause, fmt.Errorf("loading current version: %w", err))
	}
	return AdmissionResult{Version: version, Deployment: deployment}, cause
}

func (c *Controller) verifyArtifact(ctx context.Context, expected digest.SHA256) (artifact.Info, error) {
	if c == nil || c.artifacts == nil {
		return artifact.Info{}, errors.New("verifying artifact: local controller artifact store is required")
	}
	file, info, err := c.artifacts.OpenVerified(ctx, expected)
	if err != nil {
		return artifact.Info{}, fmt.Errorf("verifying artifact: %w", err)
	}
	if file == nil {
		return artifact.Info{}, errors.New("verifying artifact: store returned a nil file")
	}
	closeErr := file.Close()
	if closeErr != nil {
		return artifact.Info{}, fmt.Errorf("verifying artifact: closing verified file: %w", closeErr)
	}
	if info.Digest != expected || info.Size < 1 {
		return artifact.Info{}, errors.New("verifying artifact: store returned inconsistent metadata")
	}
	return info, nil
}

func (c *Controller) openArtifactForValidation(
	ctx context.Context,
	version model.Version,
) (*os.File, artifact.Info, error) {
	if c == nil || c.artifacts == nil {
		return nil, artifact.Info{}, errors.New("opening artifact for validation: local controller artifact store is required")
	}
	file, info, err := c.artifacts.OpenVerified(ctx, version.ArtifactDigest)
	if err != nil {
		return nil, artifact.Info{}, fmt.Errorf("opening artifact for validation: %w", err)
	}
	if file == nil {
		return nil, artifact.Info{}, errors.New("opening artifact for validation: store returned a nil file")
	}
	if info.Digest == version.ArtifactDigest && info.Size == version.ArtifactSize {
		return file, info, nil
	}
	metadataErr := errors.New("opening artifact for validation: store returned inconsistent metadata")
	return nil, artifact.Info{}, errors.Join(metadataErr, file.Close())
}

func validationRequest(version model.Version, validationID string) validatorprotocol.Request {
	return validatorprotocol.Request{
		SchemaVersion:         validatorprotocol.SchemaVersion,
		ValidationID:          validationID,
		ArtifactDigest:        version.ArtifactDigest,
		ArtifactSize:          version.ArtifactSize,
		ABI:                   version.ABI,
		HostAPIProfile:        version.HostAPIProfile,
		RuntimeFeatureProfile: version.RuntimeFeatureProfile,
		RuntimeEngine:         validatorprotocol.EngineCompiler,
		MemoryLimitMiB:        version.ResourceRequest.MemoryMiB,
		RequestedCapabilities: cloneCapabilities(version.RequestedCapabilities),
	}
}

func isValidationLimit(err error) bool {
	return errors.Is(err, validator.ErrTimedOut) || errors.Is(err, validator.ErrOutputLimit)
}

func validationLimitReport(
	version model.Version,
	validationID string,
	err error,
) validatorprotocol.Report {
	reason := "validator_output_limit"
	message := "validator output exceeded the admission limit"
	if errors.Is(err, validator.ErrTimedOut) {
		reason = "validator_timeout"
		message = "module validation exceeded the admission limit"
	}
	return validatorprotocol.Report{
		SchemaVersion:         validatorprotocol.SchemaVersion,
		ValidationID:          validationID,
		Valid:                 false,
		Code:                  string(problem.CodeInvalidModule),
		Reason:                reason,
		Message:               message,
		ArtifactDigest:        version.ArtifactDigest,
		ArtifactSize:          version.ArtifactSize,
		RuntimeName:           validatorprotocol.RuntimeName,
		RuntimeVersion:        validatorprotocol.RuntimeVersion,
		RuntimeFeatureProfile: validatorprotocol.FeatureProfile,
		RuntimeEngine:         validatorprotocol.EngineCompiler,
		Imports:               []validatorprotocol.Import{},
		Exports:               []string{},
	}
}

func initialDeployment(
	version model.Version,
	deploymentID string,
	command CommandMeta,
) (*model.Deployment, error) {
	limits := model.ResourceLimits{
		Timeout:        version.ResourceRequest.Timeout,
		MemoryMiB:      version.ResourceRequest.MemoryMiB,
		MaxInputBytes:  version.ResourceRequest.MaxInputBytes,
		MaxOutputBytes: version.ResourceRequest.MaxOutputBytes,
		MaxLogBytes:    defaultMaxLogBytes,
	}
	capabilities := cloneCapabilities(version.RequestedCapabilities)
	policy := model.EffectivePolicy{
		VersionID:             version.VersionID,
		AdmissionEpoch:        version.AdmissionEpoch,
		DeploymentGeneration:  1,
		ArtifactDigest:        version.ArtifactDigest,
		ArtifactSize:          version.ArtifactSize,
		ABI:                   version.ABI,
		HostAPIProfile:        version.HostAPIProfile,
		RuntimeFeatureProfile: version.RuntimeFeatureProfile,
		ResourceLimits:        limits,
		MaxConcurrency:        version.ResourceRequest.MaxConcurrency,
		GrantedCapabilities:   capabilities,
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		return nil, fmt.Errorf("calculating initial effective policy: %w", err)
	}
	return &model.Deployment{
		Metadata: model.Metadata{
			ID:               deploymentID,
			Namespace:        version.Namespace,
			CreatedAt:        command.At,
			UpdatedAt:        command.At,
			CreatedRaftIndex: command.AppliedIndex,
			ResourceRevision: 1,
		},
		VersionID:             version.VersionID,
		Generation:            1,
		ScalingRevision:       1,
		ResourceLimits:        limits,
		GrantedCapabilities:   capabilities,
		EffectivePolicyDigest: policyDigest,
		MinReplicas:           1,
		MaxReplicas:           1,
		DesiredReplicas:       1,
		ReadyReplicas:         0,
		TargetConcurrency:     version.ResourceRequest.MaxConcurrency,
		ScalingMode:           model.ScalingManual,
		DesiredPhase:          model.DeploymentActive,
	}, nil
}

func cloneCapabilities(source []model.CapabilityRequest) []model.CapabilityRequest {
	capabilities := make([]model.CapabilityRequest, len(source))
	copy(capabilities, source)
	return capabilities
}
