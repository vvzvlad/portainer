package settings

import (
	"cmp"
	"net/http"
	"net/url"
	"strings"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/filesystem"
	"github.com/portainer/portainer/api/internal/edge"
	"github.com/portainer/portainer/pkg/libhelm"
	httperror "github.com/portainer/portainer/pkg/libhttp/error"
	"github.com/portainer/portainer/pkg/libhttp/request"
	"github.com/portainer/portainer/pkg/libhttp/response"
	"github.com/portainer/portainer/pkg/libhttp/ssrf"
	"github.com/portainer/portainer/pkg/validate"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
)

// minAutoHealCheckInterval is the lower bound for the auto-heal check interval.
// A near-zero interval (e.g. 1ms) is valid as a positive duration but would hammer
// Docker with list/inspect calls for no benefit; keep a sane floor, mirroring the
// auto-update poll-interval validation.
const minAutoHealCheckInterval = time.Second

// minAutoUpdatePollInterval is the lower bound for the auto-update poll interval.
// Polling more often than this hammers registries (rate limits) for no benefit:
// the image-status cache (~5m) bounds detection latency, so a sub-minute interval
// only adds registry load without resolving new images any faster.
const minAutoUpdatePollInterval = time.Minute

// minAutoUpdateRollbackTimeout is the lower bound for the health-gate rollback
// timeout. A near-zero timeout (e.g. 1ms) would roll back almost immediately,
// before any container can pass its healthcheck, defeating the gate; keep a sane
// floor so the gate has a realistic chance to observe health.
const minAutoUpdateRollbackTimeout = 10 * time.Second

type settingsUpdatePayload struct {
	// URL to a logo that will be displayed on the login page as well as on top of the sidebar. Will use default Portainer logo when value is empty string
	LogoURL *string `example:"https://mycompany.mydomain.tld/logo.png"`
	// A list of label name & value that will be used to hide containers when querying containers
	BlackListedLabels []portainer.Pair
	// Active authentication method for the Portainer instance. Valid values are: 1 for internal, 2 for LDAP, or 3 for oauth
	AuthenticationMethod *int `example:"1"`
	InternalAuthSettings *portainer.InternalAuthSettings
	LDAPSettings         *portainer.LDAPSettings
	OAuthSettings        *portainer.OAuthSettings
	// The interval in which environment(endpoint) snapshots are created
	SnapshotInterval *string `example:"5m"`
	// URL to the templates that will be displayed in the UI when navigating to App Templates
	TemplatesURL *string `example:"https://raw.githubusercontent.com/portainer/templates/master/templates.json"`
	// Deployment options for encouraging deployment as code
	GlobalDeploymentOptions  *portainer.GlobalDeploymentOptions // The default check in interval for edge agent (in seconds)
	EdgeAgentCheckinInterval *int                               `example:"5"`
	// Whether edge compute features are enabled
	EnableEdgeComputeFeatures *bool `example:"true"`
	// The duration of a user session
	UserSessionTimeout *string `example:"5m"`
	// The expiry of a Kubeconfig
	KubeconfigExpiry *string `example:"24h" default:"0"`
	// Helm repository URL
	HelmRepositoryURL *string `example:"https://charts.bitnami.com/bitnami"`
	// Kubectl Shell Image
	KubectlShellImage *string `example:"portainer/kubectl-shell:latest"`
	// TrustOnFirstConnect makes Portainer accepting edge agent connection by default
	TrustOnFirstConnect *bool `example:"false"`
	// EnforceEdgeID makes Portainer store the Edge ID instead of accepting anyone
	EnforceEdgeID *bool `example:"false"`
	// EdgePortainerURL is the URL that is exposed to edge agents
	EdgePortainerURL *string `json:"EdgePortainerURL"`
	// ForceSecureCookies forces the Secure attribute on auth cookies regardless of the detected scheme
	ForceSecureCookies *bool `example:"false"`
	// Native container automation settings (auto-heal / auto-update)
	ContainerAutomation *containerAutomationSettingsPayload
}

type containerAutomationSettingsPayload struct {
	AutoHeal     *autoHealSettingsPayload
	AutoUpdate   *autoUpdateSettingsPayload
	Notification *notificationSettingsPayload
}

