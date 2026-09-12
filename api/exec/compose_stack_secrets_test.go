package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/pkg/libstack"
	"github.com/portainer/portainer/pkg/libstack/swarm"
	"github.com/portainer/portainer/pkg/secretresolver"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	tokenRef    = "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"
	tokenValue  = "resolved-admin-token"
	metricsRef  = "secret:vw:stack/nebula/arcextension/METRICS_TOKEN"
	metricsVal  = "resolved-metrics-token"
	literalName = "LOG_LEVEL"
	literalVal  = "debug"

	// distinctValue shares no substring with the fixed text of a withheld error, so a
	// test can assert that not one of its dash-separated parts survived.
	distinctValue = "Xk7pQw3-Zt9mLr5-Vb2nHd8"
)

// stubFetcher stands in for the resolver client so the deploy path can be tested
// without a resolver service.
type stubFetcher struct {
	values map[string]string
	err    error
	calls  [][]string
}

func (f *stubFetcher) Fetch(ctx context.Context, refs []string) (map[string]string, error) {
	f.calls = append(f.calls, refs)

	if f.err != nil {
		return nil, f.err
	}

	return f.values, nil
}

// failingFetcher fails the test if it is called at all.
type failingFetcher struct {
	t *testing.T
}

func (f *failingFetcher) Fetch(ctx context.Context, refs []string) (map[string]string, error) {
	f.t.Helper()
	f.t.Error("the secret resolver must not be called on this path")

	return nil, errors.New("must not be called")
}

// stubDeployer records the options it was called with and returns err, if set.
type stubDeployer struct {
	err            error
	deployOptions  libstack.DeployOptions
	deployPaths    []string
	deployContents [][]byte
	runOptions     libstack.RunOptions
	pullOptions    libstack.Options
}

func (d *stubDeployer) Deploy(ctx context.Context, filePaths []string, options libstack.DeployOptions) error {
	d.deployOptions = options
	d.deployPaths = filePaths

	// A rewritten copy is deleted the moment the deploy returns, so what compose was
	// handed can only be captured from in here.
	d.deployContents = make([][]byte, 0, len(filePaths))

	for _, filePath := range filePaths {
		content, _ := os.ReadFile(filePath)
		d.deployContents = append(d.deployContents, content)
	}

	return d.err
}

func (d *stubDeployer) Run(ctx context.Context, filePaths []string, serviceName string, options libstack.RunOptions) error {
	d.runOptions = options

	return d.err
}

func (d *stubDeployer) Pull(ctx context.Context, filePaths []string, options libstack.Options) error {
	d.pullOptions = options

	return d.err
}

func (d *stubDeployer) Remove(ctx context.Context, projectName string, filePaths []string, options libstack.RemoveOptions) error {
	return nil
}

func (d *stubDeployer) Validate(ctx context.Context, filePaths []string, options libstack.Options) error {
	return nil
}

func (d *stubDeployer) WaitForStatus(ctx context.Context, name string, status libstack.Status) libstack.WaitResult {
	return libstack.WaitResult{Status: status}
}

func (d *stubDeployer) Config(ctx context.Context, filePaths []string, options libstack.Options) ([]byte, error) {
	return nil, nil
}

func (d *stubDeployer) GetExistingEdgeStacks(ctx context.Context) ([]libstack.EdgeStack, error) {
	return nil, nil
}

// stubSwarmDeployer records the options it was called with.
type stubSwarmDeployer struct {
	options swarm.DeployOptions
}

func (d *stubSwarmDeployer) Deploy(ctx context.Context, filePaths []string, options swarm.DeployOptions) error {
	d.options = options

	return nil
}

func (d *stubSwarmDeployer) Remove(ctx context.Context, projectName string, options swarm.RemoveOptions) error {
	return nil
}

func (d *stubSwarmDeployer) Validate(ctx context.Context, filePaths []string, options swarm.Options) error {
	return nil
}

func (d *stubSwarmDeployer) WaitForStatus(ctx context.Context, projectName string, options swarm.Options, status libstack.Status) libstack.WaitResult {
	return libstack.WaitResult{Status: status}
}

// plainComposeBody carries no secret reference, so a fixture built on it exercises the
// env-field path alone.
const plainComposeBody = "services:\n  app:\n    image: nginx\n"

// newStackProject returns a project directory holding body as the stack's entry point.
//
// Every fixture deployed through the compose path needs one: that path now reads each of
// the stack's compose files to find the references written inline in them, and it fails
// closed on a file it cannot read.
func newStackProject(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(body), 0o600))

	return dir
}

// newSecretStack returns a stack mixing a literal env var with two references.
func newSecretStack(t *testing.T) *portainer.Stack {
	t.Helper()

	return &portainer.Stack{
		Name:        "arcextension",
		ProjectPath: newStackProject(t, plainComposeBody),
		EntryPoint:  "docker-compose.yml",
		Env: []portainer.Pair{
			{Name: literalName, Value: literalVal},
			{Name: "ADMIN_TOKEN", Value: tokenRef},
			{Name: "METRICS_TOKEN", Value: metricsRef},
		},
	}
}

func newFetcher() *stubFetcher {
	return &stubFetcher{values: map[string]string{tokenRef: tokenValue, metricsRef: metricsVal}}
}

// localEndpoint needs no proxy, so the manager can run with a nil proxy manager.
func localEndpoint() *portainer.Endpoint {
	return &portainer.Endpoint{URL: "unix://"}
}

func readStackEnvFile(t *testing.T, stack *portainer.Stack) string {
	t.Helper()

	content, err := os.ReadFile(stackEnvFilePath(stack))
	require.NoError(t, err)

	return string(content)
}

