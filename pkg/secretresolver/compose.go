package secretresolver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// This file finds secret references written inline in a compose body, where the stack's
// Env field cannot reach them.
//
// compose interpolates ${VAR} and nothing else, so a reference sitting in the body as its
// own text - environment: {TOKEN: secret:vw:...} - reaches the container verbatim however
// much is injected through libstack.Options.Env. The only way to resolve one is to rewrite
// the body: every reference scalar becomes "${__PORTAINER_SECRET_<n>}" and the value is
// injected under that name, so the value still never touches disk - what is written out
// carries a placeholder, exactly as a hand-written ${VAR} body would.
//
// The rewrite is a byte splice over the original text rather than a marshal of the parsed
// document. A compose file carries comments, key order, indentation and quoting style that
// a round trip through a YAML marshaller silently rewrites, and a deploy that reformats the
// operator's file is a deploy that cannot be diffed against what the operator wrote.
// Everything outside a reference scalar is therefore byte-identical, and anything the splice
// cannot place exactly - a block scalar, a multi-line, tagged or anchored one - is an error
// naming line and column rather than a reference quietly left as it stands.

// ComposePlaceholderPrefix begins the name of every compose variable a rewritten body
// interpolates in place of a reference. The caller appends an index to it, one per
// reference, and injects "<name>=<value>" through libstack.Options.Env.
//
// The leading underscores are load bearing: libstack.PortainerEnvVars sweeps every
// PORTAINER_-prefixed variable of the server process into the compose environment of every
// project, and a placeholder caught by that sweep would be a resolved value handed to every
// other stack on the environment. "__PORTAINER_SECRET_" is not "PORTAINER_SECRET_", so it
// is not swept.
const ComposePlaceholderPrefix = "__PORTAINER_SECRET_"

// composeAssignmentPrefix matches the "NAME=" of a list-form entry, the shape
// "environment:", "env_file:" entries and command arguments are written in when they are a
// sequence rather than a mapping. The reference is then the assignment's value and the name
// has to survive the rewrite verbatim.
var composeAssignmentPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// composeHit is one scalar of a compose body that the rewrite has to touch.
type composeHit struct {
	node *yaml.Node

	// start and end bound the scalar's token in the body, quotes included. They are
	// established by the scan and not by the rewrite, so that a scalar whose extent cannot
	// be established exactly is refused before a single reference is sent to the resolver -
	// and refused by ComposeReferences, which is what the paths that only refuse references
	// call.
	start int
	end   int

	// prefix is the "NAME=" of a list-form entry, kept verbatim, and empty when the whole
	// scalar is the reference.
	prefix string

	// ref is the reference as written, which is what goes to the resolver.
	ref string

	// unescapeOnly marks a scalar that carries no reference at all, only the escaped
	// marker: the rewrite turns "secret::x" back into the literal "secret:x" and allocates
	// no placeholder for it. Without this the escape would mean one thing in a stack's Env,
	// where unescapeLiterals applies it, and another in the body next to it.
	unescapeOnly bool
}

// ComposeReferences returns every secret reference written inline in a compose body, in
// document order and with repeats kept: the caller numbers the placeholders by position, so
// the i-th reference here is the i-th name RewriteCompose is given.
//
// A scalar in the escaped form "secret::x" is not a reference and is not returned; it is
// still a scalar RewriteCompose rewrites.
func ComposeReferences(content []byte) ([]string, error) {
	hits, err := scanComposeBody(content)
	if err != nil {
		return nil, err
	}

	refs := make([]string, 0, len(hits))

	for _, hit := range hits {
		if hit.unescapeOnly {
			continue
		}

		refs = append(refs, hit.ref)
	}

	return refs, nil
}

