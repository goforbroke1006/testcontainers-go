package testcontainers

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/cli"
	"github.com/compose-spec/compose-go/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
	types2 "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"golang.org/x/sync/errgroup"

	"github.com/testcontainers/testcontainers-go/wait"
)

type stackUpOptionFunc func(s *stackUpOptions)

func (f stackUpOptionFunc) applyToStackUp(o *stackUpOptions) {
	f(o)
}

type stackDownOptionFunc func(do *api.DownOptions)

func (f stackDownOptionFunc) applyToStackDown(do *api.DownOptions) {
	f(do)
}

// RunServices is comparable to 'docker-compose run' as it only creates a subset of containers
// instead of all services defined by the project
func RunServices(serviceNames ...string) StackUpOption {
	return stackUpOptionFunc(func(o *stackUpOptions) {
		o.Services = serviceNames
	})
}

// IgnoreOrphans - Ignore legacy containers for services that are not defined in the project
type IgnoreOrphans bool

func (io IgnoreOrphans) applyToStackUp(co *api.CreateOptions, _ *api.StartOptions) {
	co.IgnoreOrphans = bool(io)
}

// RemoveOrphans will cleanup containers that are not declared on the compose model but own the same labels
type RemoveOrphans bool

func (ro RemoveOrphans) applyToStackUp(o *stackUpOptions) {
	o.RemoveOrphans = bool(ro)
}

func (ro RemoveOrphans) applyToStackDown(o *stackDownOptions) {
	o.RemoveOrphans = bool(ro)
}

// Wait won't return until containers reached the running|healthy state
type Wait bool

func (w Wait) applyToStackUp(o *stackUpOptions) {
	o.Wait = bool(w)
}

// RemoveImages used by services
type RemoveImages uint8

func (ri RemoveImages) applyToStackDown(o *stackDownOptions) {
	switch ri {
	case RemoveImagesAll:
		o.Images = "all"
	case RemoveImagesLocal:
		o.Images = "local"
	}
}

type ComposeStackFiles []string

func (f ComposeStackFiles) applyToComposeStack(o *composeStackOptions) {
	o.Paths = f
}

type StackIdentifier string

func (f StackIdentifier) applyToComposeStack(o *composeStackOptions) {
	o.Identifier = string(f)
}

func (f StackIdentifier) String() string {
	return string(f)
}

type ComposeStackReaders []io.Reader

func (r ComposeStackReaders) applyToComposeStack(o *composeStackOptions) {
	currentDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	// choose directory to keep temporary files
	// like
	// 		/tmp/testcontainers-go/my-awesome-service-2f686f6d652f676f666f7262726f6b65
	projectName := filepath.Base(currentDir)
	projectHash := fmt.Sprintf("%x", fnv.New32a().Sum([]byte(currentDir)))[:32]
	tmpDir := filepath.Join(os.TempDir(), "testcontainers-go", fmt.Sprintf("%s-%s", projectName, projectHash))

	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		panic(err)
	}

	// write temporary files and put to files list
	filePaths := make([]string, 0, len(r))
	for idx, src := range r {
		content, err := io.ReadAll(src)
		if err != nil {
			panic(err)
		}
		name := fmt.Sprintf("docker-compose-%d.yaml", idx)
		filename := filepath.Join(tmpDir, name)
		if err := os.WriteFile(filename, content, os.ModePerm); err != nil {
			continue
		}
		filePaths = append(filePaths, filename)
	}

	o.Paths = filePaths
}

const (
	// RemoveImagesAll - remove all images used by the stack
	RemoveImagesAll RemoveImages = iota
	// RemoveImagesLocal - remove only images that don't have a tag
	RemoveImagesLocal
)

type dockerCompose struct {
	// used to synchronize operations
	lock sync.RWMutex

	// name/identifier of the stack that will be started
	// by default a UUID will be used
	name string

	// paths to stack files that will be considered when compiling the final compose project
	configs []string

	// wait strategies that are applied per service when starting the stack
	// only one strategy can be added to a service, to use multiple use wait.ForAll(...)
	waitStrategies map[string]wait.Strategy

	// cache for containers that are part of the stack
	// used in ServiceContainer(...) function to avoid calls to the Docker API
	containers map[string]*DockerContainer

	// docker/compose API service instance used to control the compose stack
	composeService api.Service

	// Docker API client used to interact with single container instances and the Docker API e.g. to list containers
	dockerClient client.APIClient

	// options used to compile the compose project
	// e.g. environment settings, ...
	projectOptions []cli.ProjectOptionsFn

	// compiled compose project
	// can be nil if the stack wasn't started yet
	project *types.Project
}

func (d *dockerCompose) ServiceContainer(ctx context.Context, svcName string) (*DockerContainer, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	return d.lookupContainer(ctx, svcName)
}

func (d *dockerCompose) Services() []string {
	d.lock.Lock()
	defer d.lock.Unlock()

	return d.project.ServiceNames()
}

func (d *dockerCompose) Down(ctx context.Context, opts ...StackDownOption) error {
	d.lock.Lock()
	defer d.lock.Unlock()

	options := stackDownOptions{
		DownOptions: api.DownOptions{
			Project: d.project,
		},
	}

	for i := range opts {
		opts[i].applyToStackDown(&options)
	}

	return d.composeService.Down(ctx, d.name, options.DownOptions)
}

