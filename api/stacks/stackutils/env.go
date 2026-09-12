package stackutils

import (
	"maps"
	"os"
	"path"
	"strings"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/filesystem"
	"github.com/portainer/portainer/pkg/secretresolver"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// BuildEnvMap builds the environment variable map for stack validation/loading.
// Priority (lowest to highest): OS env, .env file, stack.Env
//
// The stored values are unescaped on the way in, because what this map feeds -
// IsValidStackFile and checkComposeFileURLs, both of which run at deploy time - has to
// interpolate the same strings the deploy hands to compose. A value stored as
// "secret::/host/path" reaches the container as "secret:/host/path", and validating the
// escaped form would check a different compose file than the one that actually starts:
// the extra colon moves a volume's short syntax onto a different branch of compose's
// parser, and it changes the string the SSRF policy probes. See secretresolver.Unescape.
func BuildEnvMap(stack *portainer.Stack) map[string]string {
	env := make(map[string]string, len(os.Environ()))
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}

	dotEnvPath := filesystem.JoinPaths(stack.ProjectPath, path.Dir(stack.EntryPoint), ".env")
	if dotVars, err := dotenv.Read(dotEnvPath); err == nil {
		maps.Copy(env, dotVars)
	}

	for _, pair := range stack.Env {
		env[pair.Name] = secretresolver.Unescape(pair.Value)
	}

	return env
}