// RewriteCompose returns the body with its i-th inline reference replaced by the double
// quoted scalar "${names[i]}" and every escaped marker turned back into the literal it
// stands for. names must hold exactly one name per reference ComposeReferences found in the
// same content.
//
// The body is returned as it stands - the same slice, not a copy - when there is nothing to
// rewrite, which is every compose file of every stack that has not migrated. That is what
// tells the caller a file needs no rewritten copy at all.
func RewriteCompose(content []byte, names []string) ([]byte, error) {
	hits, err := scanComposeBody(content)
	if err != nil {
		return nil, err
	}

	if len(hits) == 0 {
		return content, nil
	}

	references := 0

	for _, hit := range hits {
		if !hit.unescapeOnly {
			references++
		}
	}

	// Checked before the splice rather than while indexing into names: the two scans are the
	// same code over the same bytes, so a mismatch means the caller built its names from
	// another body, and splicing half of them in would leave the rest of the references in
	// the deployed file as their own text.
	if len(names) != references {
		return nil, fmt.Errorf("compose file carries %d secret references but %d placeholder names were given", references, len(names))
	}

	var out bytes.Buffer

	spliced := 0
	index := 0

	for _, hit := range hits {
		var replacement string

		if hit.unescapeOnly {
			// The scan only records a hit when this reports true, so the value is discarded
			// here for the same reason it is recomputed: the hit carries the node, not the
			// text, and the two scans are the same code over the same bytes.
			unescaped, _ := composeUnescapeScalar(hit.node.Value)
			replacement = quoteComposeScalar(unescaped)
		} else {
			replacement = quoteComposeScalar(hit.prefix + "${" + names[index] + "}")
			index++
		}

		out.Write(content[spliced:hit.start])
		out.WriteString(replacement)

		spliced = hit.end
	}

	out.Write(content[spliced:])

	return out.Bytes(), nil
}

// scanComposeBody parses the body and returns every scalar the rewrite has to touch, in
// document order.
func scanComposeBody(content []byte) ([]composeHit, error) {
	// The placeholders the rewrite writes are interpolated by compose against the values
	// injected for them, so a body that already spells one could name a variable this file
	// is about to define - reading another reference's resolved value, or shadowing the
	// placeholder of its own. Refused rather than resolved either way round.
	//
	// Checked before the marker below and not after it, because the placeholders are numbered
	// across the WHOLE stack and injected through one libstack.Options.Env shared by every
	// file of the project: a file that carries no reference of its own still interpolates
	// against that same environment, so "${__PORTAINER_SECRET_0}" written into such a file
	// would read the resolved value of whichever reference happened to be numbered 0 in a
	// neighbouring file. Returning early on the marker would leave exactly that file unchecked.
	if bytes.Contains(content, []byte(ComposePlaceholderPrefix)) {
		return nil, fmt.Errorf("compose file contains %s, which is reserved for the placeholders an inline secret reference is rewritten to", ComposePlaceholderPrefix)
	}

	// Neither a reference nor the escaped form can exist without the marker, so a body
	// without it is not parsed at all. That is the path every stack that has not migrated
	// takes, on every deploy, and it costs two substring searches.
	if !bytes.Contains(content, []byte(ReferencePrefix)) {
		return nil, nil
	}

	var hits []composeHit

	// Every document, not only the first: a compose file may hold several, and a reference
	// in the second one is as live as a reference in the first.
	decoder := yaml.NewDecoder(bytes.NewReader(content))

	for {
		var document yaml.Node

		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("failed to parse the compose file: %w", err)
		}

		walkComposeNode(&document, &hits)
	}

	if len(hits) == 0 {
		return nil, nil
	}

	// The extent of every hit is established here, in document order, so that the first
	// scalar the splice could not place exactly stops the whole body rather than the one
	// before it being rewritten.
	lineStarts := composeLineStarts(content)

	for i := range hits {
		start, err := composeByteOffset(content, lineStarts, hits[i].node.Line, hits[i].node.Column)
		if err != nil {
			return nil, err
		}

		end, err := composeTokenEnd(content, start, hits[i].node)
		if err != nil {
			return nil, err
		}

		hits[i].start, hits[i].end = start, end
	}

	return hits, nil
}

