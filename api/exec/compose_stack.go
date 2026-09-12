package exec

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/filesystem"
	"github.com/portainer/portainer/api/http/proxy"
	"github.com/portainer/portainer/api/logs"
	"github.com/portainer/portainer/api/stacks/stackutils"
	"github.com/portainer/portainer/pkg/libstack"
	"github.com/portainer/portainer/pkg/secretresolver"

	"github.com/rs/zerolog/log"
	"github.com/segmentio/encoding/json"
)

// secretFetcher resolves opaque secret references into values. It is declared here
// rather than consumed as a concrete type so the deploy path can be tested without
// a resolver service.
type secretFetcher interface {
	Fetch(ctx context.Context, refs []string) (map[string]string, error)
}

// ComposeStackManager is a wrapper for docker-compose binary
type ComposeStackManager struct {
	deployer     libstack.Deployer
	proxyManager *proxy.Manager
	// secretResolver is nil unless PORTAINER_SECRET_RESOLVER is set, which is the
	// state of any deployment whose stacks still carry literal env values.
	secretResolver secretFetcher

	// statEnvFile is os.Stat when nil, which it is everywhere but in the test that proves
	// warnAboutStaleEnvFile performs no filesystem call at all for a stack that uses no
	// secret references. That property - no extra I/O on the unpatched path - is otherwise
	// held only by the order of the two len() checks, which a refactor can reorder without
	// anything noticing.
	//
	// A field and not a package-level variable: a global seam is shared state between
	// tests, so it silently forbids t.Parallel on this test and on every neighbouring one
	// that happens to deploy a stack.
	statEnvFile func(name string) (os.FileInfo, error)
}

// NewComposeStackManager returns a Compose stack manager
func NewComposeStackManager(deployer libstack.Deployer, proxyManager *proxy.Manager) *ComposeStackManager {
	manager := &ComposeStackManager{
		deployer:     deployer,
		proxyManager: proxyManager,
	}

	// The resolver is configured from the environment rather than taken as an
	// argument: this constructor is called from the composition root, and a changed
	// signature is a conflict that every future rebase of this fork has to resolve.
	//
	// FromEnv returns a typed nil *Client when no resolver is configured. Assigning
	// that straight into the interface field would yield a non-nil interface holding
	// a nil pointer, so the "no resolver configured" check in resolveStackSecrets
	// would silently pass and the deploy would go on to call Fetch.
	//
	// Client.Fetch guards its own nil receiver and fails closed, so this is not the
	// only thing standing between a reference and a container - do not delete that
	// guard on the strength of this one, or the other way round. What this assignment
	// buys is the accurate diagnosis: "no secret resolver is configured, set
	// PORTAINER_SECRET_RESOLVER" rather than a resolver failure on a resolver that
	// was never there.
	if resolver := secretresolver.FromEnv(); resolver != nil {
		manager.secretResolver = resolver
	}

	return manager
}

// ComposeSyntaxMaxVersion returns the maximum supported version of the docker compose syntax
func (manager *ComposeStackManager) ComposeSyntaxMaxVersion() string {
	return portainer.ComposeSyntaxMaxVersion
}

// Up builds, (re)creates and starts containers in the background. Wraps `docker-compose up -d` command
func (manager *ComposeStackManager) Up(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, options portainer.ComposeUpOptions) error {
	// Resolved before the proxy is fetched: a deploy that cannot resolve its references
	// has no reason to open an endpoint proxy and close it again. SwarmStackManager.Deploy
	// and DeployRemoteComposeStack refuse a reference in the same order.
	literals, secretEnv, err := manager.resolveStackSecrets(ctx, stack)
	if err != nil {
		return err
	}

	url, proxy, err := fetchEndpointProxy(manager.proxyManager, endpoint)
	if err != nil {
		return fmt.Errorf("failed to fetch environment proxy: %w", err)
	}

	if proxy != nil {
		defer proxy.Close()
	}

	envFilePath, err := manager.prepareEnvFile(stack, literals)
	if err != nil {
		return fmt.Errorf("failed to create env file: %w", err)
	}

	filePaths := stackutils.GetStackFilePaths(stack, true)
	if err = manager.deployer.Deploy(ctx, filePaths, libstack.DeployOptions{
		Options: libstack.Options{
			WorkingDir:  stack.ProjectPath,
			EnvFilePath: envFilePath,
			Env:         secretEnv,
			Host:        url,
			ProjectName: stack.Name,
			Registries:  portainerRegistriesToAuthConfigs(options.Registries),
		},
		ForceRecreate:        options.ForceRecreate,
		AbortOnContainerExit: options.AbortOnContainerExit,
		RemoveOrphans:        options.Prune,
	}); err != nil {
		return deployFailure(stack, secretEnv, "failed to deploy a stack", err)
	}
	return nil
}

