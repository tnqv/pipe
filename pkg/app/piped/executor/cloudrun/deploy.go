// Copyright 2020 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudrun

import (
	"context"

	provider "github.com/pipe-cd/pipe/pkg/app/piped/cloudprovider/cloudrun"
	"github.com/pipe-cd/pipe/pkg/app/piped/deploysource"
	"github.com/pipe-cd/pipe/pkg/app/piped/executor"
	"github.com/pipe-cd/pipe/pkg/config"
	"github.com/pipe-cd/pipe/pkg/model"
)

type deployExecutor struct {
	executor.Input

	deploySource      *deploysource.DeploySource
	deployCfg         *config.CloudRunDeploymentSpec
	cloudProviderName string
	cloudProviderCfg  *config.CloudProviderCloudRunConfig
}

func (e *deployExecutor) Execute(sig executor.StopSignal) model.StageStatus {
	ctx := sig.Context()
	ds, err := e.TargetDSP.GetReadOnly(ctx, e.LogPersister)
	if err != nil {
		e.LogPersister.Errorf("Failed to prepare target deploy source data (%v)", err)
		return model.StageStatus_STAGE_FAILURE
	}

	e.deploySource = ds
	e.deployCfg = ds.DeploymentConfig.CloudRunDeploymentSpec
	if e.deployCfg == nil {
		e.LogPersister.Error("Malformed deployment configuration: missing CloudRunDeploymentSpec")
		return model.StageStatus_STAGE_FAILURE
	}

	var found bool
	e.cloudProviderName, e.cloudProviderCfg, found = findCloudProvider(&e.Input)
	if !found {
		return model.StageStatus_STAGE_FAILURE
	}

	var (
		originalStatus = e.Stage.Status
		status         model.StageStatus
	)

	switch model.Stage(e.Stage.Name) {
	case model.StageCloudRunSync:
		status = e.ensureSync(ctx)

	case model.StageCloudRunPromote:
		status = e.ensurePromote(ctx)

	default:
		e.LogPersister.Errorf("Unsupported stage %s for cloudrun application", e.Stage.Name)
		return model.StageStatus_STAGE_FAILURE
	}

	return executor.DetermineStageStatus(sig.Signal(), originalStatus, status)
}

func (e *deployExecutor) ensureSync(ctx context.Context) model.StageStatus {
	sm, ok := loadServiceManifest(&e.Input, e.deployCfg.Input.ServiceManifestFile, e.deploySource)
	if !ok {
		return model.StageStatus_STAGE_FAILURE
	}

	revision, ok := decideRevisionName(&e.Input, sm, e.Deployment.Trigger.Commit.Hash)
	if !ok {
		return model.StageStatus_STAGE_FAILURE
	}

	traffics := []provider.RevisionTraffic{
		{
			RevisionName: revision,
			Percent:      100,
		},
	}
	if !configureServiceManifest(&e.Input, sm, revision, traffics) {
		return model.StageStatus_STAGE_FAILURE
	}

	if !apply(ctx, &e.Input, e.cloudProviderName, e.cloudProviderCfg, sm) {
		return model.StageStatus_STAGE_FAILURE
	}

	return model.StageStatus_STAGE_SUCCESS
}

func (e *deployExecutor) ensurePromote(ctx context.Context) model.StageStatus {
	options := e.StageConfig.CloudRunPromoteStageOptions
	if options == nil {
		e.LogPersister.Errorf("Malformed configuration for stage %s", e.Stage.Name)
		return model.StageStatus_STAGE_FAILURE
	}

	// Loaded the last deployed data.
	if e.Deployment.RunningCommitHash == "" {
		e.LogPersister.Errorf("Unable to determine the last deployed commit")
		return model.StageStatus_STAGE_FAILURE
	}

	runningDS, err := e.RunningDSP.GetReadOnly(ctx, e.LogPersister)
	if err != nil {
		e.LogPersister.Errorf("Failed to prepare running deploy source data (%v)", err)
		return model.StageStatus_STAGE_FAILURE
	}

	runningDeployCfg := runningDS.DeploymentConfig.CloudRunDeploymentSpec
	if runningDeployCfg == nil {
		e.LogPersister.Error("Malformed deployment configuration in running commit: missing CloudRunDeploymentSpec")
		return model.StageStatus_STAGE_FAILURE
	}

	lastDeployedSM, ok := loadServiceManifest(&e.Input, runningDeployCfg.Input.ServiceManifestFile, runningDS)
	if !ok {
		return model.StageStatus_STAGE_FAILURE
	}

	lastDeployedRevision, ok := decideRevisionName(&e.Input, lastDeployedSM, e.Deployment.RunningCommitHash)
	if !ok {
		return model.StageStatus_STAGE_FAILURE
	}

	// Load the service manifest at the target commit.
	sm, ok := loadServiceManifest(&e.Input, e.deployCfg.Input.ServiceManifestFile, e.deploySource)
	if !ok {
		return model.StageStatus_STAGE_FAILURE
	}

	revision, ok := decideRevisionName(&e.Input, sm, e.Deployment.Trigger.Commit.Hash)
	if !ok {
		return model.StageStatus_STAGE_FAILURE
	}

	traffics := []provider.RevisionTraffic{
		{
			RevisionName: revision,
			Percent:      options.Percent,
		},
		{
			RevisionName: lastDeployedRevision,
			Percent:      100 - options.Percent,
		},
	}
	if !configureServiceManifest(&e.Input, sm, revision, traffics) {
		return model.StageStatus_STAGE_FAILURE
	}

	if !apply(ctx, &e.Input, e.cloudProviderName, e.cloudProviderCfg, sm) {
		return model.StageStatus_STAGE_FAILURE
	}

	// TODO: Wait to ensure the traffic was fully configured.
	return model.StageStatus_STAGE_SUCCESS
}