func Test_resolveStackSecrets(t *testing.T) {
	t.Parallel()

	t.Run("partitions literals and references", func(t *testing.T) {
		t.Parallel()

		fetcher := newFetcher()
		manager := &ComposeStackManager{secretResolver: fetcher}

		secrets, err := manager.resolveStackSecrets(t.Context(), newSecretStack(t))
		require.NoError(t, err)

		defer secrets.cleanup()

		assert.Equal(t, []portainer.Pair{{Name: literalName, Value: literalVal}}, secrets.literals)
		assert.Equal(t, []string{"ADMIN_TOKEN=" + tokenValue, "METRICS_TOKEN=" + metricsVal}, secrets.env)
		require.Len(t, fetcher.calls, 1)
		assert.Equal(t, []string{tokenRef, metricsRef}, fetcher.calls[0])
	})

	t.Run("does nothing without references", func(t *testing.T) {
		t.Parallel()

		fetcher := newFetcher()
		manager := &ComposeStackManager{secretResolver: fetcher}
		stack := &portainer.Stack{
			Name:        "plain",
			ProjectPath: newStackProject(t, plainComposeBody),
			EntryPoint:  "docker-compose.yml",
			Env:         []portainer.Pair{{Name: "VAR1", Value: "value1"}},
		}

		secrets, err := manager.resolveStackSecrets(t.Context(), stack)
		require.NoError(t, err)

		defer secrets.cleanup()

		assert.Equal(t, stack.Env, secrets.literals)
		assert.Nil(t, secrets.env)
		assert.Empty(t, fetcher.calls)

		// Nothing was rewritten, so the stack's own file is deployed and the project
		// directory is the one compose would have derived from it anyway.
		assert.Equal(t, []string{filepath.Join(stack.ProjectPath, "docker-compose.yml")}, secrets.filePaths)
		assert.Equal(t, stack.ProjectPath, secrets.projectDir)
	})

	t.Run("fails when no resolver is configured", func(t *testing.T) {
		t.Parallel()

		manager := &ComposeStackManager{}

		_, err := manager.resolveStackSecrets(t.Context(), newSecretStack(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "arcextension")
		assert.Contains(t, err.Error(), "PORTAINER_SECRET_RESOLVER")
	})

	t.Run("fails when a reference is not resolved", func(t *testing.T) {
		t.Parallel()

		fetcher := &stubFetcher{values: map[string]string{tokenRef: tokenValue}}
		manager := &ComposeStackManager{secretResolver: fetcher}

		_, err := manager.resolveStackSecrets(t.Context(), newSecretStack(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "METRICS_TOKEN")
	})
}

// plantedValue stands in for a live credential in the error-path table below. It is
// one token with no separators and no English words, so a substring assertion against
// it cannot match the fixed text of a message by accident.
const plantedValue = "Zt9mLr5Vb2nHd8Xk7pQw3"

// resolverAt returns a real secretresolver client - not a stub - talking to handler.
//
// The table below is as much a pin on the client's error strings as on this package's,
// and a stub would pin neither: the whole point is that the two halves cannot drift
// apart without the test noticing.
//
// The endpoint carries no credential, and cannot: secretresolver.New refuses one, because
// the endpoint is PORTAINER_SECRET_RESOLVER and compose interpolates that into every
// project. What it can still carry into these messages is its host and port - an ephemeral
// httptest one here, which is why the cases built on this helper assert on the diagnostic
// they must keep rather than on the address. The two cases that do assert on a host and port
// name a fixed one and are configured through the environment, not through this helper.
func resolverAt(t *testing.T, timeout time.Duration, handler http.HandlerFunc) secretFetcher {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return resolverFor(t, server.URL, timeout)
}

// resolverFor returns a real client for an endpoint that need not answer.
func resolverFor(t *testing.T, endpoint string, timeout time.Duration) secretFetcher {
	t.Helper()

	client, err := secretresolver.New(endpoint, timeout)
	require.NoError(t, err)

	return client
}

// assertNoValueLeak fails when text carries value in any form in which a value can
// really turn up in foreign text.
//
// A raw case-sensitive substring search is weaker than the claim it stands for: it would
// miss a value that arrived lowercased, %q- or JSON-escaped, or as a fragment that does
// not start at byte 0. The forms therefore come from redactionForms - the production
// code's own enumeration of exactly those transformations - so the assertion cannot fall
// behind what the neighbouring code already knows about the threat.
//
// On top of the forms, every window of the value of minRedactableLength bytes is checked
// case-insensitively, which catches a fragment split some way nothing has thought of yet.
// The shortest windows are enough: any leak of at least that many bytes contains one.
func assertNoValueLeak(t *testing.T, text, value string) {
	t.Helper()

	forms, ok := redactionForms([]string{"PLANTED=" + value})
	require.True(t, ok)

	lowered := strings.ToLower(text)

	for _, form := range forms {
		assert.NotContainsf(t, lowered, strings.ToLower(form), "the text carries the planted value in the form %q", form)
	}

	for start := 0; start+minRedactableLength <= len(value); start++ {
		window := value[start : start+minRedactableLength]

		assert.NotContainsf(t, lowered, strings.ToLower(window), "the text carries the window %q of the planted value", window)
	}
}

// Test_resolveStackSecrets_errorPathsCarryNoSecretValue drives every error that can
// leave resolveStackSecrets and asserts that none of them carries a resolved value.
//
// This is the pin for the classification recorded in docs/secret-resolver.md §3.12:
// these errors deliberately do not go through the withholding mechanism - at the point
// they are raised nothing has been resolved yet, so redactSecretValues would have
// nothing to redact against - and they are safe only because every component of every
// message is built by Portainer from data that is already public in the stack config:
// the stack name, a variable name, a reference, an env var name, an HTTP status. The
// single exception is the resolver's own error field, which is contractually free of
// secret values and bounded in size by the client.
//
// So if a new error path ever starts carrying a value, this test fails.
func Test_resolveStackSecrets_errorPathsCarryNoSecretValue(t *testing.T) {
	// One case needs a misconfigured client, which is reachable only through the
	// environment, and t.Setenv forbids t.Parallel.
	t.Setenv(secretresolver.EndpointEnvVar, "unix:///run/secret-resolver/resolver.sock")
	t.Setenv(secretresolver.TimeoutEnvVar, "fifteen seconds")

	misconfigured := secretresolver.FromEnv()
	require.NotNil(t, misconfigured)

	// A second broken configuration, this one with a credential in the endpoint. configErr
	// is returned by every Fetch, so a misconfigured endpoint does not leak once at startup
	// but on every deploy of every stack that uses references.
	t.Setenv(secretresolver.EndpointEnvVar, "tcp://portainer:"+plantedValue+"@resolver.example:9100")
	t.Setenv(secretresolver.TimeoutEnvVar, "")

	misconfiguredEndpoint := secretresolver.FromEnv()
	require.NotNil(t, misconfiguredEndpoint)

	// A third, and the one that reaches the refusal added for the credential channel rather
	// than the unsupported-scheme branch above: a supported scheme whose userinfo carries a
	// credential. New refuses it, and the refusal is a configErr like the two before it, so
	// its text is published on every deploy of every stack that uses references.
	t.Setenv(secretresolver.EndpointEnvVar, "http://portainer:"+plantedValue+"@resolver.example:9100")
	t.Setenv(secretresolver.TimeoutEnvVar, "")

	refusedCredential := secretresolver.FromEnv()
	require.NotNil(t, refusedCredential)

	unreachable, err := secretresolver.New("unix:///nonexistent/secret-resolver.sock", time.Second)
	require.NoError(t, err)

	tests := []struct {
		name     string
		resolver secretFetcher
		// contains is the diagnostic the message must keep, so that the assertions
		// cannot be satisfied by a message that says nothing at all.
		contains string
	}{
		{
			name:     "no resolver is configured",
			resolver: nil,
			contains: secretresolver.EndpointEnvVar,
		},
		{
			name:     "the resolver configuration is broken",
			resolver: misconfigured,
			contains: secretresolver.TimeoutEnvVar,
		},
		{
			name:     "the resolver configuration names an endpoint carrying a credential",
			resolver: misconfiguredEndpoint,
			contains: "resolver.example:9100",
		},
		{
			name:     "the resolver endpoint is refused for carrying a credential",
			resolver: refusedCredential,
			contains: "resolver.example:9100",
		},
		{
			name:     "the resolver cannot be reached",
			resolver: unreachable,
			contains: "failed to reach",
		},
		{
			// A resolver that is down or unreachable is the commonest failure this feature
			// has, and it is the one whose message is built by net/http rather than by this
			// package - so it is driven through the http branch as well as the unix one
			// above. Nothing listens on port 1, so the dial is refused rather than hung.
			name:     "the resolver cannot be reached over http",
			resolver: resolverFor(t, "http://127.0.0.1:1", time.Second),
			contains: "failed to reach",
		},
		{
			name: "the resolver does not answer in time",
			resolver: resolverAt(t, 50*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
				// Released by the client's own deadline closing the connection; the timer is
				// only a backstop so a failing case cannot wedge the suite.
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			}),
			// The client no longer wraps a transport error - net/http quotes
			// resolver-written header text in those - so the message carries its own
			// classification of the failure rather than "context deadline exceeded". The
			// sentinel is still reachable with errors.Is; see
			// TestFetchClassifiesATransportFailure in pkg/secretresolver.
			contains: "no answer within",
		},
		{
			name: "the resolver fails without an error field",
			resolver: resolverAt(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}),
			contains: "500",
		},
		{
			name: "the resolver fails with an error field",
			resolver: resolverAt(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":"vault sync failed"}`))
			}),
			contains: "vault sync failed",
		},
		{
			name: "the resolver answers 200 with an error field",
			resolver: resolverAt(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"error":"vault is locked"}`))
			}),
			contains: "vault is locked",
		},
		{
			name: "the response cannot be decoded",
			resolver: resolverAt(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
				// The JSON decoder quotes the first bytes of the buffer it choked on, and
				// that buffer is the response body. The value sits at byte 6 here, well
				// inside that window, so a decoder error let through would be caught below.
				_, _ = w.Write([]byte(`{"x":"` + plantedValue))
			}),
			contains: "decode",
		},
		{
			name: "the response is over the size cap",
			resolver: resolverAt(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"values":{"` + plantedValue))
				_, _ = w.Write([]byte(strings.Repeat("a", 2<<20)))
			}),
			contains: "exceeds",
		},
		{
			name: "the resolver skips a reference",
			resolver: resolverAt(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"values":{%q:%q}}`, tokenRef, plantedValue)
			}),
			// The reference is echoed: it is a lookup key, not a secret, and it is what a
			// diagnosis of a mistyped reference needs.
			contains: metricsRef,
		},
		{
			name: "the resolver returns an empty value",
			resolver: resolverAt(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"values":{%q:"",%q:%q}}`, tokenRef, metricsRef, plantedValue)
			}),
			contains: tokenRef,
		},
		{
			// Unreachable through the real client, which guarantees a value for every
			// requested reference, so this last path needs a stub to be exercised at all.
			name:     "a variable is left without a value",
			resolver: &stubFetcher{values: map[string]string{tokenRef: plantedValue}},
			contains: "METRICS_TOKEN",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &ComposeStackManager{secretResolver: test.resolver}

			_, err := manager.resolveStackSecrets(t.Context(), newSecretStack(t))
			require.Error(t, err)

			assertNoValueLeak(t, err.Error(), plantedValue)

			assert.Contains(t, err.Error(), "arcextension")
			assert.Contains(t, err.Error(), test.contains)
		})
	}
}

