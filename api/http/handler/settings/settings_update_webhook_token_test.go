package settings

import (
	"testing"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/datastore"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/filesystem"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func boolptr(b bool) *bool { return &b }

// newWebhookTokenTestHandler builds a settings Handler backed by a fresh test
// store and a real (temp-dir) file service, enough to exercise updateSettings.
func newWebhookTokenTestHandler(t *testing.T) (*Handler, *datastore.Store) {
	t.Helper()

	_, store := datastore.MustNewTestStore(t, true, false)

	fileService, err := filesystem.NewService(t.TempDir(), "")
	require.NoError(t, err)

	return &Handler{DataStore: store, FileService: fileService}, store
}

// applyAutoUpdate runs updateSettings inside a transaction with the given
// auto-update payload and returns the resulting settings.
func applyAutoUpdate(t *testing.T, handler *Handler, store *datastore.Store, autoUpdate *autoUpdateSettingsPayload) *portainer.Settings {
	t.Helper()

	var settings *portainer.Settings
	require.NoError(t, store.UpdateTx(func(tx dataservices.DataStoreTx) error {
		var e error
		settings, e = handler.updateSettings(tx, settingsUpdatePayload{
			ContainerAutomation: &containerAutomationSettingsPayload{AutoUpdate: autoUpdate},
		})
		return e
	}))

	return settings
}

// TestUpdateSettingsWebhookTokenRegenerate verifies that RegenerateWebhookToken
// mints a fresh server-side uuid, that regenerating again rotates it to a new
// value, and that the client-supplied token value is never trusted (there is no
// field to supply one).
func TestUpdateSettingsWebhookTokenRegenerate(t *testing.T) {
	handler, store := newWebhookTokenTestHandler(t)

	settings := applyAutoUpdate(t, handler, store, &autoUpdateSettingsPayload{
		RegenerateWebhookToken: boolptr(true),
	})
	first := settings.ContainerAutomation.AutoUpdate.WebhookToken
	require.NotEmpty(t, first, "regenerate must set a token")
	_, err := uuid.Parse(first)
	require.NoError(t, err, "the token must be a valid uuid")

	// Persisted to the store.
	persisted, err := store.Settings().Settings()
	require.NoError(t, err)
	require.Equal(t, first, persisted.ContainerAutomation.AutoUpdate.WebhookToken)

	// Regenerating again rotates the token.
	settings = applyAutoUpdate(t, handler, store, &autoUpdateSettingsPayload{
		RegenerateWebhookToken: boolptr(true),
	})
	second := settings.ContainerAutomation.AutoUpdate.WebhookToken
	require.NotEmpty(t, second)
	require.NotEqual(t, first, second, "regenerate must rotate the token")
}

// TestUpdateSettingsWebhookTokenClear verifies that ClearWebhookToken removes the
// token (disabling the inbound endpoint).
func TestUpdateSettingsWebhookTokenClear(t *testing.T) {
	handler, store := newWebhookTokenTestHandler(t)

	settings := applyAutoUpdate(t, handler, store, &autoUpdateSettingsPayload{
		RegenerateWebhookToken: boolptr(true),
	})
	require.NotEmpty(t, settings.ContainerAutomation.AutoUpdate.WebhookToken)

	settings = applyAutoUpdate(t, handler, store, &autoUpdateSettingsPayload{
		ClearWebhookToken: boolptr(true),
	})
	require.Empty(t, settings.ContainerAutomation.AutoUpdate.WebhookToken, "clear must remove the token")
}

// TestUpdateSettingsWebhookTokenUntouched verifies that an auto-update save that
// does not request a token action leaves the existing token unchanged (so saving
// other auto-update fields does not accidentally rotate or drop the endpoint).
func TestUpdateSettingsWebhookTokenUntouched(t *testing.T) {
	handler, store := newWebhookTokenTestHandler(t)

	settings := applyAutoUpdate(t, handler, store, &autoUpdateSettingsPayload{
		RegenerateWebhookToken: boolptr(true),
	})
	token := settings.ContainerAutomation.AutoUpdate.WebhookToken
	require.NotEmpty(t, token)

	settings = applyAutoUpdate(t, handler, store, &autoUpdateSettingsPayload{
		Enabled: boolptr(true),
	})
	require.Equal(t, token, settings.ContainerAutomation.AutoUpdate.WebhookToken, "a non-action save must not touch the token")
}

// TestUpdateSettingsWebhookTokenRegenerateWins verifies that when both actions are
// set, regenerate takes precedence over clear.
func TestUpdateSettingsWebhookTokenRegenerateWins(t *testing.T) {
	handler, store := newWebhookTokenTestHandler(t)

	settings := applyAutoUpdate(t, handler, store, &autoUpdateSettingsPayload{
		RegenerateWebhookToken: boolptr(true),
		ClearWebhookToken:      boolptr(true),
	})
	require.NotEmpty(t, settings.ContainerAutomation.AutoUpdate.WebhookToken, "regenerate must win over clear")
}