type notificationSettingsPayload struct {
	UpdateWebhookURL *string `example:"https://example.com/notify?msg={{message}}"`
	HealWebhookURL   *string `example:"https://example.com/notify?msg={{message}}"`
}

type autoHealSettingsPayload struct {
	Enabled       *bool   `example:"false"`
	CheckInterval *string `example:"30s"`
	Scope         *string `example:"labeled"`
}

type autoUpdateSettingsPayload struct {
	Enabled           *bool   `example:"false"`
	PollInterval      *string `example:"6h"`
	Scope             *string `example:"labeled"`
	Cleanup           *bool   `example:"false"`
	RollbackOnFailure *bool   `example:"false"`
	RollbackTimeout   *string `example:"120s"`
	// RegenerateWebhookToken, when true, (re)generates the inbound registry-push
	// webhook token server-side. The client never supplies the token value itself;
	// it only requests this action, so the secret is always a fresh server uuid.
	RegenerateWebhookToken *bool `example:"false"`
	// ClearWebhookToken, when true, removes the webhook token and thereby disables
	// the inbound trigger endpoint. RegenerateWebhookToken takes precedence if both
	// are set.
	ClearWebhookToken *bool `example:"false"`
}

func (payload *settingsUpdatePayload) Validate(r *http.Request) error {
	if payload.AuthenticationMethod != nil && *payload.AuthenticationMethod != 1 && *payload.AuthenticationMethod != 2 && *payload.AuthenticationMethod != 3 {
		return errors.New("Invalid authentication method value. Value must be one of: 1 (internal), 2 (LDAP/AD) or 3 (OAuth)")
	}

	if payload.LogoURL != nil && *payload.LogoURL != "" && !validate.IsURL(*payload.LogoURL) {
		return errors.New("Invalid logo URL. Must correspond to a valid URL format")
	}

	if payload.TemplatesURL != nil && *payload.TemplatesURL != "" && !validate.IsURL(*payload.TemplatesURL) {
		return errors.New("Invalid external templates URL. Must correspond to a valid URL format")
	}

	if payload.HelmRepositoryURL != nil && *payload.HelmRepositoryURL != "" && !validate.IsURL(*payload.HelmRepositoryURL) {
		return errors.New("Invalid Helm repository URL. Must correspond to a valid URL format")
	}

	if payload.HelmRepositoryURL != nil && *payload.HelmRepositoryURL != "" {
		if err := ssrf.CheckURL(r.Context(), *payload.HelmRepositoryURL); err != nil {
			return errors.New("Invalid Helm repository URL. Must correspond to a valid URL format")
		}
	}

	if payload.UserSessionTimeout != nil {
		if _, err := time.ParseDuration(*payload.UserSessionTimeout); err != nil {
			return errors.New("Invalid user session timeout")
		}
	}

	if payload.KubeconfigExpiry != nil {
		if _, err := time.ParseDuration(*payload.KubeconfigExpiry); err != nil {
			return errors.New("Invalid Kubeconfig Expiry")
		}
	}

	if payload.EdgePortainerURL != nil && *payload.EdgePortainerURL != "" {
		if _, err := edge.ParseHostForEdge(*payload.EdgePortainerURL); err != nil {
			return err
		}
	}

	if payload.OAuthSettings != nil {
		if payload.OAuthSettings.AuthStyle < oauth2.AuthStyleAutoDetect || payload.OAuthSettings.AuthStyle > oauth2.AuthStyleInHeader {
			return errors.New("Invalid OAuth AuthStyle")
		}
	}

	if payload.ContainerAutomation != nil && payload.ContainerAutomation.AutoHeal != nil {
		autoHeal := payload.ContainerAutomation.AutoHeal
		if autoHeal.CheckInterval != nil {
			if d, err := time.ParseDuration(*autoHeal.CheckInterval); err != nil || d < minAutoHealCheckInterval {
				return errors.New("Invalid auto-heal check interval. Must be a duration of at least 1s (e.g. 30s)")
			}
		}

		if autoHeal.Scope != nil && *autoHeal.Scope != "labeled" && *autoHeal.Scope != "all" {
			return errors.New("Invalid auto-heal scope. Value must be one of: labeled, all")
		}
	}

	if payload.ContainerAutomation != nil && payload.ContainerAutomation.AutoUpdate != nil {
		autoUpdate := payload.ContainerAutomation.AutoUpdate
		if autoUpdate.PollInterval != nil {
			if d, err := time.ParseDuration(*autoUpdate.PollInterval); err != nil || d < minAutoUpdatePollInterval {
				return errors.New("Invalid auto-update poll interval. Must be a duration of at least 1m (e.g. 6h)")
			}
		}

		if autoUpdate.Scope != nil && *autoUpdate.Scope != "labeled" && *autoUpdate.Scope != "all" {
			return errors.New("Invalid auto-update scope. Value must be one of: labeled, all")
		}

		if autoUpdate.RollbackTimeout != nil {
			if d, err := time.ParseDuration(*autoUpdate.RollbackTimeout); err != nil || d < minAutoUpdateRollbackTimeout {
				return errors.New("Invalid auto-update rollback timeout. Must be a duration of at least 10s (e.g. 120s)")
			}
		}
	}

	if payload.ContainerAutomation != nil && payload.ContainerAutomation.Notification != nil {
		notification := payload.ContainerAutomation.Notification
		// Each mechanism's webhook URL is independently optional: validate a URL only
		// when it is provided and non-empty. The URL may carry the "{{message}}"
		// placeholder, so we accept any http(s) URL with a host rather than a strict
		// format check.
		if err := validateNotificationWebhookURL(notification.UpdateWebhookURL, "auto-update"); err != nil {
			return err
		}
		if err := validateNotificationWebhookURL(notification.HealWebhookURL, "auto-heal"); err != nil {
			return err
		}
	}

	return nil
}

