package images

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	portainer "github.com/portainer/portainer/api"
	consts "github.com/portainer/portainer/api/docker/consts"

	"github.com/opencontainers/go-digest"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// Status constants
const (
	Processing = Status("processing")
	Outdated   = Status("outdated")
	Updated    = Status("updated")
	Skipped    = Status("skipped")
	Preparing  = Status("preparing")
	Error      = Status("error")
)

const (
	// statusCacheTTL bounds how long a computed image status is served from the
	// statusCache. It is intentionally short (tied to the auto-update poll window),
	// NOT the previous 24h: the cache key is the LOCAL imageID, which does not
	// change when upstream pushes a new image under the same tag. A long TTL would
	// therefore keep serving a stale "updated" status for up to a day, and the
	// auto-update daemon (which resolves status through this same path) could not
	// see a freshly-pushed image within its poll interval. A few minutes still
	// absorbs bursts of badge lookups for the same image while re-checking the
	// remote digest soon after an upstream push.
	statusCacheTTL            = 5 * time.Minute
	errorStatusCacheTTL       = 5 * time.Minute
	maxConcurrentStatusChecks = 8

	// forcedRecheckMinInterval bounds how often a forced re-check (?force=true)
	// actually contacts the registry for the same local imageID. A manual re-check
	// bypasses the read caches and issues an outbound registry HEAD; because the
	// endpoint is available to any env-authorized user with no throttle, an
	// unbounded loop of forced calls would burn the instance's shared registry
	// pull-rate quota. Within this window a forced call reuses the just-computed
	// fresh result instead of re-HEADing. It is tied to the remoteDigestCache TTL so
	// a genuine manual re-check still gets a fresh answer the first time.
	forcedRecheckMinInterval = 5 * time.Second
)

var (
	statusCache       = cache.New(statusCacheTTL, statusCacheTTL)
	remoteDigestCache = cache.New(5*time.Second, 5*time.Second)
	swarmID2NameCache = cache.New(5*time.Second, 5*time.Second)

	// forcedRecheckGroup coalesces concurrent forced re-checks of the SAME local
	// imageID so N simultaneous ?force=true calls collapse to ONE registry HEAD
	// (singleflight), rather than each issuing its own outbound HEAD.
	forcedRecheckGroup singleflight.Group
	// forcedResultCache holds the last successful forced-recompute result per
	// imageID for forcedRecheckMinInterval, so rapid successive forced calls reuse it
	// instead of re-HEADing the registry. Only successful results are stored (errors
	// are never cached, so a transient failure does not suppress a real re-check).
	forcedResultCache = cache.New(forcedRecheckMinInterval, forcedRecheckMinInterval)
)

// Status holds Docker image  analysis
type Status string

func (c *DigestClient) ContainersImageStatus(ctx context.Context, containers []types.Container, endpoint *portainer.Endpoint) Status {
	cli, err := c.clientFactory.CreateClient(endpoint, "", nil)
	if err != nil {
		log.Error().Err(err).Msg("cannot create docker client")

		return Error
	}

	statuses := make([]Status, len(containers))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentStatusChecks)

	containerStatus := func(ct types.Container) Status {
		var nodeName string
		if swarmNodeId := ct.Labels[consts.SwarmNodeIDLabel]; swarmNodeId != "" {
			if swarmNodeName, ok := swarmID2NameCache.Get(swarmNodeId); ok {
				nodeName, _ = swarmNodeName.(string)
			} else {
				node, _, err := cli.NodeInspectWithRaw(ctx, swarmNodeId)
				if err != nil {
					return Error
				}

				nodeName = node.Description.Hostname
				swarmID2NameCache.Set(swarmNodeId, nodeName, 0)
			}
		}

		s, err := c.ContainerImageStatus(ctx, ct.ID, endpoint, nodeName)
		if err != nil {
			log.Warn().Str("containerId", ct.ID).Err(err).Msg("error when fetching image status for container")
			return Error
		}

		return s
	}

	for i, ct := range containers {
		g.Go(func() error {
			statuses[i] = containerStatus(ct)
			return nil
		})
	}

	_ = g.Wait()

	return AggregateImageStatus(statuses)
}

func AggregateImageStatus(statuses []Status) Status {
	if allMatch(statuses, Skipped) {
		return Skipped
	}

	if allMatch(statuses, Preparing) {
		return Preparing
	}

	if slices.Contains(statuses, Outdated) {
		return Outdated
	} else if slices.Contains(statuses, Processing) {
		return Processing
	} else if slices.Contains(statuses, Error) {
		return Error
	}

	return Updated
}

// ContainerImageStatus returns the image status for a container, serving a recent
// value from the statusCache when available (the default used by per-row badges,
// ContainersImageStatus and the auto-update daemon).
func (c *DigestClient) ContainerImageStatus(ctx context.Context, containerID string, endpoint *portainer.Endpoint, nodeName string) (Status, error) {
	return c.containerImageStatus(ctx, containerID, endpoint, nodeName, false)
}

