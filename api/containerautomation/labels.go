package containerautomation

import "strconv"

const (
	// Scope values shared by the auto-heal and auto-update global settings.
	ScopeLabeled = "labeled"
	ScopeAll     = "all"

	// Primary labels (with community aliases) controlling per-container auto-heal.
	labelEnable           = "io.portainer.autoheal.enable"
	labelEnableAlias      = "autoheal"
	labelStopTimeout      = "io.portainer.autoheal.stop-timeout"
	labelStopTimeoutAlias = "autoheal.stop.timeout"
	labelRetries          = "io.portainer.autoheal.retries"

	// Primary labels (with watchtower aliases) controlling per-container auto-update.
	labelUpdateEnable           = "io.portainer.update.enable"
	labelUpdateEnableAlias      = "com.centurylinklabs.watchtower.enable"
	labelUpdateMonitorOnly      = "io.portainer.update.monitor-only"
	labelUpdateMonitorOnlyAlias = "com.centurylinklabs.watchtower.monitor-only"

	// composeProjectLabel identifies the compose project a container belongs to.
	composeProjectLabel = "com.docker.compose.project"

	// Defaults used when a label is missing or holds an invalid value.
	defaultStopTimeout = 10
	defaultRetries     = 3
)

// InScope reports whether a container is subject to auto-heal given the global
// scope and the container's labels.
//
//   - "all": every container is in scope, unless it explicitly opts out with the
//     enable label set to false.
//   - "labeled" (default): only containers with the enable label set to true.
func InScope(scope string, labels map[string]string) bool {
	enabled, present := boolLabel(labels, labelEnable, labelEnableAlias)

	switch scope {
	case ScopeAll:
		if present && !enabled {
			return false
		}

		return true
	default: // ScopeLabeled
		return present && enabled
	}
}

// boolLabel resolves a boolean label (primary key first, alias second).
// It returns the parsed value and whether the label was present at all.
// Invalid values are treated as false but still count as "present".
func boolLabel(labels map[string]string, key, alias string) (value bool, present bool) {
	raw, ok := labels[key]
	if !ok {
		raw, ok = labels[alias]
	}

	if !ok {
		return false, false
	}

	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, true
	}

	return parsed, true
}

// InUpdateScope reports whether a container is subject to auto-update given the
// global scope and the container's labels. It mirrors InScope but reads the
// update enable label (io.portainer.update.enable / watchtower alias):
//
//   - "all": every container is in scope, unless it explicitly opts out with the
//     update enable label set to false.
//   - "labeled" (default): only containers with the update enable label true.
func InUpdateScope(scope string, labels map[string]string) bool {
	enabled, present := boolLabel(labels, labelUpdateEnable, labelUpdateEnableAlias)

	switch scope {
	case ScopeAll:
		if present && !enabled {
			return false
		}

		return true
	default: // ScopeLabeled
		return present && enabled
	}
}

// IsMonitorOnly reports whether a container is flagged detect-only via the
// monitor-only label (io.portainer.update.monitor-only / watchtower alias).
// Such containers have their image status resolved (for the badge cache) but are
// never auto-applied.
func IsMonitorOnly(labels map[string]string) bool {
	value, present := boolLabel(labels, labelUpdateMonitorOnly, labelUpdateMonitorOnlyAlias)

	return present && value
}

// UpdateKind is the apply path resolved for an outdated container.
type UpdateKind string

const (
	// UpdateStandalone: recreate-with-pull (no compose project).
	UpdateStandalone UpdateKind = "standalone"
	// UpdateStack: redeploy the owning Portainer compose stack with re-pull, so
	// the container stays part of its stack.
	UpdateStack UpdateKind = "stack"
	// UpdateExternal: compose-managed but with no matching Portainer compose
	// stack record; Portainer must not touch it (would detach it / drift).
	UpdateExternal UpdateKind = "external"
)

// StackMatch is the Portainer Docker Compose stack a compose project resolves to.
type StackMatch struct {
	StackID int
	// IsGit routes file vs git redeploy at apply time.
	IsGit bool
}

// UpdateRouting is the decision returned by resolveContainerUpdateRouting.
type UpdateRouting struct {
	Kind    UpdateKind
	StackID int
	IsGit   bool
}

