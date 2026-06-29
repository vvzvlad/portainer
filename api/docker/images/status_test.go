package images

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