func Test_Up_resolvesSecretsIntoOptionsEnv(t *testing.T) {
	t.Parallel()

	stack := newSecretStack(t)
	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	// Options.Env is the only channel that carries a resolved value: it has the
	// highest precedence in compose and is never written anywhere.
	assert.Equal(t, []string{"ADMIN_TOKEN=" + tokenValue, "METRICS_TOKEN=" + metricsVal}, deployer.deployOptions.Env)

	// A reference is a lookup key, never a value: it must never be handed to compose
	// as one, or a service would start with the literal "secret:..." as its password.
	for _, entry := range deployer.deployOptions.Env {
		assert.NotContains(t, entry, secretresolver.ReferencePrefix)
	}

	// The env file keeps the literals and nothing else - neither the references nor
	// the values they resolve to may reach disk.
	content := readStackEnvFile(t, stack)
	assert.Equal(t, literalName+"="+literalVal+"\n", content)
	assert.NotContains(t, content, tokenRef)
	assert.NotContains(t, content, metricsRef)
	assert.NotContains(t, content, tokenValue)
	assert.NotContains(t, content, metricsVal)
}

func Test_Down_neverResolvesSecrets(t *testing.T) {
	t.Parallel()

	// Removal addresses the project by name and passes no file paths, so there is
	// nothing to interpolate and no reason to hand a live credential to compose.
	manager := &ComposeStackManager{
		deployer:       &stubDeployer{},
		secretResolver: &failingFetcher{t: t},
	}

	require.NoError(t, manager.Down(t.Context(), newSecretStack(t), localEndpoint()))
}

// upWithDeployerError deploys a stack whose single variable is a reference resolving
// to value, against a deployer that fails with deployerErr, and returns the error the
// caller gets.
func upWithDeployerError(t *testing.T, value string, deployerErr error) error {
	t.Helper()

	stack := &portainer.Stack{
		Name:        "arcextension",
		ProjectPath: newStackProject(t, plainComposeBody),
		EntryPoint:  "docker-compose.yml",
		Env:         []portainer.Pair{{Name: "ADMIN_TOKEN", Value: tokenRef}},
	}

	manager := &ComposeStackManager{
		deployer:       &stubDeployer{err: deployerErr},
		secretResolver: &stubFetcher{values: map[string]string{tokenRef: value}},
	}

	err := manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{})
	require.Error(t, err)

	return err
}

func Test_Up_withholdsTheDeployerErrorWhenSecretsWereResolved(t *testing.T) {
	t.Parallel()

	t.Run("names the stack and the operation and keeps no part of the value", func(t *testing.T) {
		t.Parallel()

		err := upWithDeployerError(t, distinctValue, fmt.Errorf("invalid containerPort: %s", distinctValue))

		assert.NotContains(t, err.Error(), distinctValue)
		assert.Contains(t, err.Error(), "arcextension")
		assert.Contains(t, err.Error(), "failed to deploy a stack")

		// The whole deployer text is dropped, so not even a fragment of the value can
		// survive into the stack's deployment status message.
		for fragment := range strings.SplitSeq(distinctValue, "-") {
			assert.NotContains(t, err.Error(), fragment)
		}
	})

	t.Run("withholds an escaped value", func(t *testing.T) {
		t.Parallel()

		// compose-go formats several errors with %q - loader/validate.go, types/project.go,
		// types/device.go and the strconv.NumError of a failed toInt all do - so a value
		// holding a quote or a backslash reaches the message escaped and a raw-substring
		// search misses it entirely.
		value := `Xk7p"Qw3\Zt9mLr5`

		err := upWithDeployerError(t, value, fmt.Errorf("service %q depends on undefined service %q", "web", value))

		assert.NotContains(t, err.Error(), value)
		assert.NotContains(t, err.Error(), quotedForm(value))
	})

	t.Run("withholds a split fragment of a value", func(t *testing.T) {
		t.Parallel()

		// go-connections/nat splits a port spec on ":" and "/" before reporting it, so the
		// message carries a fragment of the value and never the whole of it.
		value := "Ab3xYzKk:Qw7pLmNn"

		err := upWithDeployerError(t, value, errors.New("invalid containerPort: Qw7pLmNn"))

		assert.NotContains(t, err.Error(), "Qw7pLmNn")
	})

	t.Run("withholds a lowercased value", func(t *testing.T) {
		t.Parallel()

		// nat.SplitProtoPort lowercases before reporting.
		value := "AbCdEfGhIj"

		err := upWithDeployerError(t, value, errors.New("invalid containerPort: abcdefghij"))

		assert.NotContains(t, err.Error(), strings.ToLower(value))
	})

	t.Run("withholds a form that redaction provably would not have covered", func(t *testing.T) {
		t.Parallel()

		// This is the case that tells withholding apart from redaction, which every other
		// subtest here would pass either way.
		//
		// redactionForms derives its forms from the whole value and from the fragments
		// left by splitting it on ":", "/" and ",". A value split on "-" produces none of
		// them, so an error carrying one such fragment - and nothing else of the value -
		// goes through redactSecretValues untouched. Only withholding the text protects
		// it, and this subtest fails the moment the deployer's error is let through and
		// merely redacted.
		fragment := "Zt9mLr5"
		require.Contains(t, distinctValue, fragment)

		// The premise itself is asserted, so the case cannot silently stop discriminating
		// if redactionForms ever learns to split on "-".
		redacted, ok := redactSecretValues("invalid containerPort: "+fragment, []string{"ADMIN_TOKEN=" + distinctValue})
		require.True(t, ok)
		require.Contains(t, redacted, fragment)

		err := upWithDeployerError(t, distinctValue, errors.New("invalid containerPort: "+fragment))

		assert.NotContains(t, err.Error(), distinctValue)

		for part := range strings.SplitSeq(distinctValue, "-") {
			assert.NotContains(t, err.Error(), part)
		}
	})

	t.Run("keeps the deployer error reachable through the chain", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("sentinel")

		err := upWithDeployerError(t, distinctValue, fmt.Errorf("invalid containerPort: %s: %w", distinctValue, sentinel))

		require.ErrorIs(t, err, sentinel)
		assert.NotContains(t, err.Error(), distinctValue)

		// Wrapping with %w formats through Error(), so a caller that wraps the withheld
		// error still gets the withheld text.
		wrapped := fmt.Errorf("failed to redeploy: %w", err)
		assert.NotContains(t, wrapped.Error(), distinctValue)
		require.ErrorIs(t, wrapped, sentinel)
	})
}

