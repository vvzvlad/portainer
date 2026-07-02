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