// walkComposeNode collects the scalars of one document that carry a reference or the
// escaped marker.
func walkComposeNode(node *yaml.Node, hits *[]composeHit) {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			walkComposeNode(child, hits)
		}

	case yaml.MappingNode:
		// Values only. A key is a compose keyword, a service, a variable or a volume name -
		// never a place a secret is written - and a key rewritten to "${...}" would change
		// the shape of the document rather than a value in it.
		for i := 1; i < len(node.Content); i += 2 {
			walkComposeNode(node.Content[i], hits)
		}

	case yaml.ScalarNode:
		if prefix, ref, ok := classifyComposeScalar(node.Value); ok {
			*hits = append(*hits, composeHit{node: node, prefix: prefix, ref: ref})

			return
		}

		if _, escaped := composeUnescapeScalar(node.Value); escaped {
			*hits = append(*hits, composeHit{node: node, unescapeOnly: true})
		}
	}

	// An alias node holds the anchor's name and not the anchored value, so there is nothing
	// in it to rewrite: the scalar it points at is visited where it is defined, and an
	// anchored scalar carrying a reference is refused by composeTokenEnd.
}

// classifyComposeScalar reports whether a scalar carries a reference - either as the whole
// of its value, or after the "NAME=" of a list-form entry - and splits it into the part kept
// verbatim and the reference itself.
func classifyComposeScalar(value string) (prefix, ref string, ok bool) {
	if IsReference(value) {
		return "", value, true
	}

	if assignment := composeAssignmentPrefix.FindString(value); assignment != "" && IsReference(value[len(assignment):]) {
		return assignment, value[len(assignment):], true
	}

	return "", "", false
}

// composeUnescapeScalar turns an escaped marker back into the literal it stands for and
// reports whether there was one, in both shapes a value can be written in: the whole scalar,
// and the value of a "NAME=" list-form entry.
//
// Both shapes and not just the first, because classifyComposeScalar accepts both for a
// reference: without this, "FOO: secret::x" would deploy the literal "secret:x" while
// "- FOO=secret::x" - the same variable, written as a sequence entry rather than a mapping -
// would deploy "secret::x", and the escape would mean two different things two lines apart.
func composeUnescapeScalar(value string) (string, bool) {
	if unescaped := Unescape(value); unescaped != value {
		return unescaped, true
	}

	assignment := composeAssignmentPrefix.FindString(value)
	if assignment == "" {
		return "", false
	}

	if unescaped := Unescape(value[len(assignment):]); unescaped != value[len(assignment):] {
		return assignment + unescaped, true
	}

	return "", false
}

// composeLineStarts returns the offset of the first byte of every line, so that a node's
// 1-based line number is one lookup rather than a scan from the top of the file.
func composeLineStarts(content []byte) []int {
	starts := []int{0}

	for i, b := range content {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}

	return starts
}

// composeByteOffset converts a node's 1-based line and column into an offset into content.
//
// The column is not an offset and adding it is the bug this function exists to avoid: yaml
// counts a column in RUNES, so a comment, a label or a container name in non-ASCII text
// earlier on the line moves the byte position away from the column by one byte for every
// extra byte those runes take. A compose file is UTF-8 and a service description in Russian
// or an emoji in a label is ordinary, so the difference is not hypothetical.
func composeByteOffset(content []byte, lineStarts []int, line, column int) (int, error) {
	if line < 1 || line > len(lineStarts) {
		return 0, fmt.Errorf("scalar at %d:%d is on a line that is not in the file", line, column)
	}

	offset := lineStarts[line-1]

	for i := 1; i < column; i++ {
		if offset >= len(content) || content[offset] == '\n' {
			return 0, fmt.Errorf("scalar at %d:%d is past the end of its line", line, column)
		}

		_, size := utf8.DecodeRune(content[offset:])
		offset += size
	}

	return offset, nil
}

