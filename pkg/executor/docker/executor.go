package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"runtime/debug"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/filecoin-project/bacalhau/pkg/capacitymanager"
	"github.com/filecoin-project/bacalhau/pkg/config"
	"github.com/filecoin-project/bacalhau/pkg/docker"
	"github.com/filecoin-project/bacalhau/pkg/executor"
	jobutils "github.com/filecoin-project/bacalhau/pkg/job"
	"github.com/filecoin-project/bacalhau/pkg/model"
	"github.com/filecoin-project/bacalhau/pkg/storage"
	"github.com/filecoin-project/bacalhau/pkg/storage/util"
	"github.com/filecoin-project/bacalhau/pkg/system"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
)

const NanoCPUCoefficient = 1000000000

type Executor struct {
	// used to allow multiple docker executors to run against the same docker server
	ID string

	// where do we copy the results from jobs temporarily?
	ResultsDir string

	// the storage providers we can implement for a job
	StorageProviders map[model.StorageSourceType]storage.StorageProvider

	Client *dockerclient.Client
}

func NewExecutor(
	ctx context.Context,
	cm *system.CleanupManager,
	id string,
	storageProviders map[model.StorageSourceType]storage.StorageProvider,
) (*Executor, error) {
	dockerClient, err := docker.NewDockerClient()
	if err != nil {
		return nil, err
	}

	dir, err := ioutil.TempDir("", "bacalhau-docker-executor")
	if err != nil {
		return nil, err
	}

	de := &Executor{
		ID:               id,
		ResultsDir:       dir,
		StorageProviders: storageProviders,
		Client:           dockerClient,
	}

	cm.RegisterCallback(func() error {
		de.cleanupAll(ctx)
		return nil
	})

	return de, nil
}

func (e *Executor) getStorageProvider(ctx context.Context, engine model.StorageSourceType) (storage.StorageProvider, error) {
	return util.GetStorageProvider(ctx, engine, e.StorageProviders)
}

// IsInstalled checks if docker itself is installed.
func (e *Executor) IsInstalled(ctx context.Context) (bool, error) {
	return docker.IsInstalled(ctx, e.Client), nil
}

func (e *Executor) HasStorageLocally(ctx context.Context, volume model.StorageSpec) (bool, error) {
	ctx, span := newSpan(ctx, "HasStorageLocally")
	defer span.End()

	s, err := e.getStorageProvider(ctx, volume.Engine)
	if err != nil {
		return false, err
	}

	return s.HasStorageLocally(ctx, volume)
}

func (e *Executor) GetVolumeSize(ctx context.Context, volume model.StorageSpec) (uint64, error) {
	storageProvider, err := e.getStorageProvider(ctx, volume.Engine)
	if err != nil {
		return 0, err
	}
	return storageProvider.GetVolumeSize(ctx, volume)
}