// resolveContainerUpdateRouting decides how a container's image update must be
// applied, given a lookup that resolves a compose project name to a matching
// Portainer Docker Compose stack (nil when none exists or it is not a compose
// stack). It is the Go equivalent of M3's TS resolveContainerUpdatePath: pure
// and side-effect free so it can be unit-tested without Docker or the datastore.
//
//   - No compose project label -> standalone (recreate-with-pull).
//   - Compose project matching a Portainer compose stack -> stack
//     (redeploy-with-pull, keeps the container in its stack).
//   - Compose project with no matching Portainer compose stack -> external
//     (managed outside Portainer / a same-named stack of another type), left
//     untouched to avoid detaching it or drifting.
func resolveContainerUpdateRouting(labels map[string]string, stackLookup func(project string) *StackMatch) UpdateRouting {
	project := labels[composeProjectLabel]
	if project == "" {
		return UpdateRouting{Kind: UpdateStandalone}
	}

	match := stackLookup(project)
	if match == nil {
		return UpdateRouting{Kind: UpdateExternal}
	}

	return UpdateRouting{Kind: UpdateStack, StackID: match.StackID, IsGit: match.IsGit}
}

// UpdateCandidate is an outdated, in-scope container considered for auto-update.
type UpdateCandidate struct {
	ID string
	// Name is the container's primary name (no leading slash). It is stable across
	// a recreate and keys the update->rollback loop guard.
	Name string
	// ImageID is the pre-update local image id ("sha256:..."), the "old" digest in a
	// per-container update notification.
	ImageID string
	// Image is the container's image reference (e.g. "nginx:latest"), carried for the
	// notification.
	Image  string
	Labels map[string]string
}

// StackUpdate identifies a Portainer stack to redeploy once, together with the
// affected member containers so each updated container can emit its own
// notification (with the stack name) after the redeploy.
type StackUpdate struct {
	StackID int
	IsGit   bool
	// Containers are the outdated member containers that triggered this stack
	// redeploy, threaded through from detection so a per-container notification can
	// be emitted for each (name + old image id + image + labels/stack name).
	Containers []UpdateCandidate
}

// GroupedUpdates partitions candidates into their apply paths, de-duplicating
// stack containers so each owning stack is redeployed at most once per tick
// (the overlap guard for stack fan-out). Pure and unit-testable, the Go analogue
// of M3's groupContainersForUpdate.
type GroupedUpdates struct {
	Standalone []UpdateCandidate
	External   []UpdateCandidate
	Stacks     []StackUpdate
}

// groupContainersForUpdate routes each candidate and collapses stack candidates
// so a stack with several outdated containers is redeployed only once.
func groupContainersForUpdate(candidates []UpdateCandidate, stackLookup func(project string) *StackMatch) GroupedUpdates {
	grouped := GroupedUpdates{}
	// stackIndex maps a stack id to its slot in grouped.Stacks so a stack is
	// redeployed once, while every member container is still collected for its own
	// notification (rather than discarded at the collapse).
	stackIndex := make(map[int]int)

	for _, c := range candidates {
		routing := resolveContainerUpdateRouting(c.Labels, stackLookup)
		switch routing.Kind {
		case UpdateStandalone:
			grouped.Standalone = append(grouped.Standalone, c)
		case UpdateExternal:
			grouped.External = append(grouped.External, c)
		case UpdateStack:
			idx, ok := stackIndex[routing.StackID]
			if !ok {
				grouped.Stacks = append(grouped.Stacks, StackUpdate{StackID: routing.StackID, IsGit: routing.IsGit})
				idx = len(grouped.Stacks) - 1
				stackIndex[routing.StackID] = idx
			}

			grouped.Stacks[idx].Containers = append(grouped.Stacks[idx].Containers, c)
		}
	}

	return grouped
}

// StopTimeout returns the per-container stop timeout (in seconds) from labels,
// falling back to the default when missing or invalid.
func StopTimeout(labels map[string]string) int {
	return positiveIntLabel(labels, labelStopTimeout, labelStopTimeoutAlias, defaultStopTimeout)
}

// MaxRetries returns the per-container max restarts per window from labels,
// falling back to the default when missing or invalid.
func MaxRetries(labels map[string]string) int {
	return positiveIntLabel(labels, labelRetries, "", defaultRetries)
}

// positiveIntLabel reads an integer label (primary first, optional alias second)
// and returns it when strictly positive, otherwise the provided default.
func positiveIntLabel(labels map[string]string, key, alias string, fallback int) int {
	raw, ok := labels[key]
	if !ok && alias != "" {
		raw, ok = labels[alias]
	}

	if !ok {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}

	return value
}