func Test_Up_keepsTheDeployerErrorWithoutResolvedSecrets(t *testing.T) {
	t.Parallel()

	// A stack that resolves nothing keeps the unpatched behaviour: the deployer's text
	// is the whole diagnostic value of a failed deploy and there is nothing to protect.
	stack := &portainer.Stack{
		Name:        "plain",
		ProjectPath: newStackProject(t, plainComposeBody),
		EntryPoint:  "docker-compose.yml",
		Env:         []portainer.Pair{{Name: "VAR1", Value: "value1"}},
	}

	manager := &ComposeStackManager{
		deployer:       &stubDeployer{err: errors.New("invalid containerPort: 99999")},
		secretResolver: newFetcher(),
	}

	err := manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{})
	require.Error(t, err)
	assert.Equal(t, "failed to deploy a stack: invalid containerPort: 99999", err.Error())
}

func Test_redactSecretValues(t *testing.T) {
	t.Parallel()

	secretEnv := []string{"ADMIN_TOKEN=" + tokenValue, "EMPTY=", "NOEQUALS"}

	t.Run("replaces a resolved value", func(t *testing.T) {
		t.Parallel()

		text, ok := redactSecretValues("invalid containerPort: "+tokenValue, secretEnv)
		require.True(t, ok)
		assert.Equal(t, "invalid containerPort: "+redactedPlaceholder, text)
	})

	t.Run("replaces the escaped form of a value", func(t *testing.T) {
		t.Parallel()

		value := `Xk7p"Qw3\Zt9mLr5`
		env := []string{"ADMIN_TOKEN=" + value}

		text, ok := redactSecretValues(fmt.Sprintf("depends on undefined service %q", value), env)
		require.True(t, ok)
		assert.NotContains(t, text, quotedForm(value))
		assert.NotContains(t, text, value)
	})

	t.Run("replaces a split fragment of a value", func(t *testing.T) {
		t.Parallel()

		env := []string{"ADMIN_TOKEN=Ab3xYzKk:Qw7pLmNn"}

		text, ok := redactSecretValues("invalid containerPort: Qw7pLmNn", env)
		require.True(t, ok)
		assert.Equal(t, "invalid containerPort: "+redactedPlaceholder, text)
	})

	t.Run("replaces the lowercased form of a value", func(t *testing.T) {
		t.Parallel()

		env := []string{"ADMIN_TOKEN=AbCdEfGhIj"}

		text, ok := redactSecretValues("invalid containerPort: abcdefghij", env)
		require.True(t, ok)
		assert.Equal(t, "invalid containerPort: "+redactedPlaceholder, text)
	})

	t.Run("replaces the longest form first", func(t *testing.T) {
		t.Parallel()

		// Replacing the short value first would consume its occurrence inside the long
		// one and leave the rest of the long value - "efgh" here - in the text.
		env := []string{"SHORT=abcd", "LONG=abcdefgh"}

		text, ok := redactSecretValues("invalid containerPort: abcdefgh", env)
		require.True(t, ok)
		assert.Equal(t, "invalid containerPort: "+redactedPlaceholder, text)
		assert.NotContains(t, text, "efgh")
	})

	t.Run("leaves a variable name or a reference alone", func(t *testing.T) {
		t.Parallel()

		// References are lookup keys, not secrets, and they are what a diagnosis of a
		// mistyped reference needs.
		original := "required variable ADMIN_TOKEN is missing a value, ref " + tokenRef

		text, ok := redactSecretValues(original, secretEnv)
		require.True(t, ok)
		assert.Equal(t, original, text)
	})

	t.Run("drops a derived form shorter than the floor", func(t *testing.T) {
		t.Parallel()

		// The Kelvin sign is three bytes and lowercases to a one-byte "k", so a value
		// long enough to redact can still yield a lowercased form below the floor.
		// Replacing that form would shred every unrelated "kk" in the message.
		env := []string{"ADMIN_TOKEN=\u212A\u212A"}

		text, ok := redactSecretValues("the bookkeeper is unrelated text", env)
		require.True(t, ok)
		assert.Equal(t, "the bookkeeper is unrelated text", text)
	})

	t.Run("refuses when a value is too short to redact safely", func(t *testing.T) {
		t.Parallel()

		// Replacing a two-character value would hit unrelated text all over the message
		// and shred it, and leaking it is not acceptable either.
		_, ok := redactSecretValues("invalid containerPort: ab", []string{"ADMIN_TOKEN=ab"})
		assert.False(t, ok)
	})

	t.Run("is a no-op without resolved values", func(t *testing.T) {
		t.Parallel()

		original := "invalid containerPort: " + tokenValue

		text, ok := redactSecretValues(original, nil)
		require.True(t, ok)
		assert.Equal(t, original, text)
	})
}

func Test_createEnvFile_deletesNothingWithoutPairs(t *testing.T) {
	t.Parallel()

	// <ProjectPath>/stack.env is not necessarily a file Portainer wrote: a stack
	// deployed from a repository is documented to carry its own stack.env in the git
	// clone, which lands in the project directory. The empty branch is taken by every
	// stack with no environment variables, so deleting here would break a supported
	// configuration.
	stack := newSecretStack(t)
	repoFile := stackEnvFilePath(stack)
	content := []byte("FROM_THE_GIT_REPOSITORY=1\n")
	require.NoError(t, os.WriteFile(repoFile, content, 0o600))

	path, err := createEnvFile(stack, nil)
	require.NoError(t, err)
	assert.Empty(t, path)

	survived, err := os.ReadFile(repoFile)
	require.NoError(t, err)
	assert.Equal(t, content, survived)
}

func Test_Up_deletesNoExistingEnvFile(t *testing.T) {
	t.Parallel()

	stack := newSecretStack(t)
	stack.Env = stack.Env[1:] // drop the literal pair: every pair is now a reference

	repoFile := stackEnvFilePath(stack)
	content := []byte("FROM_THE_GIT_REPOSITORY=1\n")
	require.NoError(t, os.WriteFile(repoFile, content, 0o600))

	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	// The file is left alone, and compose is not pointed at it either: the resolved
	// values travel through Options.Env only.
	assert.Empty(t, deployer.deployOptions.EnvFilePath)

	survived, err := os.ReadFile(repoFile)
	require.NoError(t, err)
	assert.Equal(t, content, survived)
}

