package controllers

import (
	"context"

	"github.com/dinoallo/labring-sigs-harbor/internal/harbor"
)

// HarborAPIClient defines the interface for Harbor API operations
// used by the reconciler. Using an interface allows unit testing
// without a real Harbor instance.
type HarborAPIClient interface {
	GetProjectByName(ctx context.Context, name string) (*harbor.Project, error)
	CreateProject(ctx context.Context, spec harbor.ProjectSpec) (int64, error)
	UpdateProject(ctx context.Context, projectID int64, spec harbor.ProjectSpec) error
	CreateRobot(ctx context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error)
	RefreshRobotSecret(ctx context.Context, robotID int64, secret string) error
	DeleteProjectRobot(ctx context.Context, projectID, robotID int64) error
	DeleteProject(ctx context.Context, projectID int64) error
}

// Compile-time assertion that *harbor.Client implements HarborAPIClient.
var _ HarborAPIClient = (*harbor.Client)(nil)
