package localcontroller

import (
	"context"
	"errors"
	"fmt"

	"github.com/yourikka/minicloud/internal/controlplane"
)

// CommitAssignment persists one immutable Assignment intent before a caller
// may issue the corresponding Worker Prepare command.
func (c *Controller) CommitAssignment(
	ctx context.Context,
	input CommitAssignmentInput,
) (controlplane.AssignmentRecord, error) {
	if err := checkContext(ctx); err != nil {
		return controlplane.AssignmentRecord{}, err
	}
	if c == nil || c.assignments == nil {
		return controlplane.AssignmentRecord{}, errors.New("committing assignment: local assignment store is required")
	}
	command, err := c.nextCommand()
	if err != nil {
		return controlplane.AssignmentRecord{}, err
	}
	record, err := c.assignments.Install(controlplane.InstallAssignmentCommand{
		FunctionID:              input.FunctionID,
		Placement:               input.Placement,
		IfNoneMatch:             true,
		ExpectedScalingRevision: input.ExpectedScalingRevision,
		AppliedIndex:            command.AppliedIndex,
		UpdatedAt:               command.At,
	})
	if err != nil {
		return controlplane.AssignmentRecord{}, fmt.Errorf("committing assignment: %w", err)
	}
	return record, nil
}

// CancelAssignment commits cancellation before the serving view and Worker
// reconciler remove the corresponding Replica.
func (c *Controller) CancelAssignment(
	ctx context.Context,
	input CancelAssignmentInput,
) (controlplane.AssignmentRecord, error) {
	if err := checkContext(ctx); err != nil {
		return controlplane.AssignmentRecord{}, err
	}
	if c == nil || c.assignments == nil {
		return controlplane.AssignmentRecord{}, errors.New("cancelling assignment: local assignment store is required")
	}
	command, err := c.nextCommand()
	if err != nil {
		return controlplane.AssignmentRecord{}, err
	}
	record, err := c.assignments.Cancel(controlplane.CancelAssignmentCommand{
		AssignmentID:             input.AssignmentID,
		ExpectedResourceRevision: input.ExpectedResourceRevision,
		AppliedIndex:             command.AppliedIndex,
		UpdatedAt:                command.At,
	})
	if err != nil {
		return controlplane.AssignmentRecord{}, fmt.Errorf("cancelling assignment: %w", err)
	}
	return record, nil
}

// GetAssignment returns one committed Assignment intent.
func (c *Controller) GetAssignment(
	ctx context.Context,
	assignmentID string,
) (controlplane.AssignmentRecord, error) {
	if err := checkContext(ctx); err != nil {
		return controlplane.AssignmentRecord{}, err
	}
	if c == nil || c.assignments == nil {
		return controlplane.AssignmentRecord{}, errors.New("getting assignment: local assignment store is required")
	}
	record, err := c.assignments.Get(assignmentID)
	if err != nil {
		return controlplane.AssignmentRecord{}, fmt.Errorf("getting assignment: %w", err)
	}
	return record, nil
}

// ListAssignments returns every retained Assignment intent in stable order.
func (c *Controller) ListAssignments(ctx context.Context) ([]controlplane.AssignmentRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.assignments == nil {
		return nil, errors.New("listing assignments: local assignment store is required")
	}
	return c.assignments.Snapshot(), nil
}