// captureLog points the global zerolog logger at a buffer for the duration of the test.
// A test that uses it must not call t.Parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	previous := log.Logger
	log.Logger = zerolog.New(&buf)

	t.Cleanup(func() { log.Logger = previous })

	return &buf
}

// Test_warnAboutStaleEnvFile pins the four properties the check exists for. The first
// subtest deliberately goes through prepareEnvFile rather than calling the check directly.
//
// It swaps one package-level value - the global logger - so it cannot run in parallel.
// That is safe: the package's parallel tests resume only once every sequential test has
// finished. The os.Stat seam is a field on the manager and needs no such argument.
func Test_warnAboutStaleEnvFile(t *testing.T) {
	t.Run("warns when every variable has migrated and the file is still on disk", func(t *testing.T) {
		buf := captureLog(t)

		stack := newSecretStack(t)
		stack.Env = stack.Env[1:] // drop the literal pair: every pair is now a reference

		require.NoError(t, os.WriteFile(stackEnvFilePath(stack), []byte("ADMIN_TOKEN=pre-migration\n"), 0o600))

		path, err := (&ComposeStackManager{}).prepareEnvFile(stack, nil)
		require.NoError(t, err)
		assert.Empty(t, path)

		output := buf.String()
		assert.Contains(t, output, `"level":"warn"`)
		assert.Contains(t, output, stack.Name)
		assert.Contains(t, output, stackEnvFilePath(stack))
	})

	t.Run("stays quiet when no file was left behind", func(t *testing.T) {
		buf := captureLog(t)

		stack := newSecretStack(t)
		stack.Env = stack.Env[1:]

		path, err := (&ComposeStackManager{}).prepareEnvFile(stack, nil)
		require.NoError(t, err)
		assert.Empty(t, path)

		assert.Empty(t, buf.String())
	})

	t.Run("stays quiet on a partial migration", func(t *testing.T) {
		buf := captureLog(t)

		// One literal is left, so createEnvFile rewrites the file with O_TRUNC and nothing
		// stale survives it. A warning here would be false.
		stack := newSecretStack(t)

		require.NoError(t, os.WriteFile(stackEnvFilePath(stack), []byte("ADMIN_TOKEN=pre-migration\n"), 0o600))

		literals := []portainer.Pair{{Name: literalName, Value: literalVal}}

		path, err := (&ComposeStackManager{}).prepareEnvFile(stack, literals)
		require.NoError(t, err)
		assert.NotEmpty(t, path)

		assert.Empty(t, buf.String())
		assert.Equal(t, literalName+"="+literalVal+"\n", readStackEnvFile(t, stack))
	})

	t.Run("does not touch the filesystem for a stack that cannot be stale", func(t *testing.T) {
		captureLog(t)

		var calls int

		manager := &ComposeStackManager{statEnvFile: func(name string) (os.FileInfo, error) {
			calls++

			return os.Stat(name)
		}}

		// Both branches of the short circuit. The second one - a stack with no variables
		// at all - is the commonest vanilla case there is, and it reaches this check on
		// every deploy of every unmigrated stack.
		for _, env := range [][]portainer.Pair{{{Name: literalName, Value: literalVal}}, nil} {
			plain := &portainer.Stack{Name: "plain", ProjectPath: t.TempDir(), Env: env}
			require.NoError(t, os.WriteFile(stackEnvFilePath(plain), []byte(literalName+"="+literalVal+"\n"), 0o600))

			// No extra I/O on the unpatched path. The file exists, so a stat that happened
			// would be a stat that found something. The property is otherwise held only by
			// the order of the two len() checks.
			manager.warnAboutStaleEnvFile(plain, plain.Env)
			assert.Zero(t, calls)
		}

		// The positive control. Without it, a seam that stopped being used would make the
		// assertions above pass for the wrong reason. How many times it is called is the
		// implementation's business; that it is called at all is the claim.
		migrated := newSecretStack(t)
		migrated.Env = migrated.Env[1:]
		require.NoError(t, os.WriteFile(stackEnvFilePath(migrated), []byte("ADMIN_TOKEN=pre-migration\n"), 0o600))

		manager.warnAboutStaleEnvFile(migrated, nil)
		assert.NotZero(t, calls)
	})
}

