package stackutils

import (
	"testing"

	portainer "github.com/portainer/portainer/api"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildEnvMap_unescapesTheSecretMarker(t *testing.T) {
	t.Parallel()

	stack := &portainer.Stack{Env: []portainer.Pair{
		{Name: "ESCAPED", Value: "secret::/data"},
		{Name: "PLAIN", Value: "hunter2"},
		{Name: "REFERENCE", Value: "secret:vw:stack/app/TOKEN"},
	}}

	env := BuildEnvMap(stack)

	// The value the deploy hands to compose, which is what validation has to interpolate.
	assert.Equal(t, "secret:/data", env["ESCAPED"])
	assert.Equal(t, "hunter2", env["PLAIN"])

	// A reference is not an escape and is passed through unchanged: the deploy replaces it
	// with a resolved value that validation deliberately never sees, which is the separate
	// divergence recorded in docs/secret-resolver.md §3.11.
	assert.Equal(t, "secret:vw:stack/app/TOKEN", env["REFERENCE"])
}

// TestIsValidStackFile_seesTheDeployedFormOfAnEscapedValue is why BuildEnvMap unescapes.
//
// The variable sits where its own colon is a separator, so the escape does not merely
// change the value: it changes how compose parses the line around it. The two subtests are
// the two directions that divergence can take, and both were reproduced against
// compose-go v2 before being pinned here.
func TestIsValidStackFile_seesTheDeployedFormOfAnEscapedValue(t *testing.T) {
	t.Parallel()

	securitySettings := &portainer.EndpointSecuritySettings{}

	t.Run("a policy check runs against the deployed string", func(t *testing.T) {
		t.Parallel()

		// Deployed, "secret:/data" splits the short syntax into the bind source
		// "/host/path" and the target "secret" - exactly what the bind-mount policy exists
		// to catch. Validated in its stored form the extra colon makes the whole spec
		// unparsable instead ("empty section between colons"), so the check that fires is a
		// syntax error about a line the operator did not write.
		content := []byte("services:\n  app:\n    image: nginx\n    volumes:\n      - \"/host/path:${DST}\"\n")

		stack := &portainer.Stack{Env: []portainer.Pair{{Name: "DST", Value: "secret::/data"}}}

		err := IsValidStackFile(StackFileValidationConfig{
			Content:          content,
			SecuritySettings: securitySettings,
			Env:              BuildEnvMap(stack),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bind-mount disabled")
	})

	t.Run("a stack that deploys is not rejected for its stored form", func(t *testing.T) {
		t.Parallel()

		// The mirror image, and the reason the escape exists at all: deployed,
		// "secret:/host/path" is an ordinary named volume and the stack is valid. Validated
		// in its stored form it is unparsable, so before BuildEnvMap unescaped, a stack that
		// deploys perfectly well could not be saved.
		content := []byte("services:\n  app:\n    image: nginx\n    volumes:\n      - \"${SRC}:/data\"\nvolumes:\n  secret: {}\n")

		stack := &portainer.Stack{Env: []portainer.Pair{{Name: "SRC", Value: "secret::/host/path"}}}

		require.NoError(t, IsValidStackFile(StackFileValidationConfig{
			Content:          content,
			SecuritySettings: securitySettings,
			Env:              BuildEnvMap(stack),
		}))
	})
}
