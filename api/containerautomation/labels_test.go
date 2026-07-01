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

func TestResolveContainerUpdateRouting(t *testing.T) {
	// stackLookup resolves "my-stack" to compose stack 7 (git) and nothing else,
	// mirroring how the job builds a per-endpoint compose-stack index.
	stackLookup := func(project string) *StackMatch {
		if project == "my-stack" {
			return &StackMatch{StackID: 7, IsGit: true}
		}
		return nil
	}

	tests := []struct {
		name   string
		labels map[string]string
		want   UpdateRouting
	}{
		{
			name:   "no compose label -> standalone",
			labels: map[string]string{"foo": "bar"},
			want:   UpdateRouting{Kind: UpdateStandalone},
		},
		{
			name:   "empty compose label -> standalone",
			labels: map[string]string{composeProjectLabel: ""},
			want:   UpdateRouting{Kind: UpdateStandalone},
		},
		{
			name:   "compose project matching a portainer compose stack -> stack",
			labels: map[string]string{composeProjectLabel: "my-stack"},
			want:   UpdateRouting{Kind: UpdateStack, StackID: 7, IsGit: true},
		},
		{
			name:   "compose project with no matching stack -> external",
			labels: map[string]string{composeProjectLabel: "other"},
			want:   UpdateRouting{Kind: UpdateExternal},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveContainerUpdateRouting(tt.labels, stackLookup)
			if got != tt.want {
				t.Errorf("resolveContainerUpdateRouting(%v) = %+v, want %+v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestGroupContainersForUpdate(t *testing.T) {
	// stackLookup: "web" -> compose stack 3 (file), "api" -> compose stack 4 (git).
	stackLookup := func(project string) *StackMatch {
		switch project {
		case "web":
			return &StackMatch{StackID: 3, IsGit: false}
		case "api":
			return &StackMatch{StackID: 4, IsGit: true}
		default:
			return nil
		}
	}

	candidates := []UpdateCandidate{
		{ID: "standalone-1"},
		{ID: "web-a", Name: "web-a", Labels: map[string]string{composeProjectLabel: "web"}},
		{ID: "web-b", Name: "web-b", Labels: map[string]string{composeProjectLabel: "web"}}, // same stack -> deduped redeploy, both kept as members
		{ID: "api-a", Name: "api-a", Labels: map[string]string{composeProjectLabel: "api"}},
		{ID: "ext-1", Labels: map[string]string{composeProjectLabel: "unknown"}},
	}

	grouped := groupContainersForUpdate(candidates, stackLookup)

	if len(grouped.Standalone) != 1 || grouped.Standalone[0].ID != "standalone-1" {
		t.Errorf("Standalone = %+v, want one entry standalone-1", grouped.Standalone)
	}

	if len(grouped.External) != 1 || grouped.External[0].ID != "ext-1" {
		t.Errorf("External = %+v, want one entry ext-1", grouped.External)
	}

	// One redeploy per stack: web appears twice in input but once in output.
	if len(grouped.Stacks) != 2 {
		t.Fatalf("Stacks = %+v, want 2 deduped stacks", grouped.Stacks)
	}

	got := map[int]bool{}
	for _, st := range grouped.Stacks {
		got[st.StackID] = st.IsGit
	}

	if isGit, ok := got[3]; !ok || isGit {
		t.Errorf("stack 3 = (%v, present=%v), want present file stack", isGit, ok)
	}

	if isGit, ok := got[4]; !ok || !isGit {
		t.Errorf("stack 4 = (%v, present=%v), want present git stack", isGit, ok)
	}

	// The stack is redeployed once, but every member container is threaded through
	// (not discarded at the collapse) so each can emit its own notification.
	members := map[int][]string{}
	for _, st := range grouped.Stacks {
		for _, c := range st.Containers {
			members[st.StackID] = append(members[st.StackID], c.Name)
		}
	}

	if got := members[3]; len(got) != 2 || got[0] != "web-a" || got[1] != "web-b" {
		t.Errorf("stack 3 members = %v, want [web-a web-b]", got)
	}

	if got := members[4]; len(got) != 1 || got[0] != "api-a" {
		t.Errorf("stack 4 members = %v, want [api-a]", got)
	}
}