func Test_SwarmStackManager_Deploy_refusesSecretReferences(t *testing.T) {
	t.Parallel()

	// Swarm has no resolution path, so a reference would reach a service verbatim.
	manager := &SwarmStackManager{}
	stack := newSecretStack(t)

	err := manager.Deploy(t.Context(), stack, false, false, localEndpoint(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret references")

	// The offending variable is named, the way the compose path names it.
	assert.Contains(t, err.Error(), "ADMIN_TOKEN")
}

const (
	// escapedLiteral is a value that only looks like a reference: some third-party
	// application's own URI-ish configuration format. The doubled colon is the escape.
	escapedLiteral = "secret::app/config"

	// escapedLiteralValue is what compose must end up seeing - one colon shorter.
	escapedLiteralValue = "secret:app/config"
)

func Test_Up_deploysAValueThatOnlyLooksLikeAReference(t *testing.T) {
	t.Parallel()

	// Without the escape such a value is undeployable on this fork, with no workaround
	// available in Portainer's UI: it would be taken for a reference and either fail for
	// want of a resolver or fail inside one that cannot make sense of it.
	fetcher := newFetcher()
	stack := &portainer.Stack{
		Name:        "plain",
		ProjectPath: newStackProject(t, plainComposeBody),
		EntryPoint:  "docker-compose.yml",
		Env:         []portainer.Pair{{Name: "APP_URI", Value: escapedLiteral}},
	}

	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: fetcher}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	// It is not a reference, so the resolver is never called and nothing travels through
	// Options.Env; the env file carries the literal the escape stands for.
	assert.Empty(t, fetcher.calls)
	assert.Nil(t, deployer.deployOptions.Env)
	assert.Equal(t, "APP_URI="+escapedLiteralValue+"\n", readStackEnvFile(t, stack))
}

func Test_resolveStackSecrets_unescapesLiteralsAlongsideReferences(t *testing.T) {
	t.Parallel()

	fetcher := newFetcher()
	manager := &ComposeStackManager{secretResolver: fetcher}

	stack := newSecretStack(t)
	stack.Env = append(stack.Env, portainer.Pair{Name: "APP_URI", Value: escapedLiteral})

	secrets, err := manager.resolveStackSecrets(t.Context(), stack)
	require.NoError(t, err)

	defer secrets.cleanup()

	// The escaped value is unescaped on the path that does resolve references too, not
	// only on the early return.
	assert.Equal(t, []portainer.Pair{
		{Name: literalName, Value: literalVal},
		{Name: "APP_URI", Value: escapedLiteralValue},
	}, secrets.literals)
	assert.Equal(t, []string{"ADMIN_TOKEN=" + tokenValue, "METRICS_TOKEN=" + metricsVal}, secrets.env)

	// Only the two real references were sent to the resolver.
	require.Len(t, fetcher.calls, 1)
	assert.Equal(t, []string{tokenRef, metricsRef}, fetcher.calls[0])
}

func Test_SwarmStackManager_Deploy_unescapesAValueThatOnlyLooksLikeAReference(t *testing.T) {
	t.Parallel()

	deployer := &stubSwarmDeployer{}
	manager := &SwarmStackManager{deployer: deployer}
	stack := &portainer.Stack{
		Name:        "plain",
		ProjectPath: newStackProject(t, plainComposeBody),
		EntryPoint:  "docker-compose.yml",
		Env:         []portainer.Pair{{Name: "APP_URI", Value: escapedLiteral}},
	}

	require.NoError(t, manager.Deploy(t.Context(), stack, false, false, localEndpoint(), nil))

	// Accepted rather than refused, and the escape means here exactly what it means on
	// the compose path - a stack must not change value depending on how it is deployed.
	assert.Equal(t, []string{"APP_URI=" + escapedLiteralValue}, deployer.options.Env)
}

// Test_escapedValuesAreAnnounced pins the one place the marker changes a stack silently.
//
// Every other consequence of it is loud: a reference with no resolver fails the deploy, a
// reference on the swarm or unpacker path is refused by name. But an existing stack whose
// value happens to begin with "secret::" deploys successfully here with a value one colon
// shorter than vanilla Portainer gives it, and without this line nothing would tell the
// operator that the value the container received is not the value the stack stores.
//
// It captures the global logger, so it does not run in parallel.
func Test_escapedValuesAreAnnounced(t *testing.T) {
	escapingStack := func(t *testing.T) *portainer.Stack {
		t.Helper()

		return &portainer.Stack{
			Name:        "arcextension",
			ProjectPath: newStackProject(t, plainComposeBody),
			EntryPoint:  "docker-compose.yml",
			Env: []portainer.Pair{
				{Name: literalName, Value: literalVal},
				{Name: "APP_URI", Value: escapedLiteral},
			},
		}
	}

	t.Run("on the compose path", func(t *testing.T) {
		buf := captureLog(t)

		stack := escapingStack(t)
		manager := &ComposeStackManager{deployer: &stubDeployer{}, secretResolver: &failingFetcher{t: t}}

		require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

		output := buf.String()
		assert.Contains(t, output, stack.Name)
		assert.Contains(t, output, `"variables":1`)
	})

	t.Run("on the swarm path", func(t *testing.T) {
		buf := captureLog(t)

		stack := escapingStack(t)
		manager := &SwarmStackManager{deployer: &stubSwarmDeployer{}}

		require.NoError(t, manager.Deploy(t.Context(), stack, false, false, localEndpoint(), nil))

		output := buf.String()
		assert.Contains(t, output, stack.Name)
		assert.Contains(t, output, `"variables":1`)
	})

	t.Run("and stays quiet for a stack that uses no escape", func(t *testing.T) {
		buf := captureLog(t)

		// Which is every stack that has never used the marker, so the line must not become
		// one more thing an operator learns to scroll past.
		stack := &portainer.Stack{
			Name:        "plain",
			ProjectPath: newStackProject(t, plainComposeBody),
			EntryPoint:  "docker-compose.yml",
			Env:         []portainer.Pair{{Name: literalName, Value: literalVal}},
		}

		manager := &ComposeStackManager{deployer: &stubDeployer{}, secretResolver: &failingFetcher{t: t}}

		require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

		assert.Empty(t, buf.String())
	})
}

func Test_Up_writesNoEnvFileWhenEveryPairIsAReference(t *testing.T) {
	t.Parallel()

	stack := newSecretStack(t)
	stack.Env = stack.Env[1:] // drop the literal pair

	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	assert.Empty(t, deployer.deployOptions.EnvFilePath)
	assert.NoFileExists(t, stackEnvFilePath(stack))
	assert.Len(t, deployer.deployOptions.Env, 2)
}

func Test_Up_failsOnResolverError(t *testing.T) {
	t.Parallel()

	stack := newSecretStack(t)
	deployer := &stubDeployer{}
	manager := &ComposeStackManager{
		deployer:       deployer,
		secretResolver: &stubFetcher{err: errors.New("resolver unavailable")},
	}

	err := manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolver unavailable")

	// The deploy is refused outright, and nothing was written on the way out.
	assert.Empty(t, deployer.deployOptions.ProjectName)
	assert.NoFileExists(t, stackEnvFilePath(stack))
}

func Test_Up_withoutReferencesNeverCallsTheResolver(t *testing.T) {
	t.Parallel()

	fetcher := newFetcher()
	stack := &portainer.Stack{
		Name:        "plain",
		ProjectPath: newStackProject(t, plainComposeBody),
		EntryPoint:  "docker-compose.yml",
		Env:         []portainer.Pair{{Name: "VAR1", Value: "value1"}},
	}

	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: fetcher}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	assert.Empty(t, fetcher.calls)
	assert.Nil(t, deployer.deployOptions.Env)
	assert.Equal(t, "VAR1=value1\n", readStackEnvFile(t, stack))
}

func Test_Run_resolvesSecretsIntoOptionsEnv(t *testing.T) {
	t.Parallel()

	stack := newSecretStack(t)
	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

	require.NoError(t, manager.Run(t.Context(), stack, localEndpoint(), "worker", portainer.ComposeRunOptions{}))

	assert.Equal(t, []string{"ADMIN_TOKEN=" + tokenValue, "METRICS_TOKEN=" + metricsVal}, deployer.runOptions.Env)
	assert.NotContains(t, readStackEnvFile(t, stack), tokenValue)
}

func Test_Pull_resolvesSecretsIntoOptionsEnv(t *testing.T) {
	t.Parallel()

	stack := newSecretStack(t)
	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

	require.NoError(t, manager.Pull(t.Context(), stack, localEndpoint(), portainer.ComposeOptions{}))

	assert.Equal(t, []string{"ADMIN_TOKEN=" + tokenValue, "METRICS_TOKEN=" + metricsVal}, deployer.pullOptions.Env)
	assert.NotContains(t, readStackEnvFile(t, stack), tokenValue)
}

func Test_NewComposeStackManager_leavesResolverNilWhenUnconfigured(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("PORTAINER_SECRET_RESOLVER", "")

	manager := NewComposeStackManager(&stubDeployer{}, nil)

	// A typed nil *Client assigned into the interface would compare non-nil here and
	// let a reference through as a literal, so the constructor must not assign it.
	assert.Nil(t, manager.secretResolver)
}

func Test_NewComposeStackManager_populatesResolverWhenConfigured(t *testing.T) {
	t.Setenv("PORTAINER_SECRET_RESOLVER", "unix:///run/secret-resolver/resolver.sock")

	manager := NewComposeStackManager(&stubDeployer{}, nil)

	assert.NotNil(t, manager.secretResolver)
}

// hostileFiller is one class of byte for hostileName to overrun the bound with, named by what
// %q costs to print it.
//
// The class is what decides the size of the finished message, because TruncateName bounds the
// INPUT of the verb rather than its output: %q renders an invalid UTF-8 byte and a C0 byte as
// four characters each, a backslash and U+2028 as two per input byte, and an ordinary printable
// as one. See secretresolver.TruncateName.
type hostileFiller struct {
	// name goes into the subtest name.
	name string

	// text is repeated until the bound is overrun many times over.
	text string
}

