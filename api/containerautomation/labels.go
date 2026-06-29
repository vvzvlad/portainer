package containerautomation

import "strconv"

const (
	// Scope values for the global auto-heal setting.
	ScopeLabeled = "labeled"
	ScopeAll     = "all"

	// Primary labels (with community aliases) controlling per-container auto-heal.
	labelEnable           = "io.portainer.autoheal.enable"
	labelEnableAlias      = "autoheal"
	labelStopTimeout      = "io.portainer.autoheal.stop-timeout"
	labelStopTimeoutAlias = "autoheal.stop.timeout"
	labelRetries          = "io.portainer.autoheal.retries"

	// Defaults used when a label is missing or holds an invalid value.
	defaultStopTimeout = 10
	defaultRetries     = 3
)

// parseEnable resolves the enable label (primary first, alias second).
// It returns the parsed boolean value and whether the label was present at all.
// Invalid values are treated as false but still count as "present".
func parseEnable(labels map[string]string) (enabled bool, present bool) {
	raw, ok := labels[labelEnable]
	if !ok {
		raw, ok = labels[labelEnableAlias]
	}

	if !ok {
		return false, false
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, true
	}

	return value, true
}

// InScope reports whether a container is subject to auto-heal given the global
// scope and the container's labels.
//
//   - "all": every container is in scope, unless it explicitly opts out with the
//     enable label set to false.
//   - "labeled" (default): only containers with the enable label set to true.
func InScope(scope string, labels map[string]string) bool {
	enabled, present := parseEnable(labels)

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
