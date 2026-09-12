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

// gitConfigProbe is the dataStore the ordering tests below hand to a stackDeployer. It records
// whether remoteStack reached the git-config block, and answers with an error rather than by
// crashing if it did.
//
// It replaces a nil dataStore, and the replacement is the whole point. A nil one did pin the
// ordering - moving the gate below the git-config block made the test fail - but it failed by
// dereferencing nil, and a panic does not fail one test, it takes the package's whole test
// binary down. A regression in this one gate would therefore have hidden every other result
// in api/stacks/deployments under CI, which is the opposite of what a pin is for. The counter
// below is what goes red instead, with a message naming what happened.
//
// Workflow().Read is the first thing GitSourceAndArtifactForStack does after its workflowID
// check, so recording at the accessor and refusing at the read covers the block: nothing else
// on the embedded nil DataStore is reached.
type gitConfigProbe struct {
	dataservices.DataStore

	// reads is a plain counter: each subtest builds its own probe and drives it from its
	// own goroutine, so there is nothing here for two goroutines to share.
	reads int
}

func (p *gitConfigProbe) Workflow() dataservices.WorkflowService {
	p.reads++

	return refusingWorkflowService{}
}

// refusingWorkflowService answers every read with an error of its own, so a call that gets
// past the gate fails by assertion and says why.
type refusingWorkflowService struct {
	dataservices.WorkflowService
}

func (refusingWorkflowService) Read(portainer.WorkflowID) (*portainer.Workflow, error) {
	return nil, errGitConfigWasRead
}

// errGitConfigWasRead names the thing the gate is supposed to come before, so a call that got
// past it says so in its own error as well as on the counter.
var errGitConfigWasRead = errors.New("the git config was read")

// secretReferenceStack is a stack holding one literal variable and one reference.
//
// workflowID is a parameter because it decides what remoteStack does before it ever reaches
// the docker client: a non-zero one sends it into the git-config block, which calls
// workflows.GitSourceAndArtifactForStack and therefore trips gitConfigProbe. That is what
// pins the refusal ahead of THAT block rather than merely somewhere in the function - with a
// zero WorkflowID the block is skipped, so a reviewer could move the gate below it and the
// test would stay green. The gated operations therefore use 1.
//
// The operations that are meant to walk past the gate use 0: they have to get as far as the
// docker client for their own assertion to mean anything.
func secretReferenceStack(workflowID portainer.WorkflowID) *portainer.Stack {
	return &portainer.Stack{
		Name:       "arcextension",
		WorkflowID: workflowID,
		EntryPoint: "docker-compose.yml",
		Env: []portainer.Pair{
			{Name: "LOG_LEVEL", Value: "debug"},
			{Name: "ADMIN_TOKEN", Value: "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"},
		},
	}
}