// Run runs a one-off command on a service. Wraps `docker-compose run` command
func (manager *ComposeStackManager) Run(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, serviceName string, options portainer.ComposeRunOptions) error {
	// Before the proxy, for the reason given in Up.
	literals, secretEnv, err := manager.resolveStackSecrets(ctx, stack)
	if err != nil {
		return err
	}

	url, proxy, err := fetchEndpointProxy(manager.proxyManager, endpoint)
	if err != nil {
		return fmt.Errorf("failed to fetch environment proxy: %w", err)
	}

	if proxy != nil {
		defer proxy.Close()
	}

	envFilePath, err := manager.prepareEnvFile(stack, literals)
	if err != nil {
		return fmt.Errorf("failed to create env file: %w", err)
	}

	filePaths := stackutils.GetStackFilePaths(stack, true)
	if err = manager.deployer.Run(ctx, filePaths, serviceName, libstack.RunOptions{
		Options: libstack.Options{
			WorkingDir:  stack.ProjectPath,
			EnvFilePath: envFilePath,
			Env:         secretEnv,
			Host:        url,
			ProjectName: stack.Name,
			Registries:  portainerRegistriesToAuthConfigs(options.Registries),
		},
		Remove:   options.Remove,
		Args:     options.Args,
		Detached: options.Detached,
	}); err != nil {
		return deployFailure(stack, secretEnv, "failed to deploy a stack", err)
	}
	return nil
}

// Down stops and removes containers, networks, images, and volumes
//
// No secrets are resolved here: removal addresses the project by name, with no file
// paths and therefore no compose interpolation, so there is nothing to substitute.
func (manager *ComposeStackManager) Down(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint) error {
	url, proxy, err := fetchEndpointProxy(manager.proxyManager, endpoint)
	if err != nil {
		return fmt.Errorf("failed to fetch environment proxy: %w", err)
	} else if proxy != nil {
		defer proxy.Close()
	}

	if err = manager.deployer.Remove(ctx, stack.Name, nil, libstack.RemoveOptions{
		Options: libstack.Options{
			WorkingDir: "",
			Host:       url,
		},
	}); err != nil {
		return fmt.Errorf("failed to remove a stack: %w", err)
	}
	return nil
}

// Pull an image associated with a service defined in a docker-compose.yml or docker-stack.yml file,
// but does not start containers based on those images.
func (manager *ComposeStackManager) Pull(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, options portainer.ComposeOptions) error {
	// Before the proxy, for the reason given in Up.
	literals, secretEnv, err := manager.resolveStackSecrets(ctx, stack)
	if err != nil {
		return err
	}

	url, proxy, err := fetchEndpointProxy(manager.proxyManager, endpoint)
	if err != nil {
		return fmt.Errorf("failed to fetch environment proxy: %w", err)
	} else if proxy != nil {
		defer proxy.Close()
	}

	envFilePath, err := manager.prepareEnvFile(stack, literals)
	if err != nil {
		return fmt.Errorf("failed to create env file: %w", err)
	}

	filePaths := stackutils.GetStackFilePaths(stack, true)
	if err = manager.deployer.Pull(ctx, filePaths, libstack.Options{
		WorkingDir:  stack.ProjectPath,
		EnvFilePath: envFilePath,
		Env:         secretEnv,
		Host:        url,
		ProjectName: stack.Name,
		Registries:  portainerRegistriesToAuthConfigs(options.Registries),
	}); err != nil {
		return deployFailure(stack, secretEnv, "failed to pull images of the stack", err)
	}
	return nil
}

// NormalizeStackName returns a new stack name with unsupported characters replaced
func (manager *ComposeStackManager) NormalizeStackName(name string) string {
	return normalizeStackName(name)
}

