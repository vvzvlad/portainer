package containerautomation

import "testing"

func TestInScope(t *testing.T) {
	tests := []struct {
		name   string
		scope  string
		labels map[string]string
		want   bool
	}{
		{"labeled: no labels", ScopeLabeled, nil, false},
		{"labeled: enable true (primary)", ScopeLabeled, map[string]string{labelEnable: "true"}, true},
		{"labeled: enable true (alias)", ScopeLabeled, map[string]string{labelEnableAlias: "true"}, true},
		{"labeled: enable false", ScopeLabeled, map[string]string{labelEnable: "false"}, false},
		{"labeled: enable bad value", ScopeLabeled, map[string]string{labelEnable: "yepp"}, false},
		{"labeled: primary wins over alias", ScopeLabeled, map[string]string{labelEnable: "true", labelEnableAlias: "false"}, true},
		{"all: no labels", ScopeAll, nil, true},
		{"all: enable true", ScopeAll, map[string]string{labelEnable: "true"}, true},
		{"all: explicit opt-out", ScopeAll, map[string]string{labelEnable: "false"}, false},
		{"all: opt-out via alias", ScopeAll, map[string]string{labelEnableAlias: "0"}, false},
		{"all: bad value is not opt-out", ScopeAll, map[string]string{labelEnable: "nope"}, false},
		{"unknown scope falls back to labeled", "weird", map[string]string{labelEnable: "true"}, true},
		{"unknown scope, no label", "weird", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InScope(tt.scope, tt.labels); got != tt.want {
				t.Errorf("InScope(%q, %v) = %v, want %v", tt.scope, tt.labels, got, tt.want)
			}
		})
	}
}

func TestStopTimeout(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{"default when missing", nil, defaultStopTimeout},
		{"primary value", map[string]string{labelStopTimeout: "25"}, 25},
		{"alias value", map[string]string{labelStopTimeoutAlias: "15"}, 15},
		{"primary wins over alias", map[string]string{labelStopTimeout: "25", labelStopTimeoutAlias: "15"}, 25},
		{"bad value falls back", map[string]string{labelStopTimeout: "abc"}, defaultStopTimeout},
		{"zero falls back", map[string]string{labelStopTimeout: "0"}, defaultStopTimeout},
		{"negative falls back", map[string]string{labelStopTimeout: "-5"}, defaultStopTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StopTimeout(tt.labels); got != tt.want {
				t.Errorf("StopTimeout(%v) = %d, want %d", tt.labels, got, tt.want)
			}
		})
	}
}

func TestMaxRetries(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{"default when missing", nil, defaultRetries},
		{"explicit value", map[string]string{labelRetries: "5"}, 5},
		{"bad value falls back", map[string]string{labelRetries: "lots"}, defaultRetries},
		{"zero falls back", map[string]string{labelRetries: "0"}, defaultRetries},
		{"no alias for retries", map[string]string{"autoheal.retries": "7"}, defaultRetries},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaxRetries(tt.labels); got != tt.want {
				t.Errorf("MaxRetries(%v) = %d, want %d", tt.labels, got, tt.want)
			}
		})
	}
}

func TestInUpdateScope(t *testing.T) {
	tests := []struct {
		name   string
		scope  string
		labels map[string]string
		want   bool
	}{
		{"labeled: no labels", ScopeLabeled, nil, false},
		{"labeled: enable true (primary)", ScopeLabeled, map[string]string{labelUpdateEnable: "true"}, true},
		{"labeled: enable true (watchtower alias)", ScopeLabeled, map[string]string{labelUpdateEnableAlias: "true"}, true},
		{"labeled: enable false", ScopeLabeled, map[string]string{labelUpdateEnable: "false"}, false},
		{"labeled: enable bad value", ScopeLabeled, map[string]string{labelUpdateEnable: "soon"}, false},
		{"labeled: primary wins over alias", ScopeLabeled, map[string]string{labelUpdateEnable: "true", labelUpdateEnableAlias: "false"}, true},
		{"all: no labels", ScopeAll, nil, true},
		{"all: enable true", ScopeAll, map[string]string{labelUpdateEnable: "true"}, true},
		{"all: explicit opt-out", ScopeAll, map[string]string{labelUpdateEnable: "false"}, false},
		{"all: opt-out via watchtower alias", ScopeAll, map[string]string{labelUpdateEnableAlias: "0"}, false},
		{"all: bad value is not opt-out", ScopeAll, map[string]string{labelUpdateEnable: "nope"}, false},
		{"unknown scope falls back to labeled", "weird", map[string]string{labelUpdateEnable: "true"}, true},
		{"unknown scope, no label", "weird", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InUpdateScope(tt.scope, tt.labels); got != tt.want {
				t.Errorf("InUpdateScope(%q, %v) = %v, want %v", tt.scope, tt.labels, got, tt.want)
			}
		})
	}
}

func TestIsMonitorOnly(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"no labels", nil, false},
		{"primary true", map[string]string{labelUpdateMonitorOnly: "true"}, true},
		{"watchtower alias true", map[string]string{labelUpdateMonitorOnlyAlias: "true"}, true},
		{"primary false", map[string]string{labelUpdateMonitorOnly: "false"}, false},
		{"bad value", map[string]string{labelUpdateMonitorOnly: "maybe"}, false},
		{"primary wins over alias", map[string]string{labelUpdateMonitorOnly: "true", labelUpdateMonitorOnlyAlias: "false"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMonitorOnly(tt.labels); got != tt.want {
				t.Errorf("IsMonitorOnly(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}
