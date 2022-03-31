package cloudrun

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/access"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/debug"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/label"
	sErrors "github.com/GoogleContainerTools/skaffold/pkg/skaffold/errors"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/gcp"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/graph"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/log"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/output"
	v1 "github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest/v1"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/status"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/sync"
	"github.com/GoogleContainerTools/skaffold/proto/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/run/v1"

	k8syaml "sigs.k8s.io/yaml"
)

type Deployer struct {
	logger log.Logger

	Project string
	Region  string
	Service string

	ServiceConfigFile string
}

func NewDeployer(ctx context.Context, labeller *label.DefaultLabeller, crDeploy *v1.CloudRunDeploy) (*Deployer, error) {
	return &Deployer{Project: crDeploy.ProjectID,
		Service:           crDeploy.Service,
		Region:            crDeploy.Region,
		ServiceConfigFile: crDeploy.ConfigFile,
		logger:            &log.NoopLogger{},
	}, nil
}

// Do the deploy
func (d *Deployer) Deploy(ctx context.Context, out io.Writer, artifacts []graph.Artifact) error {
	return d.deployToCloudRun(ctx, out, artifacts)
}

// Files that would trigger a redeploy
func (d *Deployer) Dependencies() ([]string, error) {
	return []string{d.ServiceConfigFile}, nil
}

// Delete the created dev service
func (d *Deployer) Cleanup(ctx context.Context, out io.Writer, dryRun bool) error {
	return d.deleteRunService(ctx, out, dryRun)
}

// This writes out the k8s configs, we may want to support this with service configs in the future
// but it's not being implemented now
func (d *Deployer) Render(context.Context, io.Writer, []graph.Artifact, bool, string) error {
	return nil
}

func (d *Deployer) GetDebugger() debug.Debugger {
	return &debug.NoopDebugger{}
}

func (d *Deployer) GetLogger() log.Logger {
	return d.logger
}

func (d *Deployer) GetAccessor() access.Accessor {
	return &access.NoopAccessor{}
}

func (d *Deployer) GetSyncer() sync.Syncer {
	return &sync.NoopSyncer{}
}

func (d *Deployer) TrackBuildArtifacts([]graph.Artifact) {

}

func (d *Deployer) RegisterLocalImages([]graph.Artifact) {

}

func (d *Deployer) GetStatusMonitor() status.Monitor {
	return &status.NoopMonitor{}
}

func (d *Deployer) deployToCloudRun(ctx context.Context, out io.Writer, artifacts []graph.Artifact) error {
	if len(artifacts) > 1 {
		return sErrors.NewError(fmt.Errorf("Too many artifacts"), &proto.ActionableErr{
			Message: "Cloud Run only supports a single image",
			ErrCode: proto.StatusCode_DEPLOY_CANCELLED,
		})
	}
	artifact := artifacts[0]
	if !strings.Contains(artifact.ImageName, "gcr.io") && strings.Contains(artifact.ImageName, "docker.pkg.dev") {
		// TODO: Cloud run requires the image to be stored in Google Container Registry or Artifact Registry. If it's not there, upload it
		return sErrors.NewError(fmt.Errorf("Unsupported Repository"), &proto.ActionableErr{
			Message: "Cloud Run artifacts must be in GCR or Artifact Registry",
			ErrCode: proto.StatusCode_DEPLOY_CANCELLED,
		})
	}
	crclient, err := run.NewService(ctx, gcp.ClientOptions(ctx)...)
	if err != nil {
		return sErrors.NewError(fmt.Errorf("Unable to create Cloud Run Client"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_GET_CLOUD_RUN_CLIENT_ERR,
		})
	}
	dat, err := os.ReadFile(d.ServiceConfigFile)
	if err != nil {
		return sErrors.NewError(fmt.Errorf("Unable to read Cloud Run Config"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	service := &run.Service{}
	if err = k8syaml.Unmarshal(dat, service); err != nil {
		return sErrors.NewError(fmt.Errorf("Unable to unmarshal Cloud Run Service config"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	service.Metadata.Name = d.Service
	service.Metadata.Namespace = d.Project
	service.Spec.Template.Spec.Containers[0].Image = artifact.ImageName
	serviceJson, err := service.MarshalJSON()
	output.Blue.Fprintf(out, "Deploying Cloud Run service:\n %v", string(serviceJson))
	parent := fmt.Sprintf("projects/%s/locations/%s", d.Project, d.Region)

	sName := fmt.Sprintf("%s/services/%s", parent, d.Service)
	getCall := crclient.Projects.Locations.Services.Get(sName)
	_, err = getCall.Do()

	if err != nil {
		gErr, ok := err.(*googleapi.Error)
		if !ok || gErr.Code != http.StatusNotFound {
			return sErrors.NewError(fmt.Errorf("Error checking Cloud Run State"), &proto.ActionableErr{
				Message: err.Error(),
				ErrCode: proto.StatusCode_DEPLOY_CANCELLED,
			})
		}
		// This is a new service, we need to create it
		createCall := crclient.Projects.Locations.Services.Create(parent, service)
		_, err = createCall.Do()
	} else {
		replaceCall := crclient.Projects.Locations.Services.ReplaceService(sName, service)
		_, err = replaceCall.Do()
	}
	if err != nil {
		return sErrors.NewError(fmt.Errorf("Error deploying Cloud Run Service"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_CLOUD_RUN_UPDATE_SERVICE_ERR,
		})
	}
	// register status monitor
	return nil
}

func (d *Deployer) deleteRunService(ctx context.Context, out io.Writer, dryRun bool) error {

	parent := fmt.Sprintf("projects/%s/locations/%s", d.Project, d.Region)
	sName := fmt.Sprintf("%s/services/%s", parent, d.Service)
	if dryRun {
		output.Yellow.Fprintln(out, sName)
		return nil
	}
	crclient, err := run.NewService(ctx, gcp.ClientOptions(ctx)...)
	if err != nil {
		return sErrors.NewError(fmt.Errorf("Unable to create Cloud Run Client"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_GET_CLOUD_RUN_CLIENT_ERR,
		})
	}
	delCall := crclient.Projects.Locations.Services.Delete(sName)
	_, err = delCall.Do()
	if err != nil {
		return sErrors.NewError(fmt.Errorf("Unable to delete Cloud Run Service"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_CLOUD_RUN_DELETE_SERVICE_ERR,
		})
	}
	return nil
}
