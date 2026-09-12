package exec

import (
	"context"
	"fmt"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/http/proxy"
	"github.com/portainer/portainer/api/stacks/stackutils"
	"github.com/portainer/portainer/pkg/libstack/swarm"
	"github.com/portainer/portainer/pkg/secretresolver"
)

// SwarmStackManager represents a service for managing stacks.
type SwarmStackManager struct {
	deployer     swarm.Deployer
	proxyManager *proxy.Manager
}

// NewSwarmStackManager creates a new SwarmStackManager.
func NewSwarmStackManager(
	deployer swarm.Deployer,
	proxyManager *proxy.Manager,
) *SwarmStackManager {
	return &SwarmStackManager{
		deployer:     deployer,
		proxyManager: proxyManager,
	}
}

// Deploy creates or updates a Docker Swarm stack.
func (manager *SwarmStackManager) Deploy(
	ctx context.Context,
	stack *portainer.Stack,
	prune bool,
	pullImage bool,
	endpoint *portainer.Endpoint,
	registries []portainer.Registry,
) error {
	// Swarm stacks do not resolve secret references. Deploying one verbatim would
	// start a service with the literal string "secret:..." as its credential, so
	// refuse instead - the same rule ComposeStackManager.resolveStackSecrets applies
	// when no resolver is configured.
	//
	// Checked before the proxy is fetched: a deploy that is refused outright has no
	// reason to open an endpoint proxy and close it again.
	//
	// Both names bounded and quoted, as at every site that names one in a persisted deploy
	// error; see secretresolver.TruncateName.
	for _, ev := range stack.Env {
		if secretresolver.IsReference(ev.Value) {
			return fmt.Errorf("stack %q: variable %q uses a secret reference, but secret references are not supported for swarm stacks",
				secretresolver.TruncateName(stack.Name), secretresolver.TruncateName(ev.Name))
		}
	}

	filePaths := stackutils.GetStackFilePaths(stack, true)

	// A reference written inline in the body is refused for the same reason, and here it is
	// the only thing that can catch it: the compose path rewrites such a body before it is
	// deployed, and nothing on this path does.
	//
	// An escaped marker in a body is not a reference and keeps passing through verbatim,
	// unlike an escaped variable, which is unescaped below. Swarm takes the body as it
	// stands, so the escape would have to be resolved by rewriting the file, which is what
	// this path deliberately does not do.
	for _, filePath := range filePaths {
		_, refs, err := readStackComposeFile(stack, filePath)
		if err != nil {
			return err
		}

		if len(refs) > 0 {
			return fmt.Errorf("stack %q: compose file %q uses a secret reference, but secret references are not supported for swarm stacks",
				secretresolver.TruncateName(stack.Name), secretresolver.TruncateName(filePath))
		}
	}

	url, proxy, err := fetchEndpointProxy(manager.proxyManager, endpoint)
	if err != nil {
		return fmt.Errorf("failed to fetch environment proxy: %w", err)
	}

	if proxy != nil {
		defer proxy.Close()
	}

	env := make([]string, 0, len(stack.Env))
	escaped := 0

	for _, ev := range stack.Env {
		// The escaped marker is a literal here too. A swarm stack cannot resolve a
		// reference, but a value that merely looks like one has to reach the service as
		// the same text the compose path would produce, or the escape would mean two
		// different things depending on how the stack is deployed.
		value := secretresolver.Unescape(ev.Value)
		if value != ev.Value {
			escaped++
		}

		env = append(env, ev.Name+"="+value)
	}

	// The one silent effect of the marker, so it is announced here as on the compose path.
	secretresolver.LogEscapedValues(stack.Name, escaped)

	return manager.deployer.Deploy(context.TODO(), filePaths, swarm.DeployOptions{
		Options: swarm.Options{
			ProjectName: stack.Name,
			Host:        url,
			Env:         env,
			WorkingDir:  stack.ProjectPath,
			Registries:  portainerRegistriesToAuthConfigs(registries),
		},
		RemoveOrphans: prune,
		PullImage:     pullImage,
	})
}

// Remove deletes all resources belonging to a Swarm stack.
func (manager *SwarmStackManager) Remove(
	ctx context.Context,
	stack *portainer.Stack,
	endpoint *portainer.Endpoint,
) error {
	url, proxy, err := fetchEndpointProxy(manager.proxyManager, endpoint)
	if err != nil {
		return fmt.Errorf("failed to fetch environment proxy: %w", err)
	}

	if proxy != nil {
		defer proxy.Close()
	}

	return manager.deployer.Remove(context.TODO(), stack.Name, swarm.RemoveOptions{
		Options: swarm.Options{
			Host: url,
		},
	})
}

// NormalizeStackName returns a new stack name with unsupported characters replaced.
func (manager *SwarmStackManager) NormalizeStackName(name string) string {
	return normalizeStackName(name)
}
