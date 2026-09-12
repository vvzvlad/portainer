package deployments

import (
	"context"
	"fmt"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/http/security"
	"github.com/portainer/portainer/api/internal/registryutils"
	"github.com/portainer/portainer/api/stacks/stackutils"
	"github.com/portainer/portainer/pkg/secretresolver"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

type ComposeStackDeploymentConfig struct {
	stack          *portainer.Stack
	endpoint       *portainer.Endpoint
	registries     []portainer.Registry
	isAdmin        bool
	user           *portainer.User
	forcePullImage bool
	ForceCreate    bool
	FileService    portainer.FileService
	StackDeployer  StackDeployer
	prune          bool
}

func CreateComposeStackDeploymentConfigTx(tx dataservices.DataStoreTx, securityContext *security.RestrictedRequestContext, stack *portainer.Stack, endpoint *portainer.Endpoint, fileService portainer.FileService, deployer StackDeployer, prune, forcePullImage, forceCreate bool) (*ComposeStackDeploymentConfig, error) {
	user, err := tx.User().Read(securityContext.UserID)
	if err != nil {
		return nil, fmt.Errorf("unable to load user information from the database: %w", err)
	}

	registries, err := tx.Registry().ReadAll()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve registries from the database: %w", err)
	}

	filteredRegistries := security.FilterRegistries(registries, user, securityContext.UserMemberships, endpoint.ID)

	registryutils.RefreshAndPersistECRTokens(tx, filteredRegistries)

	config := &ComposeStackDeploymentConfig{
		stack:          stack,
		endpoint:       endpoint,
		registries:     filteredRegistries,
		prune:          prune,
		isAdmin:        securityContext.IsAdmin,
		user:           user,
		forcePullImage: forcePullImage,
		ForceCreate:    forceCreate,
		FileService:    fileService,
		StackDeployer:  deployer,
	}

	return config, nil
}

func (config *ComposeStackDeploymentConfig) Deploy(ctx context.Context) error {
	if config.FileService == nil || config.StackDeployer == nil {
		log.Debug().Msg("file service or stack deployer is not initialized")
		return errors.New("file service or stack deployer cannot be nil")
	}

	isAdminOrEndpointAdmin := stackutils.UserIsAdminOrEndpointAdmin(config.user)
	if !isAdminOrEndpointAdmin {
		if err := refuseSecretReferencesForNonAdmin(config.stack, config.FileService); err != nil {
			return err
		}

		if config.endpoint != nil {
			if err := stackutils.ValidateStackFiles(config.stack, &config.endpoint.SecuritySettings, config.FileService); err != nil {
				return err
			}
		}
	}

	if err := stackutils.ValidateComposeURLs(ctx, config.stack, config.FileService); err != nil {
		return err
	}

	if stackutils.IsRelativePathStack(config.stack) {
		return config.StackDeployer.DeployRemoteComposeStack(ctx, config.stack, config.endpoint, config.registries, config.prune, config.forcePullImage, config.ForceCreate)
	}

	return config.StackDeployer.DeployComposeStack(ctx, config.stack, config.endpoint, config.registries, config.prune, config.forcePullImage, config.ForceCreate)
}

// refuseSecretReferencesForNonAdmin fails a deploy by a user who is not an administrator
// or an environment administrator when the stack carries any secret reference.
//
// stackutils.ValidateStackFiles is what enforces the environment's policy on bind mounts,
// privileged, pid: host, devices, sysctls, security_opt and capabilities FOR THIS CLASS OF
// USER, and it enforces it by interpolating the stack's environment into the compose file
// and inspecting the result - but it interpolates the reference while the deploy
// interpolates the value. So for a non-administrator the policy runs against a different
// string than the one that starts the containers: "secret:vw:stack/env/app/VAR" in a
// volume's source position is classified as a named volume (nothing in it begins with "/",
// "." or "~"), while the value it resolves to at deploy time can be "/:/host" and become a
// real bind mount the policy would have refused.
//
// Closing the divergence properly would mean resolving secrets for validation as well,
// which puts the vault on the path of every stack save rather than every deploy. Refusing
// the deploy closes the half that matters and only for the users the policy applies to:
// an administrator is not gated, because no policy is being evaded there. Fail closed -
// the refusal does not depend on an endpoint being present, since a missing endpoint
// means no policy check ran at all. See docs/secret-resolver.md §3.11, which also records
// what stays diverged: ValidateComposeURLs (the SSRF policy) applies to every user, so an
// admin gate cannot fix that half.
//
// Both names are bounded and quoted on the way into the message, and this is the site where
// that matters most: this refusal fires precisely FOR a user who is not an administrator -
// the lowest-privileged actor in the feature's threat model, and the one the gate exists to
// contain - while the resolver's own text, from an adjacent component we run, has been
// sanitised since round 10. A mebibyte of NUL and ESC in a variable name put 1048755 bytes
// into Stack.DeploymentStatus[].Message here, control bytes intact, for exactly that user.
// See secretresolver.TruncateName for the class and for why %q alone is not the fix.
func refuseSecretReferencesForNonAdmin(stack *portainer.Stack, fileService portainer.FileService) error {
	for _, pair := range stack.Env {
		if secretresolver.IsReference(pair.Value) {
			return fmt.Errorf("stack %q: variable %q uses a secret reference, and a stack carrying secret references can only be deployed by an administrator or an environment administrator",
				secretresolver.TruncateName(stack.Name), secretresolver.TruncateName(pair.Name))
		}
	}

	// The same divergence, and a wider one, for a reference written inline in the body: the
	// deploy rewrites that scalar into a placeholder and interpolates the value, so what
	// ValidateStackFiles inspected is again not what starts the containers. The file is named
	// instead of a variable, because a body reference has no variable name to be named by.
	//
	// Read through the file service and by relative path, as ValidateStackFiles and
	// ValidateComposeURLs read the very same files a few lines below.
	for _, file := range stackutils.GetStackFilePaths(stack, false) {
		content, err := fileService.GetFileContent(stack.ProjectPath, file)
		if err != nil {
			// Fail closed: a body that cannot be read cannot be scanned.
			return fmt.Errorf("stack %q: failed to read the compose file %q: %w",
				secretresolver.TruncateName(stack.Name), secretresolver.TruncateName(file), err)
		}

		refs, err := secretresolver.ComposeReferences(content)
		if err != nil {
			return fmt.Errorf("stack %q: compose file %q: %w",
				secretresolver.TruncateName(stack.Name), secretresolver.TruncateName(file), err)
		}

		if len(refs) > 0 {
			return fmt.Errorf("stack %q: compose file %q uses a secret reference, and a stack carrying secret references can only be deployed by an administrator or an environment administrator",
				secretresolver.TruncateName(stack.Name), secretresolver.TruncateName(file))
		}
	}

	return nil
}

func (config *ComposeStackDeploymentConfig) Undeploy(ctx context.Context) error {
	if config.StackDeployer == nil {
		log.Debug().Msg("stack deployer is not initialized")
		return errors.New("stack deployer cannot be nil")
	}

	if stackutils.IsRelativePathStack(config.stack) {
		return config.StackDeployer.UndeployRemoteComposeStack(ctx, config.stack, config.endpoint)
	}
	return config.StackDeployer.UndeployComposeStack(ctx, config.stack, config.endpoint)
}

func (config *ComposeStackDeploymentConfig) GetResponse() string {
	return ""
}