// resolveStackSecrets splits stack.Env into the pairs that may be written to disk
// and the ones that must not.
//
// Values marked as references are fetched from the resolver in a single batch and
// returned as "NAME=value" entries for libstack.Options.Env, which compose applies
// with the highest precedence of all its environment sources and which is never
// serialised anywhere. That is the whole point of the design: a resolved value must
// not reach stack.env, the project directory, or a Portainer backup. The remaining
// literal pairs are returned untouched for the env file.
func (manager *ComposeStackManager) resolveStackSecrets(ctx context.Context, stack *portainer.Stack) ([]portainer.Pair, []string, error) {
	var refs []string

	for _, pair := range stack.Env {
		if secretresolver.IsReference(pair.Value) {
			refs = append(refs, pair.Value)
		}
	}

	// The path every stack that has not migrated to references takes: no I/O, no
	// allocation, exactly the behaviour of the unpatched code. unescapeLiterals returns
	// the slice untouched unless something in it actually uses the escape.
	if len(refs) == 0 {
		return unescapeLiterals(stack.Name, stack.Env), nil, nil
	}

	// Never fall through to deploying the reference itself. A container started with
	// the literal string "secret:..." as its password is a service quietly running on
	// a garbage credential, which is far worse than a refused deploy.
	if manager.secretResolver == nil {
		return nil, nil, fmt.Errorf("stack %q uses secret references but no secret resolver is configured, set %s", secretresolver.TruncateName(stack.Name), secretresolver.EndpointEnvVar)
	}

	values, err := manager.secretResolver.Fetch(ctx, refs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve secret references of stack %q: %w", secretresolver.TruncateName(stack.Name), err)
	}

	literals := make([]portainer.Pair, 0, len(stack.Env))
	env := make([]string, 0, len(refs))

	for _, pair := range stack.Env {
		if !secretresolver.IsReference(pair.Value) {
			literals = append(literals, pair)

			continue
		}

		value, ok := values[pair.Value]
		if !ok {
			// Fetch already guarantees a value for every requested reference, so
			// reaching here means a broken client rather than a resolver answer. Fail
			// anyway rather than let the variable through undefined.
			//
			// %q and bounded, like every other site that names one of these in a message
			// persisted as the stack's deployment status and read back by agents. See
			// secretresolver.TruncateName.
			return nil, nil, fmt.Errorf("stack %q: no value resolved for variable %q", secretresolver.TruncateName(stack.Name), secretresolver.TruncateName(pair.Name))
		}

		env = append(env, pair.Name+"="+value)
	}

	return unescapeLiterals(stack.Name, literals), env, nil
}

// unescapeLiterals returns the pairs with every escaped marker turned back into the
// literal it stands for, so a value that only looks like a reference reaches compose as
// the text the operator meant. See secretresolver.Unescape for the doubling rule.
//
// The input slice is returned as it is when nothing needs unescaping, which is every
// stack that has never used the escape.
func unescapeLiterals(stackName string, pairs []portainer.Pair) []portainer.Pair {
	escaped := 0

	for _, pair := range pairs {
		if secretresolver.Unescape(pair.Value) != pair.Value {
			escaped++
		}
	}

	if escaped == 0 {
		return pairs
	}

	// The one silent effect of the marker, so it is announced. See LogEscapedValues.
	secretresolver.LogEscapedValues(stackName, escaped)

	unescaped := make([]portainer.Pair, 0, len(pairs))

	for _, pair := range pairs {
		pair.Value = secretresolver.Unescape(pair.Value)
		unescaped = append(unescaped, pair)
	}

	return unescaped
}

const (
	// redactedPlaceholder replaces a resolved secret value in the server log.
	redactedPlaceholder = "***"

	// minRedactableLength is the shortest string that may be replaced. Anything
	// shorter matches unrelated text all over a message, so replacing it would shred
	// the log line into placeholders and destroy the only diagnostic channel a
	// withheld deploy error has left. A value below it therefore disables the log
	// line entirely rather than being redacted badly; any other form below it - a
	// fragment, a lowercased value - is simply dropped from the list of forms.
	minRedactableLength = 4
)