// ContainerImageStatusForced recomputes the image status against the remote
// registry, bypassing the cached read while still refreshing the cache with the
// fresh result. It backs the UI's manual "re-check" action, where the user
// explicitly asks for an up-to-date registry comparison rather than the value
// cached for statusCacheTTL.
func (c *DigestClient) ContainerImageStatusForced(ctx context.Context, containerID string, endpoint *portainer.Endpoint, nodeName string) (Status, error) {
	return c.containerImageStatus(ctx, containerID, endpoint, nodeName, true)
}

func (c *DigestClient) containerImageStatus(ctx context.Context, containerID string, endpoint *portainer.Endpoint, nodeName string, force bool) (Status, error) {
	cli, err := c.clientFactory.CreateClient(endpoint, nodeName, nil)
	if err != nil {
		log.Warn().Str("swarmNodeId", nodeName).Msg("Cannot create new docker client.")
	}

	container, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		log.Warn().Err(err).Str("containerID", containerID).Msg("Inspect container error.")
		return Skipped, nil
	}

	var imageID string
	if strings.Contains(container.Image, "sha256") {
		imageID = container.Image[strings.Index(container.Image, "sha256"):]
	}

	if imageID == "" {
		return Skipped, nil
	}

	// statusCache is keyed by the LOCAL imageID and read here so every caller
	// (handler, ContainersImageStatus, the auto-update job) can skip the expensive,
	// rate-limited remote registry digest lookup below on a hit; the container/image
	// inspects above are cheap local Docker calls, the registry HEAD is the part
	// worth avoiding. The entry TTL is deliberately short (statusCacheTTL): because
	// the key is the local imageID, a new upstream image pushed under the same tag
	// leaves the key unchanged, so a long TTL would keep serving a stale "updated"
	// status (the full computation would now return "outdated") until it expired. A
	// short TTL re-checks the remote digest within the poll window. Both Outdated
	// and Skipped are cached too (only the error paths return early without caching).
	//
	// A forced re-check (force=true) skips this cached read and recomputes against
	// the registry (see forcedImageStatus), then writes the fresh result back into
	// the cache below.
	if !force {
		if s, err := CachedResourceImageStatus(imageID); err == nil {
			return s, nil
		}

		return c.computeImageStatus(ctx, cli, imageID, container.Config.Image, false)
	}

	return c.forcedImageStatus(ctx, cli, imageID, container.Config.Image)
}

// computeImageStatus performs the full, uncached image-status computation for a
// resolved local imageID: it parses the configured image reference, inspects the
// local image for its repo digests/tags, compares them against the remote registry
// digest (honoring force for the short-lived remoteDigestCache read in checkStatus)
// and writes the fresh result back into the statusCache. It is the shared body of
// both the default (cache-miss) and forced-recompute paths; the default path's
// behaviour is unchanged from the previous inline implementation.
func (c *DigestClient) computeImageStatus(ctx context.Context, cli *dockerclient.Client, imageID, configImage string, force bool) (Status, error) {
	digs := make([]digest.Digest, 0)
	images := make([]*Image, 0)
	if i, err := ParseImage(ParseImageOptions{Name: configImage}); err == nil {
		images = append(images, &i)
	}

	imageInspect, _, err := cli.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		log.Debug().Str("imageID", imageID).Msg("inspect failed")
		return Error, err
	}

	if len(imageInspect.RepoDigests) > 0 {
		digs = append(digs, ParseRepoDigests(imageInspect.RepoDigests)...)
	}

	if len(imageInspect.RepoTags) > 0 {
		images = append(images, ParseRepoTags(imageInspect.RepoTags)...)
	}

	s, err := c.checkStatus(ctx, images, digs, force)
	if err != nil {
		log.Debug().Str("image", configImage).Err(err).Msg("fetching a certain image status")
		return Error, err
	}

	statusCache.Set(imageID, s, 0)

	return s, nil
}

// forcedImageStatus recomputes the image status against the registry for a manual
// re-check while bounding the registry load a ?force=true call can cause:
//
//   - forcedResultCache serves the just-computed fresh result for
//     forcedRecheckMinInterval, so rapid successive forced calls reuse it instead
//     of re-HEADing the registry (min-interval throttle).
//   - forcedRecheckGroup (singleflight) shares one in-flight computation per
//     imageID, so N concurrent forced calls collapse to a single registry HEAD.
//
// A successful recompute still repopulates both the statusCache and (via
// checkStatus) the remoteDigestCache, so an immediately following default read is
// served from cache. Failures are not cached, so a transient error never suppresses
// a genuine re-check.
func (c *DigestClient) forcedImageStatus(ctx context.Context, cli *dockerclient.Client, imageID, configImage string) (Status, error) {
	if s, ok := forcedResultCache.Get(imageID); ok {
		return s.(Status), nil
	}

	v, err, _ := forcedRecheckGroup.Do(imageID, func() (any, error) {
		// A concurrent forced call may have populated the result between our read
		// above and entering the flight; reuse it rather than HEAD the registry again.
		if s, ok := forcedResultCache.Get(imageID); ok {
			return s.(Status), nil
		}

		s, err := c.computeImageStatus(ctx, cli, imageID, configImage, true)
		if err != nil {
			return Error, err
		}

		forcedResultCache.Set(imageID, s, 0)

		return s, nil
	})
	if err != nil {
		return Error, err
	}

	return v.(Status), nil
}