// Test_remoteStack_refusesSecretReferencesBeforeTouchingTheEnvironment pins the refusal
// ahead of the work every unpacker operation does before buildUnpackerCmdForStack is
// reached: reading the git config, creating a docker client and pulling the unpacker
// image, all for a deploy that was never going to proceed.
//
// Both halves of that ordering are load bearing and both are enforced. The git config comes
// first and is reached only for a stack with a WorkflowID, so the refused stack carries one
// and gitConfigProbe turns an ungated call into a counted, asserted failure. The docker
// client comes next, and the endpoint can never yield one, so a call that gets that far
// fails with an unrelated error instead and these assertions go red.
func Test_remoteStack_refusesSecretReferencesBeforeTouchingTheEnvironment(t *testing.T) {
	t.Parallel()

	stack := secretReferenceStack(1)
	endpoint := &portainer.Endpoint{Type: portainer.AzureEnvironment}

	newDeployer := func() (*stackDeployer, *gitConfigProbe) {
		probe := &gitConfigProbe{}

		return &stackDeployer{lock: &sync.Mutex{}, composeStackManager: &composeManagerStub{}, dataStore: probe}, probe
	}

	// Every entry point of deployer_remote.go, so a new one cannot be added with the
	// same shape and no check.
	refused := map[string]func(d *stackDeployer) error{
		"DeployRemoteComposeStack": func(d *stackDeployer) error {
			return d.DeployRemoteComposeStack(t.Context(), stack, endpoint, nil, false, true, false)
		},
		"StartRemoteComposeStack": func(d *stackDeployer) error {
			return d.StartRemoteComposeStack(t.Context(), stack, endpoint, nil)
		},
		"DeployRemoteSwarmStack": func(d *stackDeployer) error {
			return d.DeployRemoteSwarmStack(t.Context(), stack, endpoint, nil, false, true)
		},
		"StartRemoteSwarmStack": func(d *stackDeployer) error {
			return d.StartRemoteSwarmStack(t.Context(), stack, endpoint, nil)
		},
	}

	for name, call := range refused {
		t.Run(name+" refuses the reference before reaching the environment", func(t *testing.T) {
			t.Parallel()

			deployer, probe := newDeployer()

			err := call(deployer)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ADMIN_TOKEN")
			assert.Contains(t, err.Error(), "arcextension")

			// The git config is never read, which is the first half of the ordering and the
			// half a zero counter is the whole proof of: move the gate below that block and
			// this goes to 1 and the message below changes, by assertion rather than by
			// signal. See gitConfigProbe.
			assert.Zero(t, probe.reads)
			assert.NotContains(t, err.Error(), errGitConfigWasRead.Error())

			// The docker client is never created, so no image is pulled either.
			assert.NotContains(t, err.Error(), "docker client")
			assert.False(t, deployer.composeStackManager.(*composeManagerStub).pulled)

			// And the wording is the builder's, so the two checks cannot drift apart.
			_, fromBuilder := deployer.buildUnpackerCmdForStack(stack, OperationDeploy, unpackerCmdBuilderOptions{})
			require.Error(t, fromBuilder)
			assert.Equal(t, fromBuilder.Error(), err.Error())
		})
	}

	// Removal and stop address the project by name and pass no environment, so they must
	// stay usable on a stack that has migrated to references - otherwise a user could
	// migrate a stack and then no longer be able to clean it up.
	//
	// No WorkflowID on this one: these calls are supposed to run past the gate and fail at
	// the docker client, so the git-config block they would hit on the way must not be the
	// thing that stops them. See secretReferenceStack.
	ungated := secretReferenceStack(0)

	unaffected := map[string]func(d *stackDeployer) error{
		"UndeployRemoteComposeStack": func(d *stackDeployer) error {
			return d.UndeployRemoteComposeStack(t.Context(), ungated, endpoint)
		},
		"StopRemoteComposeStack": func(d *stackDeployer) error {
			return d.StopRemoteComposeStack(t.Context(), ungated, endpoint)
		},
		"UndeployRemoteSwarmStack": func(d *stackDeployer) error {
			return d.UndeployRemoteSwarmStack(t.Context(), ungated, endpoint)
		},
		"StopRemoteSwarmStack": func(d *stackDeployer) error {
			return d.StopRemoteSwarmStack(t.Context(), ungated, endpoint)
		},
	}

	for name, call := range unaffected {
		t.Run(name+" is not gated on the reference", func(t *testing.T) {
			t.Parallel()

			// It gets as far as the environment and fails there, which is the proof that
			// the reference did not stop it.
			deployer, _ := newDeployer()

			err := call(deployer)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "docker client")
			assert.NotContains(t, err.Error(), "ADMIN_TOKEN")
		})
	}
}