// withheldError reports a failed deploy of a stack that resolved secret references,
// carrying none of the deployer's own text.
//
// Compose reports several validation failures by quoting the offending value:
// "invalid containerPort: <value>" from the port parser, the %q-quoted forms from
// interpolation, service validation and the schema. Whatever that error says is
// stored by stackutils.UpdateStackStatusFromDeploymentResult as the stack's
// deployment status message and served back by StackInspect, so letting it through
// would put a live credential permanently into Portainer's database and API -
// precisely the channel this feature exists to close. A mistyped reference is the
// likeliest trigger.
//
// Redacting that text instead was tried and cannot be made safe. The value reaches
// the message escaped by strconv.Quote on some paths, and as a ":"- or "/"-split,
// lowercased fragment on others, so searching for the value does not necessarily
// find it. The error text is untrusted data with respect to secret content, and
// untrusted text cannot be reliably sanitised by blacklist. It is therefore withheld
// in full, with a redacted copy sent to the server log as a defence in depth.
type withheldError struct {
	err       error
	operation string
	stackName string
}

// Error returns a fixed description built from the stack name and the operation.
// Nothing derived from the deployer's text appears in it.
//
// "Fixed" was true of the operation, which is a literal at every call site, and not of the
// stack name, which arrives in the request body and had no bound: a mebibyte of it came back
// as a 1048781-byte message, persisted. Bounded here rather than at the two field
// assignments, so that the bound sits at the format verb like every other one on this path;
// see secretresolver.TruncateName.
func (e *withheldError) Error() string {
	return fmt.Sprintf("%s %q: the underlying error is withheld because this stack resolves secret references and compose quotes offending values verbatim, see the Portainer server log for the redacted text", e.operation, secretresolver.TruncateName(e.stackName))
}

// Unwrap keeps errors.Is and errors.As working against the deployer's error.
//
// This is safe only as long as nothing in Portainer unwraps a stack deployment error
// and prints the inner one. Every sink on this path renders the outer error through
// Error(): stackutils.UpdateStackStatusFromDeploymentResult, httperror's
// writeErrorResponse, pkg/errors.WithMessagef, and zerolog's .Err/.AnErr. The one
// errors.As on the path, in api/http/handler/stacks/webhook_invoke.go, uses the
// extracted value as a type discriminator and prints the outer error.
//
// The shape to watch for is api/scheduler/scheduler.go, which extracts a
// *PermanentError and logs it: that is harmless only because a PermanentError is
// always constructed above a deploy error, so its Error() delegates back down to
// this one. An extract-and-log wrapper placed BELOW this error would bypass the
// withholding. Re-check that before adding a sink, or drop this method.
func (e *withheldError) Unwrap() error { return e.err }

// deployFailure turns an error from the deployer into the error the caller receives.
//
// A stack that resolved no secret references keeps the unpatched behaviour exactly:
// the deployer's text is wrapped and returned as is. A stack that resolved at least
// one gets a withheldError instead, and the redacted text goes to the server log.
func deployFailure(stack *portainer.Stack, secretEnv []string, operation string, err error) error {
	if len(secretEnv) == 0 {
		return fmt.Errorf("%s: %w", operation, err)
	}

	logWithheldError(stack.Name, secretEnv, err)

	return &withheldError{err: err, operation: operation, stackName: stack.Name}
}

// logWithheldError writes the deployer's error to the server log with every resolved
// secret value removed.
//
// This is the secondary control, not the primary one: the caller never sees this
// text, so a miss here does not reach the database or the API. It is still the only
// place a failed deploy of a stack with secret references can be diagnosed from,
// which is why the text is redacted rather than dropped.
func logWithheldError(stackName string, secretEnv []string, err error) {
	text, ok := redactSecretValues(err.Error(), secretEnv)
	if !ok {
		log.Error().
			Str("stack", stackName).
			Msg("Stack deployment failed, the error text is withheld because a resolved secret value is too short to redact safely")

		return
	}

	log.Error().
		Str("stack", stackName).
		Str("deployer_error", text).
		Msg("Stack deployment failed, resolved secret values are redacted from the error")
}