func (c *DigestClient) ServiceImageStatus(ctx context.Context, serviceID string, endpoint *portainer.Endpoint) (Status, error) {
	cli, err := c.clientFactory.CreateClient(endpoint, "", nil)
	if err != nil {
		return Error, nil
	}

	containers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", consts.SwarmServiceIDLabel+"="+serviceID)),
	})
	if err != nil {
		log.Warn().Err(err).Str("serviceID", serviceID).Msg("cannot list container for the service")
		return Error, err
	}

	nonExistedOrStoppedContainers := make([]types.Container, 0)
	for _, container := range containers {
		if container.State == "exited" || container.State == "stopped" {
			continue
		}

		// When there is a container with the state "Created" under the service, it
		// indicates that the Docker Swarm is replacing the existing task with
		// a new task. At the moment, the state of the new task is "Created", and
		// the state of the old task is "Running".
		// Until the new task runs up, the image status should be set "Preparing"
		if container.State == "created" {
			return Preparing, nil
		}
		nonExistedOrStoppedContainers = append(nonExistedOrStoppedContainers, container)
	}

	if len(nonExistedOrStoppedContainers) == 0 {
		return Preparing, nil
	}

	return c.ContainersImageStatus(ctx, nonExistedOrStoppedContainers, endpoint), nil
}

func (c *DigestClient) checkStatus(ctx context.Context, images []*Image, digests []digest.Digest, force bool) (Status, error) {
	if digests == nil {
		digests = make([]digest.Digest, 0)
	}

	for _, img := range images {
		if img.Digest != "" && !slices.Contains(digests, img.Digest) {
			log.Info().Str("localDigest", img.Domain).Msg("incoming local digest is not nil")
			digests = append([]digest.Digest{img.Digest}, digests...)
		}
	}

	if len(digests) == 0 {
		return Skipped, nil
	}

	var imageStatus Status

	for _, img := range images {
		var remoteDigest digest.Digest
		var err error
		// A forced re-check skips the short-lived remoteDigestCache read so it
		// actually HEADs the registry; the fresh digest is still written back
		// below. The default path keeps reusing the cache (auto-badges and the
		// auto-update daemon must not add registry load).
		if !force {
			if rd, ok := remoteDigestCache.Get(img.FullName()); ok {
				remoteDigest, _ = rd.(digest.Digest)
			}
		}
		if remoteDigest == "" {
			remoteDigest, err = c.RemoteDigest(ctx, *img)
			if err != nil {
				log.Error().Str("image", img.String()).Msg("error when fetch remote digest for image")
				return Error, err
			}
		}
		remoteDigestCache.Set(img.FullName(), remoteDigest, 0)

		log.Debug().Str("image", img.FullName()).Stringer("remote_digest", remoteDigest).
			Int("local_digest_size", len(digests)).
			Msg("Digests")

		// final locals vs remote one
		for _, dig := range digests {
			log.Debug().
				Str("image", img.FullName()).
				Stringer("remote_digest", remoteDigest).
				Stringer("local_digest", dig).
				Msg("Comparing")

			if dig == remoteDigest {
				log.Debug().Str("image", img.FullName()).
					Stringer("remote_digest", remoteDigest).
					Stringer("local_digest", dig).
					Msg("Found a match")
				return Updated, nil
			}
		}
	}

	imageStatus = Outdated

	return imageStatus, nil
}

func CachedResourceImageStatus(resourceID string) (Status, error) {
	if s, ok := statusCache.Get(resourceID); ok {
		return s.(Status), nil
	}

	return "", errors.Errorf("no image found in cache: %s", resourceID)
}

func CacheResourceImageStatus(resourceID string, status Status) {
	statusCache.Set(resourceID, status, 0)
}

func CacheErrorImageStatus(resourceID string) {
	statusCache.Set(resourceID, Error, errorStatusCacheTTL)
}

func CachedImageDigest(resourceID string) (Status, error) {
	if s, ok := statusCache.Get(resourceID); ok {
		return s.(Status), nil
	}

	return "", errors.Errorf("no image found in cache: %s", resourceID)
}

func EvictImageStatus(resourceID string) {
	statusCache.Delete(resourceID)
}

func allMatch(statuses []Status, status Status) bool {
	if len(statuses) == 0 {
		return false
	}

	for _, s := range statuses {
		if s != status {
			return false
		}
	}

	return true
}
