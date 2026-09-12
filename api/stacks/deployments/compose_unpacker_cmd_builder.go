package deployments

import (
	"fmt"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	gittypes "github.com/portainer/portainer/api/git/types"
	"github.com/portainer/portainer/api/internal/registryutils"
	"github.com/portainer/portainer/pkg/secretresolver"
)

type StackRemoteOperation string

const (
	OperationDeploy        StackRemoteOperation = "compose-deploy"
	OperationUndeploy      StackRemoteOperation = "compose-undeploy"
	OperationComposeStart  StackRemoteOperation = "compose-start"
	OperationComposeStop   StackRemoteOperation = "compose-stop"
	OperationSwarmDeploy   StackRemoteOperation = "swarm-deploy"
	OperationSwarmUndeploy StackRemoteOperation = "swarm-undeploy"
	OperationSwarmStart    StackRemoteOperation = "swarm-start"
	OperationSwarmStop     StackRemoteOperation = "swarm-stop"
)

const (
	UnpackerCmdDeploy        = "deploy"
	UnpackerCmdUndeploy      = "undeploy"
	UnpackerCmdSwarmDeploy   = "swarm-deploy"
	UnpackerCmdSwarmUndeploy = "swarm-undeploy"
)

type unpackerCmdBuilderOptions struct {
	pullImage          bool
	prune              bool
	forceRecreate      bool
	composeDestination string
	registries         []portainer.Registry
	gitConfig          *gittypes.RepoConfig
}

type buildCmdFunc func(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string

// unpackerCmd is one operation's builder together with the one thing the caller has
// to know about it before calling: whether it reads the env argument at all.
type unpackerCmd struct {
	build buildCmdFunc

	// usesEnv marks the operations that hand the stack's environment to the unpacker
	// container as --env=NAME=VALUE arguments, and therefore the only ones that have to
	// refuse a secret reference. It mirrors the builders' bodies exactly: the four
	// deploy- and start-shaped commands read env, the four undeploy- and stop-shaped
	// ones address the project by name and ignore it. Keep the two in step - a builder
	// that starts consuming env without this flag would pass a reference through.
	//
	// remoteStack reads the same flag to refuse before it creates a docker client and
	// pulls the unpacker image, so this is the single source of that decision.
	usesEnv bool
}

var funcmap = map[StackRemoteOperation]unpackerCmd{
	OperationDeploy:        {build: buildDeployCmd, usesEnv: true},
	OperationUndeploy:      {build: buildUndeployCmd},
	OperationComposeStart:  {build: buildComposeStartCmd, usesEnv: true},
	OperationComposeStop:   {build: buildComposeStopCmd},
	OperationSwarmDeploy:   {build: buildSwarmDeployCmd, usesEnv: true},
	OperationSwarmUndeploy: {build: buildSwarmUndeployCmd},
	OperationSwarmStart:    {build: buildSwarmStartCmd, usesEnv: true},
	OperationSwarmStop:     {build: buildSwarmStopCmd},
}

// unpackerCmdFor returns one operation's entry in funcmap, refusing an operation that has
// none.
//
// The lookup is a function rather than an indexing expression at each call site because a
// missing key yields the zero unpackerCmd - a nil builder and usesEnv false - and the second
// of those is a silently skipped secret-reference check in remoteStack. Both callers go
// through here, so an unknown operation is refused in one place and before any work.
func unpackerCmdFor(operation StackRemoteOperation) (unpackerCmd, error) {
	cmd, known := funcmap[operation]
	if !known {
		return unpackerCmd{}, fmt.Errorf("unknown stack operation %s", operation)
	}

	return cmd, nil
}

// build the unpacker cmd for stack based on stackOperation
func (d *stackDeployer) buildUnpackerCmdForStack(stack *portainer.Stack, operation StackRemoteOperation, opts unpackerCmdBuilderOptions) ([]string, error) {
	cmd, err := unpackerCmdFor(operation)
	if err != nil {
		return nil, err
	}

	registriesStrings := generateRegistriesStrings(opts.registries, d.dataStore)

	// The environment is built - and a secret reference refused - only for the
	// operations that actually pass it on. Refusing it for all of them would leave a
	// stack that has migrated to references impossible to undeploy or stop through this
	// path: the user migrates the stack, finds the deploy unsupported here, and can then
	// no longer even clean it up. Removal needs no values, so it must not be gated on
	// resolving them.
	var envStrings []string

	if cmd.usesEnv {
		// getEnv fails for one reason only - a secret reference this path cannot resolve -
		// so this wrap is on the feature's refusal path and bounds the stack name like the
		// rest of it. See secretresolver.TruncateName.
		if envStrings, err = getEnv(stack.Name, stack.Env); err != nil {
			return nil, fmt.Errorf("stack %q: %w", secretresolver.TruncateName(stack.Name), err)
		}
	}

	return cmd.build(stack, opts, registriesStrings, envStrings), nil
}

// deploy [-u username -p password] [--skip-tls-verify] [--force-recreate] [-r] [-k] [--env KEY1=VALUE1 --env KEY2=VALUE2] <git-repo-url> <ref> <project-name> <destination> <compose-file-path> [<more-file-paths>...]
func buildDeployCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	cmd := []string{UnpackerCmdDeploy}
	cmd = appendGitAuthIfNeeded(cmd, opts.gitConfig)
	cmd = appendSkipTLSVerifyIfNeeded(cmd, opts.gitConfig)
	cmd = appendForceRecreateIfNeeded(cmd, opts.forceRecreate)

	if opts.prune {
		cmd = append(cmd, "-r")
	}

	cmd = append(cmd, env...)
	cmd = append(cmd, registries...)
	cmd = append(cmd,
		opts.gitConfig.URL,
		opts.gitConfig.ReferenceName,
		stack.Name,
		opts.composeDestination,
		stack.EntryPoint,
	)

	return append(cmd, stack.AdditionalFiles...)
}