// Test_remoteStack_refusesAnUnknownOperation pins the explicit lookup that the gate above
// depends on.
//
// funcmap[operation] on an unknown key yields the zero unpackerCmd, whose usesEnv is false -
// so with an indexing expression the secret-reference check silently does not run, and the
// operation is only refused at the very end of remoteStack, after the git config has been
// read, a docker client created and the unpacker image pulled. Unreachable through the eight
// exported entry points, all of which pass a constant, and pinned anyway so that "one check
// covers them all" holds by construction rather than by inventory.
func Test_remoteStack_refusesAnUnknownOperation(t *testing.T) {
	t.Parallel()

	probe := &gitConfigProbe{}
	deployer := &stackDeployer{lock: &sync.Mutex{}, composeStackManager: &composeManagerStub{}, dataStore: probe}

	err := deployer.remoteStack(
		t.Context(),
		secretReferenceStack(1),
		&portainer.Endpoint{Type: portainer.AzureEnvironment},
		StackRemoteOperation("compose-teleport"),
		unpackerCmdBuilderOptions{},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown stack operation")

	// And refused before anything was touched: the stack carries a WorkflowID, so a lookup
	// that fell through to the git config would show up on the probe. See gitConfigProbe for
	// why this is a counter and not a nil dataStore.
	assert.Zero(t, probe.reads)
	assert.NotContains(t, err.Error(), "docker client")
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

// stackFileServiceStub serves one compose file to the validation step.
type stackFileServiceStub struct {
	portainer.FileService
	content []byte
}

func (s stackFileServiceStub) GetFileContent(trustedRootPath, filePath string) ([]byte, error) {
	return s.content, nil
}

// countingDeployer records how many times the compose deploy was actually reached.
type countingDeployer struct {
	noopDeployer
	composeDeploys int
}

func (d *countingDeployer) DeployComposeStack(_ context.Context, stack *portainer.Stack, endpoint *portainer.Endpoint, registries []portainer.Registry, prune, forcePullImage, forceRecreate bool) error {
	d.composeDeploys++

	return nil
}

// Test_ComposeStackDeploymentConfig_Deploy_gatesSecretReferencesOnAdmin pins the deploy-time
// half of the rule in docs/secret-resolver.md §3.11: for a user who is not an administrator
// or an environment administrator, stackutils.ValidateStackFiles enforces the environment's
// policy against the interpolated *reference* while the deploy runs on the interpolated
// *value*, so the policy check and the containers see different strings. The deploy is
// refused for that class of user rather than left to run on an unvalidated string.
func Test_ComposeStackDeploymentConfig_Deploy_gatesSecretReferencesOnAdmin(t *testing.T) {
	t.Parallel()

	const composeFile = `services:
  app:
    image: nginx
`

	reference := []portainer.Pair{
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "ADMIN_TOKEN", Value: "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"},
	}

	tests := []struct {
		name        string
		role        portainer.UserRole
		env         []portainer.Pair
		wantRefused bool
	}{
		{
			name:        "a regular user cannot deploy a stack carrying a reference",
			role:        portainer.StandardUserRole,
			env:         reference,
			wantRefused: true,
		},
		{
			// The gate must bite on references and on nothing else: an ordinary stack of
			// the same user goes through untouched.
			name: "a regular user's ordinary stack is unaffected",
			role: portainer.StandardUserRole,
			env:  []portainer.Pair{{Name: "LOG_LEVEL", Value: "debug"}},
		},
		{
			// The escaped marker is a literal, not a reference: validation and deploy see
			// the same string, so there is no divergence to close and nothing to refuse.
			name: "a regular user's value that only looks like a reference is unaffected",
			role: portainer.StandardUserRole,
			env:  []portainer.Pair{{Name: "APP_URI", Value: "secret::app/config"}},
		},
		{
			// An administrator is not subject to the policy this gate protects, so gating
			// one would only break the feature for the users expected to run it.
			name: "an administrator is not gated",
			role: portainer.AdministratorRole,
			env:  reference,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			deployer := &countingDeployer{}

			config := &ComposeStackDeploymentConfig{
				stack: &portainer.Stack{
					Name:       "arcextension",
					EntryPoint: "docker-compose.yml",
					Env:        test.env,
				},
				endpoint:      &portainer.Endpoint{},
				user:          &portainer.User{Role: test.role},
				FileService:   stackFileServiceStub{content: []byte(composeFile)},
				StackDeployer: deployer,
			}

			err := config.Deploy(t.Context())

			if test.wantRefused {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "ADMIN_TOKEN")
				assert.Contains(t, err.Error(), "arcextension")
				assert.Zero(t, deployer.composeDeploys)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, 1, deployer.composeDeploys)
		})
	}
}