// redactSecretValues replaces every form in which a resolved secret value can appear
// in text with a placeholder. It reports false when no safe redaction is possible.
//
// Only the resolved values are redacted. The references themselves are not secret and
// are what a diagnosis of a mistyped reference needs, so they are left intact.
func redactSecretValues(text string, secretEnv []string) (string, bool) {
	forms, ok := redactionForms(secretEnv)
	if !ok {
		return "", false
	}

	for _, form := range forms {
		text = strings.ReplaceAll(text, form, redactedPlaceholder)
	}

	return text, true
}

// redactionForms returns every string that has to be replaced to redact the values in
// secretEnv, longest first. It reports false when a value is too short to redact
// safely - see minRedactableLength.
//
// A resolved value does not necessarily appear verbatim in a compose error:
//   - the interpolation, validation and schema paths format it with %q, which escapes
//     quotes, backslashes and control characters;
//   - the port parser reports a ":"- or "/"-split fragment of it, lowercased.
//
// Each of those forms is therefore listed separately. Longest first matters: a short
// form replaced first leaves the rest of a longer form containing it in the text.
func redactionForms(secretEnv []string) ([]string, bool) {
	var forms []string

	seen := make(map[string]struct{})

	add := func(form string) {
		// The floor is enforced here rather than on the value alone, so that no
		// transformation can produce a form below it. strings.ToLower can shorten a
		// string - the Kelvin sign is three bytes and lowercases to a one-byte "k" - and
		// a future form derived some other way would have the same freedom. A form below
		// the floor matches unrelated text all over a message and would shred the log
		// line, which is the only diagnostic a withheld deploy error leaves. This also
		// covers the empty string, which jsonForm returns on a marshalling failure.
		if len(form) < minRedactableLength {
			return
		}

		if _, duplicate := seen[form]; duplicate {
			return
		}

		seen[form] = struct{}{}
		forms = append(forms, form)
	}

	for _, entry := range secretEnv {
		// The entries are "NAME=value" and a value may itself contain "=", so the value
		// is everything past the first one. An empty value is skipped: replacing the
		// empty string would shred the whole message.
		_, value, found := strings.Cut(entry, "=")
		if !found || value == "" {
			continue
		}

		if len(value) < minRedactableLength {
			return nil, false
		}

		for _, part := range append([]string{value}, longFragments(value)...) {
			add(part)
			add(quotedForm(part))
			add(jsonForm(part))
			add(strings.ToLower(part))
		}
	}

	slices.SortFunc(forms, func(a, b string) int {
		if diff := len(b) - len(a); diff != 0 {
			return diff
		}

		return strings.Compare(a, b)
	})

	return forms, true
}

// longFragments returns the parts of a value that compose's port parser can report on
// their own, keeping only the ones long enough to redact safely.
func longFragments(value string) []string {
	fragments := strings.FieldsFunc(value, func(r rune) bool {
		return r == ':' || r == '/' || r == ','
	})

	return slices.DeleteFunc(fragments, func(fragment string) bool {
		return len(fragment) < minRedactableLength
	})
}

// quotedForm returns the value as it appears inside a %q-formatted message, without
// the quotes strconv.Quote puts around it.
func quotedForm(value string) string {
	quoted := strconv.Quote(value)

	return quoted[1 : len(quoted)-1]
}

// jsonForm returns the value as it appears inside a JSON string, without the quotes.
// It differs from quotedForm for the characters encoding/json escapes and strconv
// does not, "<" and "&" among them.
func jsonForm(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}

	return string(encoded[1 : len(encoded)-1])
}

// stackEnvFileName is the file createEnvFile writes a stack's environment variables
// to, inside the stack's project directory.
const stackEnvFileName = "stack.env"

// stackEnvFilePath returns the path of a stack's env file.
func stackEnvFilePath(stack *portainer.Stack) string {
	return filesystem.JoinPaths(stack.ProjectPath, stackEnvFileName)
}

// prepareEnvFile is what the deploy paths call: createEnvFile plus the one check that
// only makes sense on a deploy.
//
// The check needs both lists - what the stack declares and what may be written - and
// createEnvFile takes its pairs as an argument precisely so that it never has to look
// at stack.Env. So the comparison lives here, where both are in hand.
func (manager *ComposeStackManager) prepareEnvFile(stack *portainer.Stack, literals []portainer.Pair) (string, error) {
	manager.warnAboutStaleEnvFile(stack, literals)

	return createEnvFile(stack, literals)
}

