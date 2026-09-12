package secretresolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// placeholders builds the names RewriteCompose is given for a body with count references,
// the way the deploy path numbers them.
func placeholders(count int) []string {
	names := make([]string, 0, count)

	for i := range count {
		names = append(names, fmt.Sprintf("%s%d", ComposePlaceholderPrefix, i))
	}

	return names
}

// Test_ComposeReferences_and_RewriteCompose drives the two together, because they are one
// mechanism: what the scan finds is what the splice has to replace, at the same positions,
// and a case that pins only one half would let the two drift apart.
//
// Every case states the whole rewritten body rather than the changed line, which is the
// point of the splice: a comment, an indentation, a quoting style or a trailing space the
// rewrite touched would fail the comparison.
func Test_ComposeReferences_and_RewriteCompose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   string
		refs      []string
		rewritten string
	}{
		{
			name: "a reference as a mapping value",
			content: `services:
  app:
    image: nginx
    environment:
      METRICS_TOKEN: secret:vw:stack/nebula.lc/tabscurator/METRICS_TOKEN
`,
			refs: []string{"secret:vw:stack/nebula.lc/tabscurator/METRICS_TOKEN"},
			rewritten: `services:
  app:
    image: nginx
    environment:
      METRICS_TOKEN: "${__PORTAINER_SECRET_0}"
`,
		},
		{
			name: "a reference in the NAME=value list form keeps its name",
			content: `services:
  app:
    environment:
      - METRICS_TOKEN=secret:vw:one
      - LOG_LEVEL=debug
`,
			refs: []string{"secret:vw:one"},
			rewritten: `services:
  app:
    environment:
      - "METRICS_TOKEN=${__PORTAINER_SECRET_0}"
      - LOG_LEVEL=debug
`,
		},
		{
			name: "quoted references, both styles, and the quotes go with them",
			content: `single: 'secret:vw:one'
double: "secret:vw:two"
`,
			refs: []string{"secret:vw:one", "secret:vw:two"},
			rewritten: `single: "${__PORTAINER_SECRET_0}"
double: "${__PORTAINER_SECRET_1}"
`,
		},
		{
			name: "a reference inside a flow sequence",
			content: `services:
  app:
    command: ["--token", "secret:vw:one", "--verbose"]
`,
			refs: []string{"secret:vw:one"},
			rewritten: `services:
  app:
    command: ["--token", "${__PORTAINER_SECRET_0}", "--verbose"]
`,
		},
		{
			name: "a trailing comment survives the scalar it follows",
			content: `token: secret:vw:one # the admin token
other: 1
`,
			refs: []string{"secret:vw:one"},
			rewritten: `token: "${__PORTAINER_SECRET_0}" # the admin token
other: 1
`,
		},
		{
			name: "non-ASCII earlier on the line does not move the splice",
			content: `services:
  app:
    labels: {описание: "значение", token: secret:vw:one}
    # комментарий
    описание: secret:vw:two
`,
			refs: []string{"secret:vw:one", "secret:vw:two"},
			rewritten: `services:
  app:
    labels: {описание: "значение", token: "${__PORTAINER_SECRET_0}"}
    # комментарий
    описание: "${__PORTAINER_SECRET_1}"
`,
		},
		{
			name: "the same reference twice gets a placeholder each",
			content: `a: secret:vw:one
b: secret:vw:one
`,
			refs: []string{"secret:vw:one", "secret:vw:one"},
			rewritten: `a: "${__PORTAINER_SECRET_0}"
b: "${__PORTAINER_SECRET_1}"
`,
		},
		{
			name: "every document of a multi-document file is scanned",
			content: `a: secret:vw:one
---
b: secret:vw:two
`,
			refs: []string{"secret:vw:one", "secret:vw:two"},
			rewritten: `a: "${__PORTAINER_SECRET_0}"
---
b: "${__PORTAINER_SECRET_1}"
`,
		},
		{
			name: "a mapping key is never a reference",
			content: `labels:
  secret:key: a value
`,
			refs: nil,
			rewritten: `labels:
  secret:key: a value
`,
		},
		{
			name: "an escaped marker is unescaped and allocates no placeholder",
			content: `command: secret::literal
token: secret:vw:one
`,
			refs: []string{"secret:vw:one"},
			rewritten: `command: "secret:literal"
token: "${__PORTAINER_SECRET_0}"
`,
		},
		{
			name: "an escaped marker alone still rewrites the body",
			content: `command: 'secret::literal'
`,
			refs: nil,
			rewritten: `command: "secret:literal"
`,
		},
		{
			// The list form of the same variable, which classifyComposeScalar accepts for a
			// reference and therefore has to accept for the escape: otherwise the two lines
			// below would reach the container with values one colon apart.
			name: "an escaped marker after NAME= is unescaped like the mapping form",
			content: `environment:
  - FOO=secret::literal
  - BAR=secret:vw:one
`,
			refs: []string{"secret:vw:one"},
			rewritten: `environment:
  - "FOO=secret:literal"
  - "BAR=${__PORTAINER_SECRET_0}"
`,
		},
		{
			name: "a body without the marker is not touched at all",
			content: `services:
  app:
    image: nginx
    command: ["--secret", "s3cr3t"]
`,
			refs: nil,
			rewritten: `services:
  app:
    image: nginx
    command: ["--secret", "s3cr3t"]
`,
		},
		{
			name:      "the marker inside a value is not a reference",
			content:   "note: this is not a secret:reference, it is prose\n",
			refs:      nil,
			rewritten: "note: this is not a secret:reference, it is prose\n",
		},
		{
			name:      "an empty body",
			content:   "",
			refs:      nil,
			rewritten: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			refs, err := ComposeReferences([]byte(test.content))
			require.NoError(t, err)
			assert.Equal(t, test.refs, nilIfEmpty(refs))

			rewritten, err := RewriteCompose([]byte(test.content), placeholders(len(test.refs)))
			require.NoError(t, err)
			assert.Equal(t, test.rewritten, string(rewritten))
		})
	}
}

