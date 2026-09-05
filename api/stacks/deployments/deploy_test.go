package deployments

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/crypto"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/datastore"
	gittypes "github.com/portainer/portainer/api/git/types"
	"github.com/portainer/portainer/api/internal/testhelpers"
	"github.com/portainer/portainer/pkg/fips"
	"github.com/portainer/portainer/pkg/libhttp/response"
	"github.com/portainer/portainer/pkg/secretresolver"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const localhostCert = `-----BEGIN CERTIFICATE-----
MIIEOjCCAiKgAwIBAgIRALg8rJET2/9LjKSxHj0dQhYwDQYJKoZIhvcNAQELBQAw
FzEVMBMGA1UEAxMMUG9ydGFpbmVyIENBMB4XDTIzMTAxMTE5NDcxMVoXDTI1MDQx
MTE5NTM0MVowFDESMBAGA1UEAxMJbG9jYWxob3N0MIIBIjANBgkqhkiG9w0BAQEF
AAOCAQ8AMIIBCgKCAQEAx4nNGiwcCqUCxZyVLIHqvjTy20ZtZDVCedssTv1W5tmz
YqOIYGaW3CqzlRn6vBHu9bMHXef4+XfS0igKBn76MAKn5IcTccIWIal+5jq48pI3
c2FzQ3qNujX2zqZPjAjhJnVeVCP3kJu4wUtuubswLPBVLdktGa6EkL+8nu6o0Phw
6scV6s3gUmQk5/lpH4FIff8M7NAdTOxiFImQ1M0vplKtaEeiCnskpgyI8CbZl7X0
38Pu178W3+LqB7N4iMy2gKnBwjsXzw/+1dfUGkKjYdDBD+kNEKrQ4dwkjkrkQVdt
Z+GN26NvXHoeeyX/MLnVgdLbiIjvsf0DDIhabKqTcwIDAQABo4GDMIGAMA4GA1Ud
DwEB/wQEAwIDuDAdBgNVHSUEFjAUBggrBgEFBQcDAQYIKwYBBQUHAwIwHQYDVR0O
BBYEFPCefmK5Szzlfs8FRCa5+kRCIEWuMB8GA1UdIwQYMBaAFKZZ074SR/ajD3zE
gxpLGRvFT3XAMA8GA1UdEQQIMAaHBH8AAAEwDQYJKoZIhvcNAQELBQADggIBABcQ
/WPSUpuQvrcVBsmIlOMz74cDZYuIDls/mAcB/yP3mm+oWlO0qvH/F/BMs1P/bkgj
fByQZq8Zmi6/TEZNlGvW7KGx077VxDKi8jd1jL3gLDPmkFjYuGeIWQusgxBu1y3m
0WoTTqnkoism1mzV/dgNwrm3YQIV4H/fi9EEdQSm0UFRTKSAGBkwS7N2pmNb5yQO
U8glFpyznCv4evDJbs/JUUXKYExgFFhWUd25P7iBRLXg/BFfqdSTiUGUj/Msz0pO
Evqmq78eIiXjyyKSxzve6/mEIeq6AE3AC9zH+fwTd6Mhp+T2P/S/iO4EU19IMR4m
sbNBd6h/3GvRekO1KbqQ42awuMnxvWT0NVclSxiU1lMpAmRmk/w9z7wB3r4n7oh4
iiOTl5VSw1UBkcLDOJw+HB/FU2PdVFfIJKRfjLCZOGrcJX9vEcz7dYGpB5HrdqOc
/8q5j1g6f/pGE+20HITrtz6ChguETzqw5dLNeKeolC6bVH8yEtmpnP2n8VPnT9Di
V+hnONcJ+wd/dkBqabGr7LPG24Kj1F2Zp3CDDvJA94FaEsgaLfSg3JD+43uRCOWM
RuqU8bGuhQRqilR2dSIOrFaW2+MeUHsb24cUn/pkHqKpSg+RBEnf6QfGDlIgqYEl
19f/HFVBc/a8lM/D81lMyDbjQ9zH4LDYj4ipBbkL
-----END CERTIFICATE-----`

const localhostKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAx4nNGiwcCqUCxZyVLIHqvjTy20ZtZDVCedssTv1W5tmzYqOI
YGaW3CqzlRn6vBHu9bMHXef4+XfS0igKBn76MAKn5IcTccIWIal+5jq48pI3c2Fz
Q3qNujX2zqZPjAjhJnVeVCP3kJu4wUtuubswLPBVLdktGa6EkL+8nu6o0Phw6scV
6s3gUmQk5/lpH4FIff8M7NAdTOxiFImQ1M0vplKtaEeiCnskpgyI8CbZl7X038Pu
178W3+LqB7N4iMy2gKnBwjsXzw/+1dfUGkKjYdDBD+kNEKrQ4dwkjkrkQVdtZ+GN
26NvXHoeeyX/MLnVgdLbiIjvsf0DDIhabKqTcwIDAQABAoIBAQCqSP6BPG195A52
iEeCISksw9ERsou+fflKNvIcQvV7swP0xOyooERUhhiVwQMKpx9QDUXXLRV8CHch
JExR+OEYQdv4GhJM/b6XYafLYQfe80thKyQLzTXQWSdUeffe4OEMShODKOKoRUyp
oO9Qj9/wKfX3V6S2iwnU4dxdofztv+YP9rYQyjnhKbv/9OfeCp2Pb9eFKKRsA+QQ
xneDz1+wr8ToTuiTn8HBPNSeSAKvhzXuzyluI7VAetRloNgCtumrA9kpVbW2cDgE
Gk0q3RY125ejFELQO/cOJFuBsqoJlvPxzg8/vHyfyF9hFMqbqvcUw2e1eqHpnJd5
dP4+ZGYZAoGBAOOFuPXMLBts0rN9mfNbVfx36H+aOCL77SafZvWm0D+rH69QN3/q
/ZSWQEjwH5Tzn1e+NVcl/Um2vL/dIyEGBklXQ7yAyJo25gpEOD/rt1U94HKzMOwy
yKtsKghRAOx0piie7ORS6MGbEOQxU3/1Eg1uvd0qoSnALqJ/le75QpFXAoGBAOCD
aZQTszzDddr1cFPzLyqjIGJWfPcDYSONXVcCeQmhvC7mkfw9SWdIfku7JbdNgFYq
ZAAU0klsLX0lEe8f4A12FnHNylKoxmTWdE3wWPptejdA1KUgzt/2kNljgOMFuY0Q
rlCEW/Jabrg5aFMwVVG8bHLZR0xalfniDvXLvnFFAoGACdztJLKiIto31BIYz2Th
OF2WVZnA3ztej3MPioydsHThnb7zePcd4QgWZ1MJe3KIMMyNEWcTMNPcINEcSb0y
HpHK3OwURiMlG8LTUWoNe4OALFi6QTL+YfgBZnTkflucLFyfVlKFxobLV6kPvpdI
Hg7z6heD/wRWwTKYtFBX42cCgYBIeoQJ9rYlRqB0eEm0AEzYweLBfFRJVgD0/j0E
ytqSPnFG3s6AFLTur9t9zUPmwhFNP9Aaqp4cb9zbiq0YejzVe6rRQHMxbiTmBslz
I8VFyzPqRHahfE7sxGeMlm/UWlPFc34ipigcvA8EUBwaxv60LVUBWp2Gy7OhANZ9
iTHI1QKBgQCdHFj9dnbpaEHA426CoaPsyj5cv2nBLRf8p1cs71sq+qQOGlGJfajm
L9x22ol5c5rToZa1qKSnSdSDCud298MyRujMUy2UcUKHeNs3MK9AT41sDv266I7b
vJUUCFYm8+9p6gTVOcoMit+eGSwa81PCPEs1TnU1PV/PaDFeUhn/mg==
-----END RSA PRIVATE KEY-----`

type noopDeployer struct{}

// without unpacker
func (s noopDeployer) DeploySwarmStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, registries []portainer.Registry, prune, pullImage bool) error {
	return nil
}

func (s noopDeployer) DeployComposeStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, registries []portainer.Registry, prune, forcePullImage, forceRecreate bool) error {
	return nil
}

func (s noopDeployer) UndeployComposeStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint) error {
	return nil
}

func (s noopDeployer) DeployKubernetesStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, user *portainer.User) error {
	return nil
}

// with unpacker
func (s noopDeployer) DeployRemoteComposeStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, registries []portainer.Registry, prune, forcePullImage, forceRecreate bool) error {
	return nil
}
func (s noopDeployer) UndeployRemoteComposeStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint) error {
	return nil
}
func (s noopDeployer) StartRemoteComposeStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, registries []portainer.Registry) error {
	return nil
}
func (s noopDeployer) StopRemoteComposeStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint) error {
	return nil
}
func (s noopDeployer) DeployRemoteSwarmStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, registries []portainer.Registry, prune, pullImage bool) error {
	return nil
}
func (s noopDeployer) UndeployRemoteSwarmStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint) error {
	return nil
}
func (s noopDeployer) StartRemoteSwarmStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, registries []portainer.Registry) error {
	return nil
}
func (s noopDeployer) StopRemoteSwarmStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint) error {
	return nil
}

func agentServer(t *testing.T) string {
	h := http.NewServeMux()

	h.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(portainer.PortainerAgentHeader, "v2.19.0")
		w.Header().Set(portainer.HTTPResponseAgentPlatform, strconv.Itoa(int(portainer.AgentPlatformDocker)))

		_ = response.Empty(w)
	})

	cert, err := tls.X509KeyPair([]byte(localhostCert), []byte(localhostKey))
	require.NoError(t, err)

	tlsConfig := crypto.CreateTLSConfiguration(false)
	tlsConfig.Certificates = []tls.Certificate{cert}

	l, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	require.NoError(t, err)

	s := &http.Server{
		Handler: h,
	}

	errCh := make(chan error)
	go func() {
		errCh <- s.Serve(l)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		require.NoError(t, s.Shutdown(ctx))
		require.ErrorIs(t, <-errCh, http.ErrServerClosed)
	})

	return "http://" + l.Addr().String()
}

func Test_redeployWhenChanged_FailsWhenCannotFindStack(t *testing.T) {
	t.Parallel()
	_, store := datastore.MustNewTestStore(t, false, true)

	err := RedeployWhenChanged(t.Context(), 1, nil, store, nil)
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(err.Error(), "failed to get the stack"), "it isn't an error we expected: %v", err.Error())
}

func Test_redeployWhenChanged_DoesNothingWhenNotAGitBasedStack(t *testing.T) {
	t.Parallel()
	_, store := datastore.MustNewTestStore(t, false, true)

	admin := &portainer.User{ID: 1, Username: "admin"}
	err := store.User().Create(admin)
	require.NoError(t, err, "error creating an admin")

	err = store.Stack().Create(&portainer.Stack{ID: 1, CreatedBy: "admin"})
	require.NoError(t, err, "failed to create a test stack")

	err = RedeployWhenChanged(t.Context(), 1, nil, store, testhelpers.NewGitService(nil, ""))
	require.NoError(t, err)
}

func Test_redeployWhenChanged_DoesNothingWhenNoGitChanges(t *testing.T) {
	t.Parallel()
	_, store := datastore.MustNewTestStore(t, false, true)

	tmpDir := t.TempDir()

	admin := &portainer.User{ID: 1, Username: "admin"}
	err := store.User().Create(admin)
	require.NoError(t, err, "error creating an admin")

	err = store.Endpoint().Create(&portainer.Endpoint{ID: 0})
	require.NoError(t, err, "error creating environment")

	src := &portainer.Source{
		Type: portainer.SourceTypeGit,
		Git: &gittypes.RepoConfig{
			URL:           "url",
			ReferenceName: "ref",
			ConfigHash:    "oldHash",
		},
	}
	err = store.Source().Create(src)
	require.NoError(t, err, "failed to create source")

	wf := &portainer.Workflow{Artifacts: []portainer.Artifact{{Files: []portainer.ArtifactFile{{SourceID: src.ID}}}}}
	err = store.Workflow().Create(wf)
	require.NoError(t, err, "failed to create workflow")

	err = store.Stack().Create(&portainer.Stack{
		ID:          1,
		CreatedBy:   "admin",
		ProjectPath: tmpDir,
		WorkflowID:  wf.ID,
	})
	require.NoError(t, err, "failed to create a test stack")

	err = RedeployWhenChanged(t.Context(), 1, nil, store, testhelpers.NewGitService(nil, "oldHash"))
	require.NoError(t, err)
}

func Test_redeployWhenChanged_FailsWhenCannotClone(t *testing.T) {
	fips.InitFIPS(false)

	cloneErr := errors.New("failed to clone")
	_, store := datastore.MustNewTestStore(t, false, true)

	admin := &portainer.User{ID: 1, Username: "admin"}
	err := store.User().Create(admin)
	require.NoError(t, err, "error creating an admin")

	err = store.Endpoint().Create(&portainer.Endpoint{
		ID:  0,
		URL: agentServer(t),
		TLSConfig: portainer.TLSConfiguration{
			TLS:           true,
			TLSSkipVerify: true,
		},
	})
	require.NoError(t, err, "error creating environment")

	src := &portainer.Source{
		Type: portainer.SourceTypeGit,
		Git: &gittypes.RepoConfig{
			URL:           "url",
			ReferenceName: "ref",
			ConfigHash:    "oldHash",
		},
	}
	err = store.Source().Create(src)
	require.NoError(t, err, "failed to create source")

	wf := &portainer.Workflow{Artifacts: []portainer.Artifact{{
		StackID: 1,
		Files:   []portainer.ArtifactFile{{SourceID: src.ID}},
	}}}
	err = store.Workflow().Create(wf)
	require.NoError(t, err, "failed to create workflow")

	err = store.Stack().Create(&portainer.Stack{
		ID:         1,
		CreatedBy:  "admin",
		WorkflowID: wf.ID,
	})
	require.NoError(t, err, "failed to create a test stack")

	err = RedeployWhenChanged(t.Context(), 1, nil, store, testhelpers.NewGitService(cloneErr, "newHash"))
	require.Error(t, err)
	require.ErrorIs(t, err, cloneErr, "should failed to clone but didn't, check test setup")
}

func setupRedeployStore(t *testing.T, stackType portainer.StackType) (dataservices.DataStore, portainer.StackID) {
	t.Helper()

	_, store := datastore.MustNewTestStore(t, false, true)
	tmpDir := t.TempDir()

	err := store.Endpoint().Create(&portainer.Endpoint{ID: 1})
	require.NoError(t, err, "error creating environment")

	username := "user"
	err = store.User().Create(&portainer.User{Username: username, Role: portainer.AdministratorRole})
	require.NoError(t, err, "error creating a user")

	src := &portainer.Source{
		Type: portainer.SourceTypeGit,
		Git: &gittypes.RepoConfig{
			URL:           "url",
			ReferenceName: "ref",
			ConfigHash:    "oldHash",
		},
	}
	err = store.Source().Create(src)
	require.NoError(t, err, "failed to create source")

	wf := &portainer.Workflow{Artifacts: []portainer.Artifact{{Files: []portainer.ArtifactFile{{SourceID: src.ID}}}}}
	err = store.Workflow().Create(wf)
	require.NoError(t, err, "failed to create workflow")

	const stackID portainer.StackID = 1

	err = store.Stack().Create(&portainer.Stack{
		ID:          stackID,
		EndpointID:  1,
		ProjectPath: tmpDir,
		UpdatedBy:   username,
		WorkflowID:  wf.ID,
		Type:        stackType,
	})
	require.NoError(t, err, "failed to create a test stack")

	return store, stackID
}

func Test_redeployWhenChanged_DockerComposeStack(t *testing.T) {
	t.Parallel()

	store, stackID := setupRedeployStore(t, portainer.DockerComposeStack)

	err := RedeployWhenChanged(t.Context(), stackID, noopDeployer{}, store, testhelpers.NewGitService(nil, "newHash"))
	require.NoError(t, err)
}

func Test_redeployWhenChanged_DockerSwarmStack(t *testing.T) {
	t.Parallel()

	store, stackID := setupRedeployStore(t, portainer.DockerSwarmStack)

	err := RedeployWhenChanged(t.Context(), stackID, noopDeployer{}, store, testhelpers.NewGitService(nil, "newHash"))
	require.NoError(t, err)
}

func Test_redeployWhenChanged_KubernetesStack(t *testing.T) {
	t.Parallel()

	store, stackID := setupRedeployStore(t, portainer.KubernetesStack)

	err := RedeployWhenChanged(t.Context(), stackID, noopDeployer{}, store, testhelpers.NewGitService(nil, "newHash"))
	require.NoError(t, err)
}

func Test_getUserRegistries(t *testing.T) {
	t.Parallel()
	_, store := datastore.MustNewTestStore(t, false, true)

	endpointID := 123

	admin := &portainer.User{ID: 1, Username: "admin", Role: portainer.AdministratorRole}
	err := store.User().Create(admin)
	require.NoError(t, err, "error creating an admin")

	user := &portainer.User{ID: 2, Username: "user", Role: portainer.StandardUserRole}
	err = store.User().Create(user)
	require.NoError(t, err, "error creating a user")

	team := portainer.Team{ID: 1, Name: "team"}

	err = store.TeamMembership().Create(&portainer.TeamMembership{
		ID:     1,
		UserID: user.ID,
		TeamID: team.ID,
		Role:   portainer.TeamMember,
	})
	require.NoError(t, err)

	registryReachableByUser := portainer.Registry{
		ID:   1,
		Name: "registryReachableByUser",
		RegistryAccesses: portainer.RegistryAccesses{
			portainer.EndpointID(endpointID): {
				UserAccessPolicies: map[portainer.UserID]portainer.AccessPolicy{
					user.ID: {RoleID: portainer.RoleID(portainer.StandardUserRole)},
				},
			},
		},
	}
	err = store.Registry().Create(&registryReachableByUser)
	require.NoError(t, err, "couldn't create a registry")

	registryReachableByTeam := portainer.Registry{
		ID:   2,
		Name: "registryReachableByTeam",
		RegistryAccesses: portainer.RegistryAccesses{
			portainer.EndpointID(endpointID): {
				TeamAccessPolicies: map[portainer.TeamID]portainer.AccessPolicy{
					team.ID: {RoleID: portainer.RoleID(portainer.StandardUserRole)},
				},
			},
		},
	}
	err = store.Registry().Create(&registryReachableByTeam)
	require.NoError(t, err, "couldn't create a registry")

	registryRestricted := portainer.Registry{
		ID:   3,
		Name: "registryRestricted",
		RegistryAccesses: portainer.RegistryAccesses{
			portainer.EndpointID(endpointID): {
				UserAccessPolicies: map[portainer.UserID]portainer.AccessPolicy{
					user.ID + 100: {RoleID: portainer.RoleID(portainer.StandardUserRole)},
				},
			},
		},
	}
	err = store.Registry().Create(&registryRestricted)
	require.NoError(t, err, "couldn't create a registry")

	t.Run("admin should has access to all registries", func(t *testing.T) {
		registries, err := getUserRegistries(store, admin, portainer.EndpointID(endpointID))
		require.NoError(t, err)
		assert.ElementsMatch(t, []portainer.Registry{registryReachableByUser, registryReachableByTeam, registryRestricted}, registries)
	})

	t.Run("regular user has access to registries allowed to him and/or his team", func(t *testing.T) {
		registries, err := getUserRegistries(store, user, portainer.EndpointID(endpointID))
		require.NoError(t, err)
		assert.ElementsMatch(t, []portainer.Registry{registryReachableByUser, registryReachableByTeam}, registries)
	})
}

func Test_getEnv_refusesSecretReferences(t *testing.T) {
	t.Parallel()

	t.Run("passes literal variables through", func(t *testing.T) {
		t.Parallel()

		env, err := getEnv("plain", []portainer.Pair{{Name: "LOG_LEVEL", Value: "debug"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"--env=LOG_LEVEL=debug"}, env)
	})

	t.Run("refuses a reference instead of passing it through", func(t *testing.T) {
		t.Parallel()

		// The unpacker container has no resolution path, so a reference handed to it as
		// a literal --env argument would start a service on the string "secret:..." as
		// its credential. Dropping the variable silently would be no better.
		env, err := getEnv("plain", []portainer.Pair{
			{Name: "LOG_LEVEL", Value: "debug"},
			{Name: "ADMIN_TOKEN", Value: "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"},
		})
		require.Error(t, err)
		assert.Nil(t, env)
		assert.Contains(t, err.Error(), "ADMIN_TOKEN")
	})

	t.Run("accepts a value that only looks like a reference", func(t *testing.T) {
		t.Parallel()

		// The doubled colon is the escape for a literal whose own text starts with
		// "secret:". It reaches the unpacker as the same string the compose path would
		// produce, one colon shorter.
		env, err := getEnv("plain", []portainer.Pair{{Name: "APP_URI", Value: "secret::app/config"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"--env=APP_URI=secret:app/config"}, env)
	})
}

// Test_getEnv_announcesTheEscape pins the line that makes the escape visible on this path
// too. The value reaches the unpacker one colon shorter than the stack stores it, which is
// the one effect of the marker that is otherwise silent - see secretresolver.Unescape.
//
// It captures the global logger, so it does not run in parallel. That is safe: the
// package's parallel tests resume only once every sequential test has finished.
func Test_getEnv_announcesTheEscape(t *testing.T) {
	var buf bytes.Buffer

	previous := log.Logger
	log.Logger = zerolog.New(&buf)

	t.Cleanup(func() { log.Logger = previous })

	env, err := getEnv("arcextension", []portainer.Pair{
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "APP_URI", Value: "secret::app/config"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"--env=LOG_LEVEL=debug", "--env=APP_URI=secret:app/config"}, env)

	output := buf.String()
	assert.Contains(t, output, "arcextension")
	assert.Contains(t, output, `"variables":1`)
}

// composeManagerStub records whether the image pull was reached.
type composeManagerStub struct {
	pulled bool
}

func (m *composeManagerStub) ComposeSyntaxMaxVersion() string { return "" }

func (m *composeManagerStub) NormalizeStackName(name string) string { return name }

func (m *composeManagerStub) Run(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, serviceName string, options portainer.ComposeRunOptions) error {
	return nil
}

func (m *composeManagerStub) Up(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, options portainer.ComposeUpOptions) error {
	return nil
}

func (m *composeManagerStub) Down(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint) error {
	return nil
}

func (m *composeManagerStub) Pull(ctx context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, options portainer.ComposeOptions) error {
	m.pulled = true

	return nil
}

func Test_DeployRemoteComposeStack_refusesSecretReferencesBeforePulling(t *testing.T) {
	t.Parallel()

	stack := &portainer.Stack{
		Name:       "arcextension",
		EntryPoint: "docker-compose.yml",
		Env: []portainer.Pair{
			{Name: "LOG_LEVEL", Value: "debug"},
			{Name: "ADMIN_TOKEN", Value: "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"},
		},
	}

	compose := &composeManagerStub{}
	deployer := &stackDeployer{lock: &sync.Mutex{}, composeStackManager: compose}

	err := deployer.DeployRemoteComposeStack(t.Context(), stack, &portainer.Endpoint{}, nil, false, true, false)
	require.Error(t, err)

	// buildUnpackerCmdForStack refuses this stack anyway, but only after Pull has
	// resolved every reference - a full vault sync - and pulled every image for a deploy
	// that was never going to proceed.
	assert.False(t, compose.pulled)

	// And it refuses it in the same words, so the two checks cannot drift apart.
	_, fromBuilder := deployer.buildUnpackerCmdForStack(stack, OperationDeploy, unpackerCmdBuilderOptions{})
	require.Error(t, fromBuilder)
	assert.Equal(t, fromBuilder.Error(), err.Error())
}

func Test_buildUnpackerCmdForStack_refusesSecretReferencesOnlyWhereEnvIsUsed(t *testing.T) {
	t.Parallel()

	stack := &portainer.Stack{
		Name:       "arcextension",
		EntryPoint: "docker-compose.yml",
		Env: []portainer.Pair{
			{Name: "LOG_LEVEL", Value: "debug"},
			{Name: "ADMIN_TOKEN", Value: "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"},
		},
	}

	opts := unpackerCmdBuilderOptions{
		composeDestination: "/data/compose/1/v1/compose",
		gitConfig: &gittypes.RepoConfig{
			URL:           "https://example.com/stack.git",
			ReferenceName: "refs/heads/main",
		},
	}

	deployer := &stackDeployer{}

	t.Run("refuses every operation that passes the environment on", func(t *testing.T) {
		t.Parallel()

		for _, operation := range []StackRemoteOperation{
			OperationDeploy,
			OperationComposeStart,
			OperationSwarmDeploy,
			OperationSwarmStart,
		} {
			cmd, err := deployer.buildUnpackerCmdForStack(stack, operation, opts)
			require.Error(t, err, operation)
			assert.Nil(t, cmd, operation)
			assert.Contains(t, err.Error(), "ADMIN_TOKEN", operation)
			assert.Contains(t, err.Error(), "arcextension", operation)
		}
	})

	t.Run("still builds the operations that need no values", func(t *testing.T) {
		t.Parallel()

		// Undeploy and stop address the project by name. Refusing them for a reference
		// they never read would leave a migrated stack impossible to remove or stop
		// through this path.
		for _, operation := range []StackRemoteOperation{
			OperationUndeploy,
			OperationComposeStop,
			OperationSwarmUndeploy,
			OperationSwarmStop,
		} {
			cmd, err := deployer.buildUnpackerCmdForStack(stack, operation, opts)
			require.NoError(t, err, operation)
			require.NotEmpty(t, cmd, operation)

			for _, arg := range cmd {
				assert.NotContains(t, arg, secretresolver.ReferencePrefix, operation)
				assert.NotContains(t, arg, "--env=", operation)
			}
		}
	})
}
