//go:build integration

// This file runs the client against a REAL secret_resolver process instead of against a
// handwritten server, and it is behind a build tag so that `go test ./...` never needs one.
// Everything else in this package's tests states what the client does with a server this
// repository wrote; nothing until now stated that the other implementation of the protocol
// agrees. The two are developed in separate repositories, so "both sides pass their own
// tests" is exactly the condition under which a wire mismatch ships unnoticed.
//
// ci/secretresolver-integration/run.sh is what brings the other side up (a resolver on a unix
// socket, backed by a fake `rbw`) and sets the endpoint this file reads. Without it the test
// skips.

package secretresolver

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// integrationEndpointEnvVar carries the endpoint of a resolver that is actually listening.
// It is deliberately NOT EndpointEnvVar: the production variable is what the subtests set
// through t.Setenv, and reading it here as well would make an endpoint left in the
// environment silently change what the unit tests in this package see.
const integrationEndpointEnvVar = "SECRET_RESOLVER_INTEGRATION_ENDPOINT"

// The vault entries the fake `rbw` serves, and the values it prints for them.
//
// THE SAME LITERALS LIVE IN ci/secretresolver-integration/inside.sh, which writes that fake.
// Neither file can import the other - one is Go, one is a shell heredoc holding Python - so
// the pairing is kept by hand: change a value here and change it there in the same commit,
// or this test fails with a mismatch rather than with a protocol problem.
const (
	alphaRef   = "secret:vw:ci/alpha"
	betaRef    = "secret:vw:ci/beta"
	awkwardRef = "secret:vw:ci/awkward"
	// unknownRef names no entry at all, so the fake exits non-zero for it and the resolver
	// answers 502.
	unknownRef = "secret:vw:ci/missing"

	alphaValue = "s3cr3t-alpha-9f2a"
	betaValue  = "s3cr3t-beta-4d71"
	// A double quote, a newline and two non-ASCII scripts, because those are the three ways a
	// value gets mangled between a CLI's stdout, a JSON body and a Go string: the quote breaks
	// a body that is concatenated rather than encoded, the newline is what `rbw`'s trailing
	// line ending is stripped by, and the non-ASCII proves the bytes survive both sides'
	// encoding choices (the resolver decodes rbw's stdout as UTF-8 and json.dumps escapes it
	// back to \uXXXX).
	awkwardValue = "s3cr3t-\"gamma\"\nпароль-ß"
)

func TestIntegrationAgainstResolverService(t *testing.T) {
	endpoint := os.Getenv(integrationEndpointEnvVar)
	if endpoint == "" {
		t.Skipf("%s is unset, so there is no resolver to talk to; run ci/secretresolver-integration/run.sh", integrationEndpointEnvVar)
	}

	// Through FromEnv rather than New, because FromEnv is what the server actually calls and
	// it is where the endpoint is parsed, the timeout is defaulted and the transport is
	// chosen. A test that built the client directly would prove the protocol and leave the
	// production construction path untested against a real listener.
	t.Setenv(EndpointEnvVar, endpoint)

	client := FromEnv()
	require.NotNil(t, client, "FromEnv returned no client for %s=%s", EndpointEnvVar, endpoint)

	t.Run("happy path", func(t *testing.T) {
		// alphaRef twice: one vault entry referenced from two variables is the ordinary case,
		// and the client deduplicates before it sends, so the answer is keyed by the unique
		// references and not by the ones asked for.
		values, err := client.Fetch(context.Background(), []string{alphaRef, betaRef, alphaRef})
		require.NoError(t, err)
		require.Equal(t, map[string]string{alphaRef: alphaValue, betaRef: betaValue}, values)
	})

	t.Run("awkward value", func(t *testing.T) {
		values, err := client.Fetch(context.Background(), []string{awkwardRef})
		require.NoError(t, err)
		require.Equal(t, map[string]string{awkwardRef: awkwardValue}, values)
	})

	t.Run("unknown entry", func(t *testing.T) {
		_, err := client.Fetch(context.Background(), []string{unknownRef})
		require.Error(t, err)

		// The failure text is persisted as the stack's deployment status message and served
		// back over Portainer's API, so it must carry no value from this vault - not the one
		// that was asked for and not one resolved a moment earlier by another subtest.
		for name, secret := range map[string]string{
			"alpha":   alphaValue,
			"beta":    betaValue,
			"awkward": awkwardValue,
		} {
			require.NotContains(t, err.Error(), secret, "the error text leaked the %s value", name)
		}
	})
}