// nilIfEmpty keeps a case's expectation readable: a body with no reference gives an empty
// slice rather than a nil one, and the difference is not what these cases are about.
func nilIfEmpty(refs []string) []string {
	if len(refs) == 0 {
		return nil
	}

	return refs
}

// Test_ComposeReferences_refusals pins the bodies neither function may pass, and pins them
// on BOTH: a scalar whose extent cannot be established exactly has to stop the paths that
// only refuse references - swarm, edge, the non-administrator gate - as well as the deploy
// path that rewrites the body, and it has to stop them before a reference is sent to the
// resolver.
func Test_ComposeReferences_refusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		message string
	}{
		{
			name: "a body that already spells a placeholder",
			content: `a: "${__PORTAINER_SECRET_0}"
b: secret:vw:one
`,
			message: "__PORTAINER_SECRET_",
		},
		{
			// The placeholders are numbered across the whole stack and injected through one
			// Options.Env shared by every file of the project, so this file needs no reference
			// of its own to read a neighbouring file's resolved value. Refused on the
			// placeholder alone, before the marker is looked for at all.
			name: "a body that spells a placeholder and carries no reference",
			content: `a: "${__PORTAINER_SECRET_0}"
b: a plain value
`,
			message: "__PORTAINER_SECRET_",
		},
		{
			name: "a block scalar",
			content: `command: |
  secret:vw:one
`,
			message: `scalar at 1:10 begins with the "secret:" marker but is a block or folded scalar`,
		},
		{
			name: "a folded scalar",
			content: `command: >
  secret:vw:one
`,
			message: `scalar at 1:10 begins with the "secret:" marker but is a block or folded scalar`,
		},
		{
			name: "a tagged quoted scalar",
			content: `command: !!str "secret:vw:one"
`,
			message: `scalar at 1:10 begins with the "secret:" marker but is a block or folded scalar`,
		},
		{
			name: "a tagged plain scalar",
			content: `command: !!str secret:vw:one
`,
			message: `scalar at 1:10 begins with the "secret:" marker but does not match its text in the file`,
		},
		{
			name: "an anchored scalar",
			content: `command: &token secret:vw:one
other: *token
`,
			message: `scalar at 1:10 begins with the "secret:" marker but does not match its text in the file`,
		},
		{
			name: "a plain scalar spanning two lines",
			content: `command: secret:vw:one
  continued
`,
			message: `scalar at 1:10 begins with the "secret:" marker but does not match its text in the file`,
		},
		{
			name:    "a body that is not YAML at all",
			content: "command: [secret:vw:one\n",
			message: "failed to parse the compose file",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ComposeReferences([]byte(test.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)

			_, err = RewriteCompose([]byte(test.content), nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}

// Test_RewriteCompose_nameCountMismatch pins the one thing the caller can get wrong: names
// built from another body would splice a placeholder over one reference and leave the next
// one in the deployed file as its own text.
func Test_RewriteCompose_nameCountMismatch(t *testing.T) {
	t.Parallel()

	content := []byte("a: secret:vw:one\nb: secret:vw:two\n")

	_, err := RewriteCompose(content, placeholders(1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carries 2 secret references but 1 placeholder names were given")
}

// Test_RewriteCompose_returnsTheBodyItWasGiven pins what tells the deploy path that a file
// needs no rewritten copy: a body with nothing to rewrite comes back as it stands.
func Test_RewriteCompose_returnsTheBodyItWasGiven(t *testing.T) {
	t.Parallel()

	content := []byte("services:\n  app:\n    image: nginx\n")

	rewritten, err := RewriteCompose(content, nil)
	require.NoError(t, err)
	assert.Equal(t, string(content), string(rewritten))
}

// Test_RewriteCompose_escapesWhatItWrites pins the quoting of the one scalar whose text is
// not this package's own: an unescaped literal goes back into the body as a double-quoted
// scalar that parses to itself, whatever is in it.
func Test_RewriteCompose_escapesWhatItWrites(t *testing.T) {
	t.Parallel()

	content := []byte("command: \"secret::a\\tb\\\"c\\\\d\"\n")

	rewritten, err := RewriteCompose(content, nil)
	require.NoError(t, err)
	assert.Equal(t, "command: \"secret:a\\tb\\\"c\\\\d\"\n", string(rewritten))
}

// Test_ComposeReferences_ignoresWhatIsNotAReference keeps the NAME= form from widening into
// every string that happens to hold the marker somewhere.
func Test_ComposeReferences_ignoresWhatIsNotAReference(t *testing.T) {
	t.Parallel()

	content := []byte(strings.Join([]string{
		"a: PATH=/usr/bin:secret:vw:one",
		"b: 9secret=secret:vw:two",
		"c: -- secret:vw:three",
		"",
	}, "\n"))

	refs, err := ComposeReferences(content)
	require.NoError(t, err)
	assert.Empty(t, refs)
}
