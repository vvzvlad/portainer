package images

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

// fakeClientFactory hands the DigestClient a Docker client wired to a test server.
type fakeClientFactory struct {
	cli *dockerclient.Client
}

func (f fakeClientFactory) CreateClient(*portainer.Endpoint, string, *time.Duration) (*dockerclient.Client, error) {
	return f.cli, nil
}

// TestStatusCacheTTLIsShort is a regression guard for the stale-detection bug: the
// statusCache is keyed by the LOCAL imageID, which does not change when upstream
// pushes a new image under the same tag. A long TTL (the previous 24h) would serve
// a stale "updated" status and hide a freshly-pushed image from both the badge and
// the auto-update daemon for up to a day. The TTL must stay tied to the poll window
// (a few minutes), and entries set with the default expiration (0) must actually
// expire rather than live forever.
func TestStatusCacheTTLIsShort(t *testing.T) {
	require.LessOrEqual(t, statusCacheTTL, 10*time.Minute, "status cache TTL must be short, not 24h")

	key := "status-test-ttl-key"
	CacheResourceImageStatus(key, Updated)
	defer EvictImageStatus(key)

	_, exp, ok := statusCache.GetWithExpiration(key)
	require.True(t, ok)
	require.False(t, exp.IsZero(), "status entries must expire, not live forever")
	require.LessOrEqual(t, time.Until(exp), statusCacheTTL)
}

// TestContainerImageStatusForcedBypassesCache verifies the manual "re-check"
// contract: the default read serves the value cached under the local imageID,
// while the forced read skips that cached read and recomputes (here the image
// inspect is stubbed to fail, so the forced call surfaces a non-cached result
// instead of the seeded sentinel).
func TestContainerImageStatusForcedBypassesCache(t *testing.T) {
	const containerID = "force-test-container"
	const imageID = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/"+containerID+"/json") {
			resp := container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    containerID,
					Image: imageID,
				},
				Config: &container.Config{Image: "nginx:latest"},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Any other call (notably the image inspect on the forced recompute path)
		// fails, so the forced status is deterministically not the cached sentinel
		// and no real registry is contacted.
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost(srv.URL),
		dockerclient.WithHTTPClient(http.DefaultClient),
	)
	require.NoError(t, err)

	c := &DigestClient{clientFactory: fakeClientFactory{cli: cli}}

	// Seed the cache for the local imageID with a sentinel the recompute here
	// could never legitimately reproduce.
	CacheResourceImageStatus(imageID, Updated)
	defer EvictImageStatus(imageID)

	// Default (cached) read serves the seeded sentinel without recomputing.
	cached, err := c.ContainerImageStatus(context.Background(), containerID, &portainer.Endpoint{}, "")
	require.NoError(t, err)
	require.Equal(t, Updated, cached)

	// Forced read must bypass the cache: it recomputes and, with the image inspect
	// stubbed to fail, does not return the cached sentinel.
	forced, _ := c.ContainerImageStatusForced(context.Background(), containerID, &portainer.Endpoint{}, "")
	require.NotEqual(t, Updated, forced)
}

// TestCheckStatusForcedBypassesRemoteDigestCache proves the forced re-check also
// bypasses the short-lived remoteDigestCache (not just the statusCache): with a
// warm cache entry that matches the local digest, the default path returns
// "updated" without any registry HEAD, while the forced path ignores the cache
// and performs a fresh remote lookup (here against an unroutable registry, so it
// errors instead of serving the cached match).
func TestCheckStatusForcedBypassesRemoteDigestCache(t *testing.T) {
	// A tag on an unroutable registry: RemoteDigest fails fast (connection
	// refused) rather than reaching a real network.
	img, err := ParseImage(ParseImageOptions{Name: "127.0.0.1:1/portainer/forcecheck:latest"})
	require.NoError(t, err)

	localDigest := digest.Digest("sha256:" + strings.Repeat("a", 64))

	// Warm the remote digest cache with a value that matches the local digest.
	remoteDigestCache.Set(img.FullName(), localDigest, 0)
	defer remoteDigestCache.Delete(img.FullName())

	c := &DigestClient{}

	// Default path: reads the warm cache, remote == local -> Updated, no HEAD.
	cached, err := c.checkStatus(context.Background(), []*Image{&img}, []digest.Digest{localDigest}, false)
	require.NoError(t, err)
	require.Equal(t, Updated, cached)

	// Forced path: skips the cache and attempts a fresh remote lookup, which
	// fails against the unroutable registry instead of serving the cached match.
	forced, err := c.checkStatus(context.Background(), []*Image{&img}, []digest.Digest{localDigest}, true)
	require.Error(t, err)
	require.Equal(t, Error, forced)
}

func TestAggregateImageStatus(t *testing.T) {
	t.Parallel()

	f := func(statuses []Status, expected Status) {
		t.Helper()
		require.Equal(t, expected, AggregateImageStatus(statuses))
	}

	f([]Status{Skipped, Skipped, Skipped}, Skipped)
	f([]Status{Preparing, Preparing}, Preparing)
	f([]Status{Updated, Outdated, Processing, Error}, Outdated)
	f([]Status{Updated, Processing, Error}, Processing)
	f([]Status{Updated, Error}, Error)
	f([]Status{Updated, Updated}, Updated)
	f([]Status{}, Updated)
	f([]Status{Updated, Skipped}, Updated)
}

func TestCachedResourceImageStatusMiss(t *testing.T) {
	t.Parallel()

	_, err := CachedResourceImageStatus("status-test-miss-key")
	require.Error(t, err)
}

func TestCachedResourceImageStatusHitAndEvict(t *testing.T) {
	t.Parallel()

	key := "status-test-hit-evict-key"

	CacheResourceImageStatus(key, Updated)

	s, err := CachedResourceImageStatus(key)
	require.NoError(t, err)
	require.Equal(t, Updated, s)

	EvictImageStatus(key)

	_, err = CachedResourceImageStatus(key)
	require.Error(t, err)
}

func TestCacheErrorImageStatus(t *testing.T) {
	t.Parallel()

	key := "status-test-error-key"

	CacheErrorImageStatus(key)

	s, err := CachedResourceImageStatus(key)
	require.NoError(t, err)
	require.Equal(t, Error, s)

	EvictImageStatus(key)
}