func (d *dockerCompose) Up(ctx context.Context, opts ...StackUpOption) (err error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.project, err = d.compileProject()
	if err != nil {
		return err
	}

	upOptions := stackUpOptions{
		Services:             d.project.ServiceNames(),
		Recreate:             api.RecreateDiverged,
		RecreateDependencies: api.RecreateDiverged,
		Project:              d.project,
	}

	for i := range opts {
		opts[i].applyToStackUp(&upOptions)
	}

	if len(upOptions.Services) != len(d.project.Services) {
		sort.Strings(upOptions.Services)

		filteredServices := make(types.Services, 0, len(d.project.Services))

		for i := range d.project.Services {
			if idx := sort.SearchStrings(upOptions.Services, d.project.Services[i].Name); idx < len(upOptions.Services) && upOptions.Services[idx] == d.project.Services[i].Name {
				filteredServices = append(filteredServices, d.project.Services[i])
			}
		}

		d.project.Services = filteredServices
	}

	err = d.composeService.Up(ctx, d.project, api.UpOptions{
		Create: api.CreateOptions{
			Services:             upOptions.Services,
			Recreate:             upOptions.Recreate,
			RecreateDependencies: upOptions.RecreateDependencies,
			RemoveOrphans:        upOptions.RemoveOrphans,
		},
		Start: api.StartOptions{
			Project: upOptions.Project,
			Wait:    upOptions.Wait,
		},
	})

	if err != nil {
		return err
	}

	if len(d.waitStrategies) == 0 {
		return nil
	}

	errGrp, errGrpCtx := errgroup.WithContext(ctx)

	for svc, strategy := range d.waitStrategies { // pinning the variables
		svc := svc
		strategy := strategy

		errGrp.Go(func() error {
			target, err := d.lookupContainer(errGrpCtx, svc)
			if err != nil {
				return err
			}
			return strategy.WaitUntilReady(errGrpCtx, target)
		})
	}

	return errGrp.Wait()
}

func (d *dockerCompose) WaitForService(s string, strategy wait.Strategy) ComposeStack {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.waitStrategies[s] = strategy
	return d
}

func (d *dockerCompose) WithEnv(m map[string]string) ComposeStack {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.projectOptions = append(d.projectOptions, withEnv(m))
	return d
}

func (d *dockerCompose) WithOsEnv() ComposeStack {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.projectOptions = append(d.projectOptions, cli.WithOsEnv)
	return d
}

func (d *dockerCompose) lookupContainer(ctx context.Context, svcName string) (*DockerContainer, error) {
	if container, ok := d.containers[svcName]; ok {
		return container, nil
	}

	listOptions := types2.ContainerListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", fmt.Sprintf("%s=%s", api.ProjectLabel, d.name)),
			filters.Arg("label", fmt.Sprintf("%s=%s", api.ServiceLabel, svcName)),
		),
	}

	containers, err := d.dockerClient.ContainerList(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	if len(containers) == 0 {
		return nil, fmt.Errorf("no container found for service name %s", svcName)
	}

	containerInstance := containers[0]
	container := &DockerContainer{
		ID: containerInstance.ID,
		provider: &DockerProvider{
			client: d.dockerClient,
		},
	}

	d.containers[svcName] = container

	return container, nil
}

func (d *dockerCompose) compileProject() (*types.Project, error) {
	const nameAndDefaultConfigPath = 2
	projectOptions := make([]cli.ProjectOptionsFn, len(d.projectOptions), len(d.projectOptions)+nameAndDefaultConfigPath)

	copy(projectOptions, d.projectOptions)
	projectOptions = append(projectOptions, cli.WithName(d.name), cli.WithDefaultConfigPath)

	compiledOptions, err := cli.NewProjectOptions(d.configs, projectOptions...)
	if err != nil {
		return nil, err
	}

	proj, err := cli.ProjectFromOptions(compiledOptions)
	if err != nil {
		return nil, err
	}

	for i, s := range proj.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     proj.Name,
			api.ServiceLabel:     s.Name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  proj.WorkingDir,
			api.ConfigFilesLabel: strings.Join(proj.ComposeFiles, ","),
			api.OneoffLabel:      "False", // default, will be overridden by `run` command
		}
		if compiledOptions.EnvFile != "" {
			s.CustomLabels[api.EnvironmentFileLabel] = compiledOptions.EnvFile
		}
		proj.Services[i] = s
	}

	return proj, nil
}

func withEnv(env map[string]string) func(*cli.ProjectOptions) error {
	return func(options *cli.ProjectOptions) error {
		for k, v := range env {
			if _, ok := options.Environment[k]; ok {
				return fmt.Errorf("environment with key %s already set", k)
			} else {
				options.Environment[k] = v
			}
		}

		return nil
	}
}

func makeClient(*command.DockerCli) (client.APIClient, error) {
	dockerClient, _, _, err := NewDockerClient()
	if err != nil {
		return nil, err
	}
	return dockerClient, nil
}