// composeTokenEnd returns the offset just past the scalar token that starts at start, which
// is the other end of the span the rewrite replaces.
//
// The extent is measured from the file rather than from the node, because the node holds the
// PARSED value and the file holds the token: the two differ by the quotes, by every escape
// inside them, and by a line break a folded scalar turned into a space. Every style the
// extent cannot be established for exactly is an error naming line and column - never a
// reference silently left in the deployed file as its own text.
//
// The messages say the scalar BEGINS WITH the marker rather than that it is a reference, and
// the difference is not pedantry: a block scalar carrying an embedded config file whose first
// line happens to be "secret: something" arrives here too, and telling its author that their
// config file is a malformed secret reference would send them looking in the wrong place.
// Both ways out are named for the same reason - a reference and a literal written with the
// escape are equally impossible to write as a block scalar, so an author who reads only half
// the message would try the escape and fail again.
func composeTokenEnd(content []byte, start int, node *yaml.Node) (int, error) {
	switch node.Style {
	case 0, yaml.TaggedStyle:
		// A plain scalar is its own text, so the value has to be there verbatim. It is not
		// when the scalar spans lines - the parser folds the break into a space - and it is
		// not when a tag or an anchor precedes it, because the node's position is then the
		// position of the tag or of the anchor rather than of the value.
		if !bytes.HasPrefix(content[start:], []byte(node.Value)) {
			return 0, fmt.Errorf("scalar at %d:%d begins with the %q marker but does not match its text in the file, so it is a multi-line, tagged or anchored scalar: a secret reference, and a literal written with the %q escape, must each be a plain or single-line quoted scalar", node.Line, node.Column, ReferencePrefix, escapePrefix)
		}

		return start + len(node.Value), nil

	case yaml.SingleQuotedStyle:
		if start >= len(content) || content[start] != '\'' {
			return 0, fmt.Errorf("scalar at %d:%d begins with the %q marker but does not start where it is reported to, so it is a tagged or anchored scalar: a secret reference, and a literal written with the %q escape, must each be a plain or single-line quoted scalar", node.Line, node.Column, ReferencePrefix, escapePrefix)
		}

		for i := start + 1; i < len(content); i++ {
			if content[i] == '\n' {
				return 0, fmt.Errorf("single-quoted scalar at %d:%d spans lines, a secret reference must be a single-line scalar", node.Line, node.Column)
			}

			if content[i] == '\'' {
				// A doubled quote is the escape for one quote and not the end of the scalar.
				if i+1 < len(content) && content[i+1] == '\'' {
					i++

					continue
				}

				return i + 1, nil
			}
		}

		return 0, fmt.Errorf("single-quoted scalar at %d:%d is never closed", node.Line, node.Column)

	case yaml.DoubleQuotedStyle:
		if start >= len(content) || content[start] != '"' {
			return 0, fmt.Errorf("scalar at %d:%d begins with the %q marker but does not start where it is reported to, so it is a tagged or anchored scalar: a secret reference, and a literal written with the %q escape, must each be a plain or single-line quoted scalar", node.Line, node.Column, ReferencePrefix, escapePrefix)
		}

		for i := start + 1; i < len(content); i++ {
			if content[i] == '\n' {
				return 0, fmt.Errorf("double-quoted scalar at %d:%d spans lines, a secret reference must be a single-line scalar", node.Line, node.Column)
			}

			if content[i] == '\\' {
				i++

				continue
			}

			if content[i] == '"' {
				return i + 1, nil
			}
		}

		return 0, fmt.Errorf("double-quoted scalar at %d:%d is never closed", node.Line, node.Column)

	default:
		return 0, fmt.Errorf("scalar at %d:%d begins with the %q marker but is a block or folded scalar: a secret reference, and a literal written with the %q escape, must each be a plain or single-line quoted scalar", node.Line, node.Column, ReferencePrefix, escapePrefix)
	}
}

// quoteComposeScalar renders a string as a double-quoted YAML scalar.
//
// strconv.Quote does the escaping, and every escape it emits - \\, \", \a, \b, \f, \n, \r,
// \t, \v, \xNN, \uNNNN and \UNNNNNNNN - is one YAML's double-quoted style defines with the
// same meaning, so what is written parses back to the string that was handed in.
//
// Double-quoted and never plain: the text is either "${...}" or a literal that begins with
// "secret:", and both start with a character a plain scalar in a compose file cannot be
// trusted to carry unchanged.
func quoteComposeScalar(value string) string {
	return strconv.Quote(value)
}