// undeploy [-u username -p password] [-k] <git-repo-url> <project-name> <destination> <compose-file-path> [<more-file-paths>...]
func buildUndeployCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	cmd := []string{UnpackerCmdUndeploy}
	cmd = appendGitAuthIfNeeded(cmd, opts.gitConfig)
	cmd = append(cmd, opts.gitConfig.URL,
		stack.Name,
		opts.composeDestination,
		stack.EntryPoint,
	)

	return append(cmd, stack.AdditionalFiles...)
}

// deploy [-u username -p password] [--skip-tls-verify] [-k] [--env KEY1=VALUE1 --env KEY2=VALUE2] <git-repo-url> <project-name> <destination> <compose-file-path> [<more-file-paths>...]
func buildComposeStartCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	cmd := []string{UnpackerCmdDeploy}
	cmd = appendGitAuthIfNeeded(cmd, opts.gitConfig)
	cmd = appendSkipTLSVerifyIfNeeded(cmd, opts.gitConfig)
	cmd = append(cmd, "-k")
	cmd = append(cmd, env...)
	cmd = append(cmd, registries...)
	cmd = append(cmd, opts.gitConfig.URL,
		opts.gitConfig.ReferenceName,
		stack.Name,
		opts.composeDestination,
		stack.EntryPoint,
	)

	return append(cmd, stack.AdditionalFiles...)
}

// undeploy [-u username -p password] [-k] <git-repo-url> <project-name> <destination> <compose-file-path> [<more-file-paths>...]
func buildComposeStopCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	cmd := []string{UnpackerCmdUndeploy}
	cmd = appendGitAuthIfNeeded(cmd, opts.gitConfig)
	cmd = append(cmd,
		"-k",
		opts.gitConfig.URL,
		stack.Name,
		opts.composeDestination,
		stack.EntryPoint,
	)

	return append(cmd, stack.AdditionalFiles...)
}

// swarm-deploy [-u username -p password] [--skip-tls-verify] [--force-recreate] [-f] [-r] [-k] [--env KEY1=VALUE1 --env KEY2=VALUE2] <git-repo-url> <git-ref> <project-name> <destination> <compose-file-path> [<more-file-paths>...]
func buildSwarmDeployCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	cmd := []string{UnpackerCmdSwarmDeploy}
	cmd = appendGitAuthIfNeeded(cmd, opts.gitConfig)
	cmd = appendSkipTLSVerifyIfNeeded(cmd, opts.gitConfig)
	cmd = appendForceRecreateIfNeeded(cmd, opts.forceRecreate)
	if opts.pullImage {
		cmd = append(cmd, "-f")
	}

	if opts.prune {
		cmd = append(cmd, "-r")
	}

	cmd = append(cmd, env...)
	cmd = append(cmd, registries...)
	cmd = append(cmd, opts.gitConfig.URL,
		opts.gitConfig.ReferenceName,
		stack.Name,
		opts.composeDestination,
		stack.EntryPoint,
	)

	return append(cmd, stack.AdditionalFiles...)
}

// swarm-undeploy [-k] <project-name> <destination>
func buildSwarmUndeployCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	return []string{UnpackerCmdSwarmUndeploy, stack.Name, opts.composeDestination}
}