// Test_ComposeStackDeploymentConfig_Deploy_gatesAnInlineBodyReference pins the same rule for
// a reference written inline in the body, where the divergence is wider still: the deploy
// rewrites that scalar into a placeholder and interpolates the value, so the string
// ValidateStackFiles inspected is not the string that starts the containers.
func Test_ComposeStackDeploymentConfig_Deploy_gatesAnInlineBodyReference(t *testing.T) {
	t.Parallel()

	body := []byte(`services:
  app:
    image: nginx
    environment:
      METRICS_TOKEN: secret:vw:stack/nebula/arcextension/METRICS_TOKEN
`)

	newConfig := func(role portainer.UserRole, deployer *countingDeployer) *ComposeStackDeploymentConfig {
		return &ComposeStackDeploymentConfig{
			stack: &portainer.Stack{
				Name:       "arcextension",
				EntryPoint: "docker-compose.yml",
			},
			endpoint:      &portainer.Endpoint{},
			user:          &portainer.User{Role: role},
			FileService:   stackFileServiceStub{content: body},
			StackDeployer: deployer,
		}
	}

	t.Run("a regular user cannot deploy it", func(t *testing.T) {
		t.Parallel()

		deployer := &countingDeployer{}

		err := newConfig(portainer.StandardUserRole, deployer).Deploy(t.Context())
		require.Error(t, err)

		// The file is named, because a body reference has no variable name to be named by.
		assert.Contains(t, err.Error(), "docker-compose.yml")
		assert.Contains(t, err.Error(), "arcextension")
		assert.Zero(t, deployer.composeDeploys)
	})

	t.Run("an administrator is not gated", func(t *testing.T) {
		t.Parallel()

		deployer := &countingDeployer{}

		require.NoError(t, newConfig(portainer.AdministratorRole, deployer).Deploy(t.Context()))
		assert.Equal(t, 1, deployer.composeDeploys)
	})
}