// validateNotificationWebhookURL validates an optional container-automation
// notification webhook URL: nil or empty is accepted (the mechanism's webhook is
// disabled); otherwise it must be a valid http(s) URL with a host. mechanism
// names the automation the URL belongs to for the error message.
func validateNotificationWebhookURL(webhookURL *string, mechanism string) error {
	if webhookURL == nil || *webhookURL == "" {
		return nil
	}

	u, err := url.Parse(*webhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("Invalid " + mechanism + " notification webhook URL. Must be a valid http(s) URL")
	}

	return nil
}

// @id SettingsUpdate
// @summary Update Portainer settings
// @description Update Portainer settings.
// @description **Access policy**: administrator
// @tags settings
// @security ApiKeyAuth
// @security jwt
// @accept json
// @produce json
// @param body body settingsUpdatePayload true "New settings"
// @success 200 {object} portainer.Settings "Success"
// @failure 400 "Invalid request"
// @failure 500 "Server error"
// @router /settings [put]
func (handler *Handler) settingsUpdate(w http.ResponseWriter, r *http.Request) *httperror.HandlerError {
	var payload settingsUpdatePayload
	err := request.DecodeAndValidateJSONPayload(r, &payload)
	if err != nil {
		return httperror.BadRequest("Invalid request payload", err)
	}

	var settings *portainer.Settings
	if err = handler.DataStore.UpdateTx(func(tx dataservices.DataStoreTx) error {
		settings, err = handler.updateSettings(tx, payload)

		return err
	}); err != nil {
		return response.TxErrorResponse(err)
	}

	// Re-apply container automation settings so the auto-heal and auto-update jobs
	// are rescheduled (or stopped) with the new interval/scope after a successful
	// save.
	if handler.ContainerAutomationService != nil {
		if err := handler.ContainerAutomationService.Reload(); err != nil {
			log.Warn().Err(err).Msg("unable to reload container automation settings")
		}
	}

	hideFields(settings)
	return response.JSON(w, settings)
}