// hostileFillers are the classes the ceiling below has to hold against, worst first.
//
// The fixture is parameterised over them because 'A' is the cheapest of them: %q leaves it
// alone. A ceiling asserted only against 'A' therefore passes for a reason unrelated to what
// it claims - the swarm refusal measures 635 bytes filled with 'A' against 2081 filled with
// invalid UTF-8.
var hostileFillers = []hostileFiller{
	{name: "an invalid UTF-8 byte", text: "\xff"},
	{name: "NUL", text: "\x00"},
	{name: "a non-printable multi-byte rune", text: "\u2028"},
	{name: "a backslash", text: `\`},
	{name: "an ordinary printable", text: "A"},
}

// hostileName returns a stack or variable name of the shape an attacker can actually set:
// control bytes a terminal and an agent both act on, then a mebibyte of filler. Both halves
// matter and they fail differently - the control bytes need a verb that escapes, the mebibyte
// needs a bound - which is why every site takes both.
//
// The readable prefix is kept so a test can assert the diagnostic survived the treatment. A
// message bounded into saying nothing would pass a size assertion and be useless.
func hostileName(prefix, filler string) string {
	return prefix + "\x00\x1b[31m\n" + strings.Repeat(filler, 1<<20)
}

// worstCaseQuotedNameBytes is the most one bounded name can occupy once %q has printed it.
//
// Measured from the production helper rather than written down, so that it tracks maxNameBytes
// and the truncation marker without this file knowing either number. A name of nothing but
// invalid UTF-8 is the worst input TruncateName can be handed, and strconv.Quote is what fmt's
// %q calls for a string. It comes to 1029 bytes today - 4 per bounded byte, plus the marker and
// the two quotes - against 261 for a bounded name of 'A'.
//
// One assumption remains, and it is what the mebibyte buys: the probe has to be LONGER than
// maxNameBytes. Below that, TruncateName hands it straight back, no marker is appended, and the
// figure stops tracking the bound - a 1 KiB probe froze at 4098 for every maxNameBytes of 1024
// or more, and the tests below go red at 2048. 1<<20 is the same mebibyte hostileName already
// uses and exceeds any plausible value of maxNameBytes; at today's 256 it measures identically.
var worstCaseQuotedNameBytes = len(strconv.Quote(secretresolver.TruncateName(strings.Repeat("\xff", 1<<20))))

// maxFixedMessageBytes is the allowance for everything in one of these messages that is not a
// name: the sentence, the wrapping, the operation the withheld error names and the short
// resolver error one case wraps. A flat number rather than a derived one, because it bounds
// text we author and review rather than text a caller supplies - the longest of these messages
// carries a little over 200 bytes of it - and it sits well over that so the prose can be
// reworded without anybody having to retune a ceiling.
const maxFixedMessageBytes = 512

// maxRefusalMessageBytes is the ceiling the messages below are held to: the authored allowance
// plus two worst-case names, which is the most any message here names.
//
// The previous value was a flat 2048, and the worst case exceeds it - two bounded names of
// invalid UTF-8 print as 2058 bytes between them before a word of the message is added. It held
// only because the fixture was filled with 'A'.
var maxRefusalMessageBytes = maxFixedMessageBytes + 2*worstCaseQuotedNameBytes

// assertBoundedAndEscaped holds one persisted message to the ceiling, to the absence of raw
// control bytes, and to still naming what it is about.
func assertBoundedAndEscaped(t *testing.T, err error, wantNamed ...string) {
	t.Helper()

	require.Error(t, err)

	message := err.Error()

	// The name is logged with the size because these subtests run in parallel and their
	// output interleaves: the measurement is only evidence if it says which case produced it.
	t.Logf("%s: message is %d bytes, ceiling is %d (%d authored + 2x%d for a worst-case name)",
		t.Name(), len(message), maxRefusalMessageBytes, maxFixedMessageBytes, worstCaseQuotedNameBytes)

	assert.LessOrEqual(t, len(message), maxRefusalMessageBytes)

	// The bound fired rather than the name merely being short.
	assert.Contains(t, message, "...")

	for _, control := range []string{"\x00", "\x1b", "\n", "\r", "\t", "\x7f"} {
		assert.NotContains(t, message, control)
	}

	for _, named := range wantNamed {
		assert.Contains(t, message, named)
	}
}

// Test_secretErrors_boundAndEscapeHostileNames pins both halves of the fix at every site in
// this package that names a stack or a variable in an error raised on the deploy path.
//
// The channel: stackutils.UpdateStackStatusFromDeploymentResult writes these messages verbatim
// into Stack.DeploymentStatus[].Message, which Portainer persists and StackInspect serves back
// to operators and to agents. Neither name is validated on the way in - a stack's Env comes
// straight out of the request body, and a stack name reaches the database unnormalised through
// POST /stacks/{id}/migrate - so both the length and the bytes are the caller's.
//
// Measured on a copy with these inputs before the fix, a mebibyte in the variable name and an
// ordinary stack name: SwarmStackManager.Deploy 1048700 bytes with the NUL, the ESC and the
// newline intact, and "no value resolved" 1048649 bytes - escaped by the %q it already had,
// and a mebibyte all the same, which is the half %q does not close. A mebibyte in both names
// put the swarm refusal over 2 MiB.
//
// Reverting either half of the fix at any one site fails this test at that site: dropping the
// bound fails the size assertion, dropping %q fails the control-byte assertion. See
// secretresolver.TruncateName.
//
// Every site is run once per byte class in hostileFillers, because the bound applies to the
// input of %q and the classes cost between one and four printed bytes each. Measured here, per
// class, for the two sites that name both a stack and a variable:
//
//	invalid UTF-8, NUL   swarm 2081   no value resolved 2021
//	U+2028               swarm 1113   no value resolved 1053
//	backslash            swarm 1117   no value resolved 1057
//	ordinary printable   swarm  635   no value resolved  575
func Test_secretErrors_boundAndEscapeHostileNames(t *testing.T) {
	t.Parallel()

	newStack := func(t *testing.T, filler string) *portainer.Stack {
		t.Helper()

		return &portainer.Stack{
			Name:        hostileName("STACKNAME", filler),
			ProjectPath: newStackProject(t, plainComposeBody),
			EntryPoint:  "docker-compose.yml",
			Env: []portainer.Pair{
				{Name: hostileName("VARNAME", filler), Value: tokenRef},
			},
		}
	}

	for _, filler := range hostileFillers {
		t.Run("filled with "+filler.name, func(t *testing.T) {
			t.Parallel()

			t.Run("the swarm refusal", func(t *testing.T) {
				t.Parallel()

				manager := &SwarmStackManager{}

				err := manager.Deploy(t.Context(), newStack(t, filler.text), false, false, localEndpoint(), nil)
				assertBoundedAndEscaped(t, err, "STACKNAME", "VARNAME")
			})

			t.Run("no resolver is configured", func(t *testing.T) {
				t.Parallel()

				manager := &ComposeStackManager{}

				_, err := manager.resolveStackSecrets(t.Context(), newStack(t, filler.text))
				assertBoundedAndEscaped(t, err, "STACKNAME", secretresolver.EndpointEnvVar)
			})

			t.Run("the resolver failed", func(t *testing.T) {
				t.Parallel()

				manager := &ComposeStackManager{secretResolver: &stubFetcher{err: errors.New("the vault is locked")}}

				_, err := manager.resolveStackSecrets(t.Context(), newStack(t, filler.text))
				assertBoundedAndEscaped(t, err, "STACKNAME", "the vault is locked")
			})

			t.Run("a variable is left without a value", func(t *testing.T) {
				t.Parallel()

				// Unreachable against the real client, which guarantees a value for
				// every requested reference, so it is driven by a stub that answers
				// with none.
				manager := &ComposeStackManager{secretResolver: &stubFetcher{values: map[string]string{}}}

				_, err := manager.resolveStackSecrets(t.Context(), newStack(t, filler.text))
				assertBoundedAndEscaped(t, err, "STACKNAME", "VARNAME")
			})

			t.Run("the withheld deploy error", func(t *testing.T) {
				t.Parallel()

				// The one site here that names no variable: its whole point is that it
				// carries nothing of the deployer's text. The stack name was the
				// exception it carried anyway, unbounded, at 1048781 bytes.
				err := deployFailure(newStack(t, filler.text), []string{"ADMIN_TOKEN=" + tokenValue}, "failed to deploy a stack", errors.New("boom"))
				assertBoundedAndEscaped(t, err, "STACKNAME")
			})
		})
	}
}

// bodyRefComposeBody writes a reference where it cannot be reached through Options.Env
// alone: compose interpolates ${VAR} and nothing else, so this scalar is handed to the
// container as its own text unless the body itself is rewritten.
const bodyRefComposeBody = `services:
  app:
    image: nginx
    environment:
      METRICS_TOKEN: ` + metricsRef + `
`

// newBodyRefStack returns a stack whose only reference is written inline in its body.
func newBodyRefStack(t *testing.T) *portainer.Stack {
	t.Helper()

	return &portainer.Stack{
		Name:        "arcextension",
		ProjectPath: newStackProject(t, bodyRefComposeBody),
		EntryPoint:  "docker-compose.yml",
		Env:         []portainer.Pair{{Name: literalName, Value: literalVal}},
	}
}

func Test_Up_rewritesAnInlineBodyReference(t *testing.T) {
	t.Parallel()

	stack := newBodyRefStack(t)
	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	// The value travels through Options.Env under a placeholder name, the same channel
	// the stack's own variables use.
	placeholder := secretresolver.ComposePlaceholderPrefix + "0"
	assert.Equal(t, []string{placeholder + "=" + metricsVal}, deployer.deployOptions.Env)

	// A rewritten copy is deployed, not the stack's own file, and the copy carries the
	// placeholder and neither the reference nor the value.
	entryPoint := filepath.Join(stack.ProjectPath, "docker-compose.yml")
	require.Len(t, deployer.deployPaths, 1)
	assert.NotEqual(t, entryPoint, deployer.deployPaths[0])

	require.Len(t, deployer.deployContents, 1)
	copied := string(deployer.deployContents[0])
	assert.Contains(t, copied, "${"+placeholder+"}")
	assert.NotContains(t, copied, metricsRef)
	assert.NotContains(t, copied, metricsVal)

	// The operator's file is not touched: the rewrite is a copy, and the stack keeps
	// storing the reference.
	original, err := os.ReadFile(entryPoint)
	require.NoError(t, err)
	assert.Equal(t, bodyRefComposeBody, string(original))

	// The env file keeps the literals and nothing else.
	assert.Equal(t, literalName+"="+literalVal+"\n", readStackEnvFile(t, stack))
}

func Test_Up_pinsTheProjectDirectoryToTheStackDirectory(t *testing.T) {
	t.Parallel()

	// compose-go resolves every relative path in a body - a build context, a bind mount
	// source, an env_file, an include - against the directory of the first config file
	// unless ProjectDir says otherwise. The rewritten copy lives somewhere else, so
	// without this every such path would silently start meaning a path under the
	// temporary directory.
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "with a rewritten body", body: bodyRefComposeBody},
		{name: "with a body that needed no rewrite", body: plainComposeBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stack := newBodyRefStack(t)
			stack.ProjectPath = newStackProject(t, test.body)

			deployer := &stubDeployer{}
			manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

			require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

			assert.Equal(t, stack.ProjectPath, deployer.deployOptions.ProjectDir)
		})
	}
}

func Test_Up_deletesTheRewrittenCopyAfterTheDeploy(t *testing.T) {
	t.Parallel()

	stack := newBodyRefStack(t)
	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: newFetcher()}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	require.Len(t, deployer.deployPaths, 1)

	// The copy carries placeholders and never a value, but it is still not the operator's
	// file and has no business outliving the deploy.
	assert.NoFileExists(t, deployer.deployPaths[0])
	assert.NoDirExists(t, filepath.Dir(deployer.deployPaths[0]))
}

func Test_Up_batchesEnvAndBodyReferencesTogether(t *testing.T) {
	t.Parallel()

	stack := newBodyRefStack(t)
	stack.Env = append(stack.Env,
		portainer.Pair{Name: "ADMIN_TOKEN", Value: tokenRef},
		// The same reference the body carries: one lookup, not two.
		portainer.Pair{Name: "METRICS_TOKEN", Value: metricsRef},
	)

	fetcher := newFetcher()
	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: fetcher}

	require.NoError(t, manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{}))

	require.Len(t, fetcher.calls, 1)
	assert.Equal(t, []string{tokenRef, metricsRef}, fetcher.calls[0])

	// The stack's own variables first, then one placeholder per body reference.
	assert.Equal(t, []string{
		"ADMIN_TOKEN=" + tokenValue,
		"METRICS_TOKEN=" + metricsVal,
		secretresolver.ComposePlaceholderPrefix + "0=" + metricsVal,
	}, deployer.deployOptions.Env)
}

func Test_Up_withholdsTheDeployerErrorForABodyValue(t *testing.T) {
	t.Parallel()

	// A value resolved for a body reference goes into the same slice the stack's own
	// variables do, which is what deployFailure redacts the deployer's text against. A
	// value kept anywhere else would be the one value that redaction does not cover.
	stack := newBodyRefStack(t)
	manager := &ComposeStackManager{
		deployer:       &stubDeployer{err: fmt.Errorf("invalid containerPort: %s", distinctValue)},
		secretResolver: &stubFetcher{values: map[string]string{metricsRef: distinctValue}},
	}

	err := manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{})
	require.Error(t, err)

	assert.NotContains(t, err.Error(), distinctValue)
	assert.Contains(t, err.Error(), "failed to deploy a stack")

	for fragment := range strings.SplitSeq(distinctValue, "-") {
		assert.NotContains(t, err.Error(), fragment)
	}
}

func Test_Up_failsOnABodyThatCannotBeScanned(t *testing.T) {
	t.Parallel()

	// A scalar whose extent cannot be established exactly is refused rather than deployed
	// with the reference left in it, and it is refused before the resolver is called.
	stack := newBodyRefStack(t)
	stack.ProjectPath = newStackProject(t, "command: |\n  "+metricsRef+"\n")

	deployer := &stubDeployer{}
	manager := &ComposeStackManager{deployer: deployer, secretResolver: &failingFetcher{t: t}}

	err := manager.Up(t.Context(), stack, localEndpoint(), portainer.ComposeUpOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "arcextension")
	assert.Contains(t, err.Error(), "docker-compose.yml")
	assert.Empty(t, deployer.deployOptions.ProjectName)
}

func Test_SwarmStackManager_Deploy_refusesAnInlineBodyReference(t *testing.T) {
	t.Parallel()

	// Swarm has no rewriting path either, so the reference would reach the service
	// verbatim in the body as it would in a variable.
	manager := &SwarmStackManager{}

	err := manager.Deploy(t.Context(), newBodyRefStack(t), false, false, localEndpoint(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret references")
	assert.Contains(t, err.Error(), "docker-compose.yml")
}

func Test_SwarmStackManager_Deploy_passesAnEscapedBodyScalarThrough(t *testing.T) {
	t.Parallel()

	// Unlike an escaped variable, which is unescaped on this path, an escaped marker in a
	// body is not a reference and keeps travelling verbatim: resolving it would mean
	// rewriting the file, which the swarm path deliberately does not do.
	body := "services:\n  app:\n    command: " + escapedLiteral + "\n"

	stack := newBodyRefStack(t)
	stack.ProjectPath = newStackProject(t, body)

	deployer := &stubSwarmDeployer{}
	manager := &SwarmStackManager{deployer: deployer}

	require.NoError(t, manager.Deploy(t.Context(), stack, false, false, localEndpoint(), nil))

	survived, err := os.ReadFile(filepath.Join(stack.ProjectPath, "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, body, string(survived))
}