// Test_ComposeStackDeploymentConfig_Deploy_refusesAReferenceWithoutAnEndpoint pins the
// fail-closed direction: a missing endpoint means no policy check ran at all, which is a
// reason to refuse a non-administrator rather than to let the deploy through.
func Test_ComposeStackDeploymentConfig_Deploy_refusesAReferenceWithoutAnEndpoint(t *testing.T) {
	t.Parallel()

	deployer := &countingDeployer{}

	config := &ComposeStackDeploymentConfig{
		stack: &portainer.Stack{
			Name:       "arcextension",
			EntryPoint: "docker-compose.yml",
			Env:        []portainer.Pair{{Name: "ADMIN_TOKEN", Value: "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"}},
		},
		user:          &portainer.User{Role: portainer.StandardUserRole},
		FileService:   stackFileServiceStub{content: []byte("services:\n  app:\n    image: nginx\n")},
		StackDeployer: deployer,
	}

	err := config.Deploy(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ADMIN_TOKEN")
	assert.Zero(t, deployer.composeDeploys)
}

// hostileFiller is one class of byte for hostileName to overrun the bound with, named by what
// %q costs to print it.
//
// The class is what decides the size of the finished message, because TruncateName bounds the
// INPUT of the verb rather than its output: %q renders an invalid UTF-8 byte and a C0 byte as
// four characters each, a backslash and U+2028 as two per input byte, and an ordinary printable
// as one. See secretresolver.TruncateName.
//
// "Non-printable multi-byte rune" is not a class with one cost, and U+2028 does not speak for
// it: a C1 control prints as \u00NN, three per input byte, and an astral non-printable as
// \UNNNNNNNN, two and a half. U+2028 is here because it is the one that ties the backslash at
// two, not because it represents the others.
type hostileFiller struct {
	// name goes into the subtest name.
	name string

	// text is repeated until the bound is overrun many times over.
	text string
}

// hostileFillers are the classes the ceiling below has to hold against, worst first.
//
// The fixture is parameterised over them because 'A', which this test used to use on its own,
// is the cheapest of them: %q leaves it alone. A ceiling asserted only against 'A' therefore
// passes for a reason unrelated to what it claims - the non-administrator gate measures 690
// bytes filled with 'A' against 2136 filled with invalid UTF-8 - which is the exact failure
// mode this feature exists to catch.
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
// name: the sentence and the wrapping. A flat number rather than a derived one, because it
// bounds text we author and review rather than text a caller supplies - the longest of these
// messages carries 154 bytes of it - and it sits well over that so the prose can be reworded
// without anybody having to retune a ceiling.
const maxFixedMessageBytes = 512

// maxRefusalMessageBytes is the ceiling the refusals below are held to: the authored allowance
// plus two worst-case names, which is the most any message here names.
//
// The previous value was a flat 2048, and the worst case exceeds it - two bounded names of
// invalid UTF-8 print as 2058 bytes between them before a word of the message is added, and the
// non-administrator gate measures 2212. It held only because the fixture was filled with 'A'.
var maxRefusalMessageBytes = maxFixedMessageBytes + 2*worstCaseQuotedNameBytes

// assertBoundedAndEscaped holds one persisted refusal message to the ceiling, to the absence
// of raw control bytes, and to still naming what it is about.
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

// Test_secretRefusals_boundAndEscapeHostileNames pins both halves of the fix at every site in
// this package that names a stack or a variable in a refusal.
//
// The channel: stackutils.UpdateStackStatusFromDeploymentResult writes these messages verbatim
// into Stack.DeploymentStatus[].Message, which Portainer persists and StackInspect serves back
// to operators and to agents. Neither name is validated on the way in - a stack's Env comes
// straight out of the request body and updateComposeStackPayload.Validate looks only at the
// compose file - so both the length and the bytes are the caller's.
//
// Measured on a copy with these inputs before the fix, a mebibyte in the variable name and an
// ordinary stack name: refuseSecretReferencesForNonAdmin 1048755 bytes, getEnv 1048710,
// checkNoSecretReferences 1048732, each with the NUL, the ESC and the newline intact. A
// mebibyte in both names put the first and the third over 2 MiB.
//
// Reverting either half of the fix at any one site fails this test at that site: dropping the
// bound fails the size assertion, dropping %q fails the control-byte assertion. See
// secretresolver.TruncateName.
//
// Every site is run once per byte class in hostileFillers, because the bound applies to the
// input of %q and the classes cost between one and four printed bytes each. Measured here, per
// class, for the non-administrator gate and the two unpacker sites that name both names:
//
//	invalid UTF-8, NUL   gate 2136   unpacker 2113
//	U+2028               gate 1168   unpacker 1145
//	backslash            gate 1172   unpacker 1149
//	ordinary printable   gate  690   unpacker  667
//
// The last row is what this fixture used to assert on its own, and 690 bytes clears a 2048
// ceiling three times over without ever approaching it - while the first row does not clear it
// at all.
func Test_secretRefusals_boundAndEscapeHostileNames(t *testing.T) {
	t.Parallel()

	for _, filler := range hostileFillers {
		t.Run("filled with "+filler.name, func(t *testing.T) {
			t.Parallel()

			stack := &portainer.Stack{
				Name: hostileName("STACKNAME", filler.text),
				Env: []portainer.Pair{
					{Name: hostileName("VARNAME", filler.text), Value: "secret:vw:stack/nebula/arcextension/ADMIN_TOKEN"},
				},
			}

			t.Run("the non-administrator gate", func(t *testing.T) {
				t.Parallel()

				assertBoundedAndEscaped(t, refuseSecretReferencesForNonAdmin(stack, stackFileServiceStub{}), "STACKNAME", "VARNAME")
			})

			t.Run("the unpacker environment builder", func(t *testing.T) {
				t.Parallel()

				_, err := getEnv(stack.Name, stack.Env)

				// getEnv names the variable only; its caller adds the stack.
				assertBoundedAndEscaped(t, err, "VARNAME")
			})

			t.Run("the unpacker check hoisted ahead of the pull", func(t *testing.T) {
				t.Parallel()

				assertBoundedAndEscaped(t, checkNoSecretReferences(stack), "STACKNAME", "VARNAME")
			})

			t.Run("the unpacker command builder", func(t *testing.T) {
				t.Parallel()

				deployer := &stackDeployer{}

				_, err := deployer.buildUnpackerCmdForStack(stack, OperationDeploy, unpackerCmdBuilderOptions{})
				assertBoundedAndEscaped(t, err, "STACKNAME", "VARNAME")
			})
		})
	}
}