func (handler *Handler) updateSettings(tx dataservices.DataStoreTx, payload settingsUpdatePayload) (*portainer.Settings, error) {
	settings, err := tx.Settings().Settings()
	if err != nil {
		return nil, httperror.InternalServerError("Unable to retrieve the settings from the database", err)
	}

	if payload.AuthenticationMethod != nil {
		settings.AuthenticationMethod = portainer.AuthenticationMethod(*payload.AuthenticationMethod)
	}

	settings.LogoURL = *cmp.Or(payload.LogoURL, &settings.LogoURL)
	settings.TemplatesURL = *cmp.Or(payload.TemplatesURL, &settings.TemplatesURL)

	// Update the global deployment options, and the environment deployment options if they have changed
	settings.GlobalDeploymentOptions = *cmp.Or(payload.GlobalDeploymentOptions, &settings.GlobalDeploymentOptions)

	if payload.HelmRepositoryURL != nil {
		settings.HelmRepositoryURL = ""
		if *payload.HelmRepositoryURL != "" {
			newHelmRepo := strings.TrimSuffix(strings.ToLower(*payload.HelmRepositoryURL), "/")

			if newHelmRepo != settings.HelmRepositoryURL && newHelmRepo != portainer.DefaultHelmRepositoryURL {
				if err := libhelm.ValidateHelmRepositoryURL(*payload.HelmRepositoryURL, nil); err != nil {
					return nil, httperror.BadRequest("Invalid Helm repository URL. Must correspond to a valid URL format", err)
				}
			}

			settings.HelmRepositoryURL = newHelmRepo
		}
	}

	if payload.BlackListedLabels != nil {
		settings.BlackListedLabels = payload.BlackListedLabels
	}

	if payload.InternalAuthSettings != nil {
		settings.InternalAuthSettings.RequiredPasswordLength = payload.InternalAuthSettings.RequiredPasswordLength
	}

	if payload.LDAPSettings != nil {
		ldapReaderDN := cmp.Or(payload.LDAPSettings.ReaderDN, settings.LDAPSettings.ReaderDN)
		ldapPassword := cmp.Or(payload.LDAPSettings.Password, settings.LDAPSettings.Password)

		settings.LDAPSettings = *payload.LDAPSettings
		settings.LDAPSettings.ReaderDN = ldapReaderDN
		settings.LDAPSettings.Password = ldapPassword
	}

	if payload.OAuthSettings != nil {
		clientSecret := payload.OAuthSettings.ClientSecret
		if clientSecret == "" {
			clientSecret = settings.OAuthSettings.ClientSecret
		}

		kubeSecret := payload.OAuthSettings.KubeSecretKey
		if kubeSecret == nil {
			kubeSecret = settings.OAuthSettings.KubeSecretKey
		}

		settings.OAuthSettings = *payload.OAuthSettings
		settings.OAuthSettings.ClientSecret = clientSecret
		settings.OAuthSettings.KubeSecretKey = kubeSecret
		settings.OAuthSettings.AuthStyle = payload.OAuthSettings.AuthStyle
	}

	settings.EnableEdgeComputeFeatures = *cmp.Or(payload.EnableEdgeComputeFeatures, &settings.EnableEdgeComputeFeatures)
	settings.TrustOnFirstConnect = *cmp.Or(payload.TrustOnFirstConnect, &settings.TrustOnFirstConnect)
	settings.EnforceEdgeID = *cmp.Or(payload.EnforceEdgeID, &settings.EnforceEdgeID)
	settings.EdgePortainerURL = *cmp.Or(payload.EdgePortainerURL, &settings.EdgePortainerURL)
	settings.ForceSecureCookies = *cmp.Or(payload.ForceSecureCookies, &settings.ForceSecureCookies)

	if payload.SnapshotInterval != nil && *payload.SnapshotInterval != settings.SnapshotInterval {
		if err := handler.updateSnapshotInterval(settings, *payload.SnapshotInterval); err != nil {
			return nil, httperror.InternalServerError("Unable to update snapshot interval", err)
		}
	}

	settings.EdgeAgentCheckinInterval = *cmp.Or(payload.EdgeAgentCheckinInterval, &settings.EdgeAgentCheckinInterval)
	settings.KubeconfigExpiry = *cmp.Or(payload.KubeconfigExpiry, &settings.KubeconfigExpiry)

	if payload.UserSessionTimeout != nil {
		settings.UserSessionTimeout = *payload.UserSessionTimeout

		userSessionDuration, _ := time.ParseDuration(*payload.UserSessionTimeout)

		handler.JWTService.SetUserSessionDuration(userSessionDuration)
	}

	if err := handler.updateTLS(settings); err != nil {
		return nil, err
	}

	settings.KubectlShellImage = *cmp.Or(payload.KubectlShellImage, &settings.KubectlShellImage)

	if payload.ContainerAutomation != nil && payload.ContainerAutomation.AutoHeal != nil {
		autoHeal := payload.ContainerAutomation.AutoHeal
		current := &settings.ContainerAutomation.AutoHeal
		current.Enabled = *cmp.Or(autoHeal.Enabled, &current.Enabled)
		current.CheckInterval = *cmp.Or(autoHeal.CheckInterval, &current.CheckInterval)
		current.Scope = *cmp.Or(autoHeal.Scope, &current.Scope)
	}

	if payload.ContainerAutomation != nil && payload.ContainerAutomation.AutoUpdate != nil {
		autoUpdate := payload.ContainerAutomation.AutoUpdate
		current := &settings.ContainerAutomation.AutoUpdate
		current.Enabled = *cmp.Or(autoUpdate.Enabled, &current.Enabled)
		current.PollInterval = *cmp.Or(autoUpdate.PollInterval, &current.PollInterval)
		current.Scope = *cmp.Or(autoUpdate.Scope, &current.Scope)
		current.Cleanup = *cmp.Or(autoUpdate.Cleanup, &current.Cleanup)
		current.RollbackOnFailure = *cmp.Or(autoUpdate.RollbackOnFailure, &current.RollbackOnFailure)
		current.RollbackTimeout = *cmp.Or(autoUpdate.RollbackTimeout, &current.RollbackTimeout)

		// Webhook token actions. The token value is never accepted from the client:
		// regenerate mints a fresh server-side uuid (enabling/rotating the inbound
		// trigger endpoint), clear removes it (disabling the endpoint). Regenerate wins
		// if both are set.
		switch {
		case autoUpdate.RegenerateWebhookToken != nil && *autoUpdate.RegenerateWebhookToken:
			token, err := uuid.NewRandom()
			if err != nil {
				return nil, httperror.InternalServerError("Unable to generate a webhook token", err)
			}
			current.WebhookToken = token.String()
		case autoUpdate.ClearWebhookToken != nil && *autoUpdate.ClearWebhookToken:
			current.WebhookToken = ""
		}
	}

	if payload.ContainerAutomation != nil && payload.ContainerAutomation.Notification != nil {
		notification := payload.ContainerAutomation.Notification
		current := &settings.ContainerAutomation.Notification
		current.UpdateWebhookURL = *cmp.Or(notification.UpdateWebhookURL, &current.UpdateWebhookURL)
		current.HealWebhookURL = *cmp.Or(notification.HealWebhookURL, &current.HealWebhookURL)
	}

	if err := tx.Settings().UpdateSettings(settings); err != nil {
		return nil, httperror.InternalServerError("Unable to persist settings changes inside the database", err)
	}

	return settings, nil
}

func (handler *Handler) updateSnapshotInterval(settings *portainer.Settings, snapshotInterval string) error {
	settings.SnapshotInterval = snapshotInterval

	return handler.SnapshotService.SetSnapshotInterval(snapshotInterval)
}

func (handler *Handler) updateTLS(settings *portainer.Settings) error {
	if (settings.LDAPSettings.TLSConfig.TLS || settings.LDAPSettings.StartTLS) && !settings.LDAPSettings.TLSConfig.TLSSkipVerify {
		caCertPath, _ := handler.FileService.GetPathForTLSFile(filesystem.LDAPStorePath, portainer.TLSFileCA)
		settings.LDAPSettings.TLSConfig.TLSCACertPath = caCertPath

		return nil
	}

	settings.LDAPSettings.TLSConfig.TLSCACertPath = ""

	if err := handler.FileService.DeleteTLSFiles(filesystem.LDAPStorePath); err != nil {
		return httperror.InternalServerError("Unable to remove TLS files from disk", err)
	}

	return nil
}