//nolint:funlen,gocyclo // will clean up
func (e *Executor) RunShard(
	ctx context.Context,
	shard model.JobShard,
	jobResultsDir string,
) error {
	//nolint:ineffassign,staticcheck
	ctx, span := system.GetTracer().Start(ctx, "pkg/executor/docker.RunShard")
	defer span.End()
	system.AddJobIDFromBaggageToSpan(ctx, span)
	system.AddNodeIDFromBaggageToSpan(ctx, span)

	// the actual mounts we will give to the container
	// these are paths for both input and output data
	mounts := []mount.Mount{}

	shardStorageSpec, err := jobutils.GetShardStorageSpec(ctx, shard, e.StorageProviders)
	if err != nil {
		return err
	}

	// reusable between the input shards and the input context
	addInputStorageHandler := func(spec model.StorageSpec) error {
		var storageProvider storage.StorageProvider
		var volumeMount storage.StorageVolume
		storageProvider, err = e.getStorageProvider(ctx, spec.Engine)
		if err != nil {
			return err
		}

		volumeMount, err = storageProvider.PrepareStorage(ctx, spec)
		if err != nil {
			return err
		}

		if volumeMount.Type == storage.StorageVolumeConnectorBind {
			log.Trace().Msgf("Input Volume: %+v %+v", spec, volumeMount)
			mounts = append(mounts, mount.Mount{
				Type: "bind",
				// this is an input volume so is read only
				ReadOnly: true,
				Source:   volumeMount.Source,
				Target:   volumeMount.Target,
			})
		} else {
			return fmt.Errorf("unknown storage volume type: %s", volumeMount.Type)
		}
		return nil
	}

	// loop over the job contexts and prepare them
	for _, contextStorage := range shard.Job.Spec.Contexts {
		err = addInputStorageHandler(contextStorage)
		if err != nil {
			return err
		}
	}

	// loop over the job storage inputs and prepare them
	for _, inputStorage := range shardStorageSpec {
		err = addInputStorageHandler(inputStorage)
		if err != nil {
			return err
		}
	}

	// for this phase of the outputs we ignore the engine because it's just about collecting the
	// data from the job and keeping it locally
	// the engine property of the output storage spec is how we will "publish" the output volume
	// if and when the deal is settled
	for _, output := range shard.Job.Spec.Outputs {
		if output.Name == "" {
			return fmt.Errorf("output volume has no name: %+v", output)
		}

		if output.Path == "" {
			return fmt.Errorf("output volume has no path: %+v", output)
		}

		srcd := fmt.Sprintf("%s/%s", jobResultsDir, output.Name)
		err = os.Mkdir(srcd, util.OS_ALL_R|util.OS_ALL_X|util.OS_USER_W)
		if err != nil {
			return err
		}

		log.Trace().Msgf("Output Volume: %+v", output)

		// create a mount so the output data does not need to be copied back to the host
		mounts = append(mounts, mount.Mount{

			Type: "bind",
			// this is an output volume so can be written to
			ReadOnly: false,

			// we create a named folder in the job results folder for this output
			Source: srcd,

			// the path of the output volume is from the perspective of inside the container
			Target: output.Path,
		})
	}

	if os.Getenv("SKIP_IMAGE_PULL") == "" {
		// TODO: #283 work out why this does not work in github actions
		// err = docker.PullImage(e.Client, job.Spec.Vm.Image)
		var im dockertypes.ImageInspect
		im, _, err = e.Client.ImageInspectWithRaw(ctx, shard.Job.Spec.Docker.Image)
		if err == nil {
			log.Debug().Msgf("Not pulling image %s, already have %s", shard.Job.Spec.Docker.Image, im.ID)
		} else if dockerclient.IsErrNotFound(err) {
			stdout, err := system.RunCommandGetResults( //nolint:govet // shadowing ok
				"docker",
				[]string{"pull", shard.Job.Spec.Docker.Image},
			)
			if err != nil {
				return fmt.Errorf("error pulling %s: %s, %s", shard.Job.Spec.Docker.Image, err, stdout)
			}
			log.Trace().Msgf("Pull image output: %s\n%s", shard.Job.Spec.Docker.Image, stdout)
		} else {
			return fmt.Errorf("error checking if we have %s locally: %s", shard.Job.Spec.Docker.Image, err)
		}
	}

	// json the job spec and pass it into all containers
	// TODO: check if this will overwrite a user supplied version of this value
	// (which is what we actually want to happen)
	jsonJobSpec, err := json.Marshal(shard.Job.Spec)
	if err != nil {
		return err
	}

	useEnv := append(shard.Job.Spec.Docker.Env, fmt.Sprintf("BACALHAU_JOB_SPEC=%s", string(jsonJobSpec))) //nolint:gocritic

	containerConfig := &container.Config{
		Image:           shard.Job.Spec.Docker.Image,
		Tty:             false,
		Env:             useEnv,
		Entrypoint:      shard.Job.Spec.Docker.Entrypoint,
		Labels:          e.jobContainerLabels(shard.Job),
		NetworkDisabled: true,
		WorkingDir:      shard.Job.Spec.Docker.WorkingDir,
	}

	log.Trace().Msgf("Container: %+v %+v", containerConfig, mounts)

	resourceRequirements := capacitymanager.ParseResourceUsageConfig(shard.Job.Spec.Resources)

	// Create GPU request if the job requests it
	var deviceRequests []container.DeviceRequest
	if resourceRequirements.GPU > 0 {
		deviceRequests = append(deviceRequests,
			container.DeviceRequest{
				DeviceIDs:    []string{"0"}, // TODO: how do we know which device ID to use?
				Capabilities: [][]string{{"gpu"}},
			},
		)
		log.Trace().Msgf("Adding %d GPUs to request", resourceRequirements.GPU)
	}

	jobContainer, err := e.Client.ContainerCreate(
		ctx,
		containerConfig,
		&container.HostConfig{
			Mounts: mounts,
			Resources: container.Resources{
				Memory:         int64(resourceRequirements.Memory),
				NanoCPUs:       int64(resourceRequirements.CPU * NanoCPUCoefficient),
				DeviceRequests: deviceRequests,
			},
		},
		&network.NetworkingConfig{},
		nil,
		e.jobContainerName(shard),
	)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	err = e.Client.ContainerStart(
		ctx,
		jobContainer.ID,
		dockertypes.ContainerStartOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	defer e.cleanupJob(ctx, shard)

	// the idea here is even if the container errors
	// we want to capture stdout, stderr and feed it back to the user
	var containerError error
	var containerExitStatusCode int64
	statusCh, errCh := e.Client.ContainerWait(
		ctx,
		jobContainer.ID,
		container.WaitConditionNotRunning,
	)
	select {
	case err = <-errCh:
		containerError = err
	case exitStatus := <-statusCh:
		containerExitStatusCode = exitStatus.StatusCode
		if exitStatus.Error != nil {
			containerError = errors.New(exitStatus.Error.Message)
		}
	}
	if containerExitStatusCode != 0 {
		if containerError == nil {
			containerError = fmt.Errorf("exit code was not zero: %d", containerExitStatusCode)
		}
		log.Info().Msgf("container error %s", containerError)
	}

	stdout, stderr, err := system.RunCommandGetStdoutAndStderr(
		"docker",
		[]string{
			"logs",
			"-f",
			jobContainer.ID,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	err = os.WriteFile(
		fmt.Sprintf("%s/exitCode", jobResultsDir),
		[]byte(fmt.Sprintf("%d", containerExitStatusCode)),
		util.OS_ALL_R|util.OS_USER_RW,
	)
	if err != nil {
		msg := fmt.Sprintf("could not write results to exitCode: %s", err)
		log.Error().Msg(msg)
		return errors.New(msg)
	}

	err = os.WriteFile(
		fmt.Sprintf("%s/stdout", jobResultsDir),
		[]byte(stdout),
		util.OS_ALL_R|util.OS_USER_RW,
	)
	if err != nil {
		msg := fmt.Sprintf("could not write results to stdout: %s", err)
		log.Error().Msg(msg)
		return errors.New(msg)
	}

	err = os.WriteFile(
		fmt.Sprintf("%s/stderr", jobResultsDir),
		[]byte(stderr),
		util.OS_ALL_R|util.OS_USER_RW,
	)
	if err != nil {
		msg := fmt.Sprintf("could not write results to stderr: %s", err)
		log.Error().Msg(msg)
		return errors.New(msg)
	}

	return containerError
}

func (e *Executor) cleanupJob(ctx context.Context, shard model.JobShard) {
	if config.ShouldKeepStack() {
		return
	}

	err := docker.RemoveContainer(ctx, e.Client, e.jobContainerName(shard))
	if err != nil {
		log.Error().Msgf("Docker remove container error: %s", err.Error())
		debug.PrintStack()
	}
}

func (e *Executor) cleanupAll(ctx context.Context) {
	if config.ShouldKeepStack() {
		return
	}

	log.Debug().Msgf("Cleaning up all bacalhau containers for executor %s...", e.ID)
	containersWithLabel, err := docker.GetContainersWithLabel(ctx, e.Client, "bacalhau-executor", e.ID)
	if err != nil {
		log.Error().Msgf("Docker executor stop error: %s", err.Error())
		return
	}
	// TODO: #287 Fix if when we care about optimization of memory (224 bytes copied per loop)
	//nolint:gocritic // will fix when we care
	for _, container := range containersWithLabel {
		err = docker.RemoveContainer(ctx, e.Client, container.ID)
		if err != nil {
			log.Error().Msgf("Non-critical error cleaning up container: %s", err.Error())
		}
	}
}

func (e *Executor) jobContainerName(shard model.JobShard) string {
	return fmt.Sprintf("bacalhau-%s-%s-%d", e.ID, shard.Job.ID, shard.Index)
}

func (e *Executor) jobContainerLabels(job model.Job) map[string]string {
	return map[string]string{
		"bacalhau-executor": e.ID,
		"bacalhau-jobID":    job.ID,
	}
}

func newSpan(ctx context.Context, apiName string) (context.Context, trace.Span) {
	return system.Span(ctx, "executor/docker", apiName)
}

// Compile-time interface check:
var _ executor.Executor = (*Executor)(nil)