// swarm-deploy [-u username -p password] [-f] [-r] [-k] [--skip-tls-verify] [--env KEY1=VALUE1 --env KEY2=VALUE2] <git-repo-url> <project-name> <destination> <compose-file-path> [<more-file-paths>...]
func buildSwarmStartCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	cmd := []string{UnpackerCmdSwarmDeploy, "-f", "-r", "-k"}
	cmd = appendSkipTLSVerifyIfNeeded(cmd, opts.gitConfig)
	// The env argument holds getEnv(stack.Env), already checked for secret references
	// by buildUnpackerCmdForStack - this operation is marked usesEnv there. Calling
	// getEnv again here would bypass that check.
	cmd = append(cmd, env...)
	cmd = append(cmd, registries...)
	cmd = append(cmd, opts.gitConfig.URL,
		opts.gitConfig.ReferenceName,
		stack.Name,
		opts.composeDestination,
		stack.EntryPoint,
	)

	return append(cmd, stack.AdditionalFiles...)
}

// swarm-undeploy [-k] <project-name> <destination>
func buildSwarmStopCmd(stack *portainer.Stack, opts unpackerCmdBuilderOptions, registries []string, env []string) []string {
	return []string{UnpackerCmdSwarmUndeploy, "-k", stack.Name, opts.composeDestination}
}

func appendGitAuthIfNeeded(cmd []string, gc *gittypes.RepoConfig) []string {
	if gc == nil || gc.Authentication == nil || gc.Authentication.Password == "" {
		return cmd
	}

	return append(cmd, "-u", gc.Authentication.Username, "-p", gc.Authentication.Password)
}

func appendSkipTLSVerifyIfNeeded(cmd []string, gc *gittypes.RepoConfig) []string {
	if gc == nil || !gc.TLSSkipVerify {
		return cmd
	}

	return append(cmd, "--skip-tls-verify")
}

func appendForceRecreateIfNeeded(cmd []string, forceRecreate bool) []string {
	if forceRecreate {
		cmd = append(cmd, "--force-recreate")
	}

	return cmd
}

func generateRegistriesStrings(registries []portainer.Registry, dataStore dataservices.DataStore) []string {
	cmds := []string{}

	for _, registry := range registries {
		if registry.Authentication {
			if err := registryutils.EnsureRegTokenValid(dataStore, &registry); err != nil {
				continue
			}

			username, password, err := registryutils.GetRegEffectiveCredential(&registry)
			if err != nil {
				continue
			}

			cmds = append(cmds, fmt.Sprintf("--registry=%s:%s:%s", username, password, registry.URL))
		}
	}

	return cmds
}

func getEnv(stackName string, env []portainer.Pair) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}

	cmd := []string{}
	escaped := 0

	for _, pair := range env {
		// Every variable is handed to the compose-unpacker container as a literal
		// --env=NAME=VALUE argument, and nothing on that side resolves a reference: the
		// service would start with the string "secret:..." as its credential. There is no
		// resolution path here, so refuse rather than pass it through - the same rule
		// SwarmStackManager.Deploy applies. Refusing beats silently dropping the
		// variable, which would start the same service on an undefined one.
		//
		// This path is unreachable in CE today only because stackutils.IsRelativePathStack
		// is hardcoded to false; the guard must not rest on that stub.
		if secretresolver.IsReference(pair.Value) {
			return nil, unsupportedSecretReferenceError(pair.Name)
		}

		// The escaped marker is a literal: it is passed on as the same text the compose
		// path would produce, one colon shorter.
		value := secretresolver.Unescape(pair.Value)
		if value != pair.Value {
			escaped++
		}

		cmd = append(cmd, fmt.Sprintf(`--env=%s=%s`, pair.Name, value))
	}

	// The one silent effect of the marker, so it is announced here as on the compose path.
	secretresolver.LogEscapedValues(stackName, escaped)

	return cmd, nil
}

// unsupportedSecretReferenceError reports a variable this path cannot deploy. It is one
// function so that the checks hoisted ahead of the work - the compose pull in
// DeployRemoteComposeStack, the docker client and image pull in remoteStack - and the one
// inside getEnv cannot drift apart in their wording.
//
// The variable name is bounded and quoted, like every other name this feature names in a
// persisted deploy error; see secretresolver.TruncateName.
func unsupportedSecretReferenceError(name string) error {
	return fmt.Errorf("variable %q uses a secret reference, but secret references are not supported for stacks deployed through the compose unpacker", secretresolver.TruncateName(name))
}

// checkNoSecretReferences reports the first variable of the stack that holds a secret
// reference, wrapped exactly as buildUnpackerCmdForStack wraps it.
func checkNoSecretReferences(stack *portainer.Stack) error {
	for _, pair := range stack.Env {
		if secretresolver.IsReference(pair.Value) {
			return fmt.Errorf("stack %q: %w", secretresolver.TruncateName(stack.Name), unsupportedSecretReferenceError(pair.Name))
		}
	}

	return nil
}
