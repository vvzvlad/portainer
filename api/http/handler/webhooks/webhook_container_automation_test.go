package webhooks

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/portainer/portainer/api/datastore"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// fakeTrigger records how many times the inbound webhook asked for an update pass.
type fakeTrigger struct{ calls int }

func (f *fakeTrigger) TriggerUpdate() { f.calls++ }

// setupWebhookAutomationHandler builds a webhooks.Handler backed by a test store
// whose auto-update settings carry the given token and enabled flag, plus a fake
// trigger to observe the kick.
func setupWebhookAutomationHandler(t *testing.T, token string, enabled bool) (*Handler, *fakeTrigger) {
	t.Helper()

	_, store := datastore.MustNewTestStore(t, true, false)

	settings, err := store.Settings().Settings()
	require.NoError(t, err)
	settings.ContainerAutomation.AutoUpdate.WebhookToken = token
	settings.ContainerAutomation.AutoUpdate.Enabled = enabled
	require.NoError(t, store.Settings().UpdateSettings(settings))

	trigger := &fakeTrigger{}
	handler := &Handler{DataStore: store, ContainerAutomationService: trigger}

	return handler, trigger
}

// callWebhook invokes the handler with the given request-path token wired as the
// mux route variable, mirroring the router configuration.
func callWebhook(handler *Handler, requestToken string) (*httptest.ResponseRecorder, int) {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/container-automation/"+requestToken, nil)
	req = mux.SetURLVars(req, map[string]string{"token": requestToken})
	rr := httptest.NewRecorder()

	herr := handler.webhookContainerAutomation(rr, req)
	if herr != nil {
		return rr, herr.StatusCode
	}

	return rr, rr.Code
}

// captureLog swaps the global zerolog logger for a buffer for the rest of the test,
// so the handler's log line can be asserted. Safe because no test here is parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	return &buf
}

func TestWebhookContainerAutomation(t *testing.T) {
	const token = "11111111-2222-3333-4444-555555555555"

	t.Run("valid token on enabled auto-update triggers a pass and returns 202", func(t *testing.T) {
		handler, trigger := setupWebhookAutomationHandler(t, token, true)
		logs := captureLog(t)

		rr, status := callWebhook(handler, token)

		require.Equal(t, http.StatusAccepted, status)
		require.Equal(t, 1, trigger.calls, "a valid kick must trigger exactly one pass")
		require.Empty(t, rr.Body.String(), "202 response has no body")
		require.Contains(t, logs.String(), `"level":"info"`)
		require.Contains(t, logs.String(), "auto-update: webhook kick accepted (202)")
		require.Contains(t, logs.String(), `"remote_addr":"192.0.2.1:1234"`)
		require.NotContains(t, logs.String(), token, "the token must never be logged")
	})

	t.Run("wrong token returns 404 and does not trigger", func(t *testing.T) {
		handler, trigger := setupWebhookAutomationHandler(t, token, true)
		logs := captureLog(t)

		_, status := callWebhook(handler, "wrong-token")

		require.Equal(t, http.StatusNotFound, status)
		require.Zero(t, trigger.calls)
		require.Contains(t, logs.String(), `"level":"warn"`)
		require.Contains(t, logs.String(), "auto-update: webhook kick rejected (404), invalid token")
		require.Contains(t, logs.String(), `"remote_addr":"192.0.2.1:1234"`)
		require.NotContains(t, logs.String(), "wrong-token", "the request token must never be logged")
		require.NotContains(t, logs.String(), token, "the configured token must never be logged")
	})

	t.Run("empty request token returns 404", func(t *testing.T) {
		handler, trigger := setupWebhookAutomationHandler(t, token, true)

		_, status := callWebhook(handler, "")

		require.Equal(t, http.StatusNotFound, status)
		require.Zero(t, trigger.calls)
	})

	t.Run("empty configured token cannot be matched (endpoint disabled)", func(t *testing.T) {
		// No token configured: even an empty request token must not match, or anyone
		// could trigger a pass.
		handler, trigger := setupWebhookAutomationHandler(t, "", true)

		_, status := callWebhook(handler, "")

		require.Equal(t, http.StatusNotFound, status)
		require.Zero(t, trigger.calls)
	})

	t.Run("valid token but auto-update disabled returns 409", func(t *testing.T) {
		handler, trigger := setupWebhookAutomationHandler(t, token, false)
		logs := captureLog(t)

		_, status := callWebhook(handler, token)

		require.Equal(t, http.StatusConflict, status)
		require.Zero(t, trigger.calls, "a disabled endpoint must not trigger a pass")
		require.Contains(t, logs.String(), `"level":"info"`)
		require.Contains(t, logs.String(), "auto-update: webhook kick rejected (409), auto-update is disabled")
		require.Contains(t, logs.String(), `"remote_addr":"192.0.2.1:1234"`)
		require.NotContains(t, logs.String(), token, "the token must never be logged")
	})
}