// warnAboutStaleEnvFile reports an env file left behind by a stack whose variables
// have all migrated to secret references.
//
// createEnvFile writes nothing when there is no literal pair left, which is exactly
// the state a fully migrated stack reaches - so the stack.env written by the last
// pre-migration deploy stays in the project directory with the pre-migration values
// in it, and the project directory is in filesToBackup, so it keeps riding out in
// every POST /backup.
//
// Nothing here touches that file, neither unlinking nor truncating it. The operator is
// told where the file is instead.
//
// The message does not tell the operator to delete the file, and deliberately so: the
// firing condition cannot tell Portainer's own stack.env from the one a git-deployed
// stack brings in its clone, which may be referenced by an env_file: directive and comes
// back on the next pull anyway. Nothing removes the file either, so a webhook-deployed
// stack prints this on every deploy; suppressing the repeat would take state this check
// deliberately does not keep.
func (manager *ComposeStackManager) warnAboutStaleEnvFile(stack *portainer.Stack, literals []portainer.Pair) {
	if len(stack.Env) == 0 || len(literals) > 0 {
		return
	}

	stat := manager.statEnvFile
	if stat == nil {
		stat = os.Stat
	}

	envFilePath := stackEnvFilePath(stack)
	if _, err := stat(envFilePath); err != nil {
		return
	}

	log.Warn().
		Str("stack", stack.Name).
		Str("path", envFilePath).
		Msg("A stack env file is on disk for a stack whose variables have all migrated to secret references, it may hold pre-migration values")
}

// createEnvFile creates a file that would hold both "in-place" and default environment variables.
// It will return the name of the file if the stack has "in-place" env vars, otherwise empty string.
//
// The pairs are passed in rather than read from stack.Env so that a caller can keep
// resolved secrets - and the references themselves - out of the file. When every
// pair was a reference the list is empty and no file is written at all, which leaves
// compose falling back to the project's own .env exactly as it does for a stack with
// no env vars.
//
// Nothing is ever deleted here. <ProjectPath>/stack.env is not necessarily a file
// Portainer wrote: a stack deployed from a repository is documented to carry its own
// stack.env in the git clone, which lands in exactly this directory. A deploy must
// not delete a file it did not create, and the empty branch is taken by every stack
// with no environment variables at all.
func createEnvFile(stack *portainer.Stack, env []portainer.Pair) (string, error) {
	if len(env) == 0 {
		return "", nil
	}

	envFilePath := stackEnvFilePath(stack)
	envfile, err := os.OpenFile(envFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer logs.CloseAndLogErr(envfile)

	// Copy from default .env file
	defaultEnvPath := path.Join(stack.ProjectPath, path.Dir(stack.EntryPoint), ".env")
	if err := copyDefaultEnvFile(envfile, defaultEnvPath); err != nil {
		return "", err
	}

	// Copy from stack env vars
	if err := copyConfigEnvVars(envfile, env); err != nil {
		return "", err
	}

	return envFilePath, nil
}

// copyDefaultEnvFile copies the default .env file if it exists to the provided writer
func copyDefaultEnvFile(w io.Writer, defaultEnvFilePath string) error {
	defaultEnvFile, err := os.Open(defaultEnvFilePath)
	if err != nil {
		// If cannot open a default file, then don't need to copy it.
		// We could as well stat it and check if it exists, but this is more efficient.
		return nil
	}

	defer logs.CloseAndLogErr(defaultEnvFile)

	if _, err = io.Copy(w, defaultEnvFile); err == nil {
		if _, err = fmt.Fprintf(w, "\n"); err != nil {
			return fmt.Errorf("failed to copy default env file: %w", err)
		}
	}

	return nil
	// If couldn't copy the .env file, then ignore the error and try to continue
}

// copyConfigEnvVars write the environment variables from stack configuration to the writer
func copyConfigEnvVars(w io.Writer, envs []portainer.Pair) error {
	for _, v := range envs {
		if _, err := fmt.Fprintf(w, "%s=%s\n", v.Name, v.Value); err != nil {
			return fmt.Errorf("failed to copy config env vars: %w", err)
		}
	}

	return nil
}
