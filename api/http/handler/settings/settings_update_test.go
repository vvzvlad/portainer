package settings

import (
	"net/http/httptest"
	"testing"
)

func strptr(s string) *string { return &s }

// TestSettingsUpdatePayloadValidateAutoUpdatePollInterval covers the auto-update
// poll-interval floor (F3): durations below minAutoUpdatePollInterval (1m), as
// well as malformed or non-positive durations, must be rejected.
func TestSettingsUpdatePayloadValidateAutoUpdatePollInterval(t *testing.T) {
	cases := []struct {
		name     string
		interval string
		wantErr  bool
	}{
		{name: "one second is below the floor", interval: "1s", wantErr: true},
		{name: "fifty-nine seconds is below the floor", interval: "59s", wantErr: true},
		{name: "exactly one minute is allowed", interval: "1m", wantErr: false},
		{name: "six hours is allowed", interval: "6h", wantErr: false},
		{name: "zero is rejected", interval: "0s", wantErr: true},
		{name: "negative is rejected", interval: "-5m", wantErr: true},
		{name: "unparseable is rejected", interval: "soon", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := settingsUpdatePayload{
				ContainerAutomation: &containerAutomationSettingsPayload{
					AutoUpdate: &autoUpdateSettingsPayload{
						PollInterval: strptr(tc.interval),
					},
				},
			}

			err := payload.Validate(httptest.NewRequest("PUT", "/settings", nil))
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tc.interval)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.interval, err)
			}
		})
	}
}

// TestSettingsUpdatePayloadValidateAutoHealCheckInterval covers the auto-heal
// check-interval floor (F6): durations below minAutoHealCheckInterval (1s), as
// well as malformed or non-positive durations, must be rejected, mirroring the
// auto-update poll-interval validation.
func TestSettingsUpdatePayloadValidateAutoHealCheckInterval(t *testing.T) {
	cases := []struct {
		name     string
		interval string
		wantErr  bool
	}{
		{name: "one millisecond is below the floor", interval: "1ms", wantErr: true},
		{name: "half a second is below the floor", interval: "500ms", wantErr: true},
		{name: "exactly one second is allowed", interval: "1s", wantErr: false},
		{name: "thirty seconds is allowed", interval: "30s", wantErr: false},
		{name: "zero is rejected", interval: "0s", wantErr: true},
		{name: "negative is rejected", interval: "-5s", wantErr: true},
		{name: "unparseable is rejected", interval: "soon", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := settingsUpdatePayload{
				ContainerAutomation: &containerAutomationSettingsPayload{
					AutoHeal: &autoHealSettingsPayload{
						CheckInterval: strptr(tc.interval),
					},
				},
			}

			err := payload.Validate(httptest.NewRequest("PUT", "/settings", nil))
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tc.interval)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.interval, err)
			}
		})
	}
}

// TestSettingsUpdatePayloadValidateRollbackTimeout covers the M5 health-gated
// rollback timeout and its floor (F7): it must be a Go duration of at least
// minAutoUpdateRollbackTimeout (10s), rejecting near-zero, non-positive and
// malformed values.
func TestSettingsUpdatePayloadValidateRollbackTimeout(t *testing.T) {
	cases := []struct {
		name    string
		timeout string
		wantErr bool
	}{
		{name: "two minutes is allowed", timeout: "120s", wantErr: false},
		{name: "compound duration is allowed", timeout: "1m30s", wantErr: false},
		{name: "exactly the floor is allowed", timeout: "10s", wantErr: false},
		{name: "one millisecond is below the floor", timeout: "1ms", wantErr: true},
		{name: "nine seconds is below the floor", timeout: "9s", wantErr: true},
		{name: "zero is rejected", timeout: "0s", wantErr: true},
		{name: "negative is rejected", timeout: "-5s", wantErr: true},
		{name: "unparseable is rejected", timeout: "soon", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := settingsUpdatePayload{
				ContainerAutomation: &containerAutomationSettingsPayload{
					AutoUpdate: &autoUpdateSettingsPayload{
						RollbackTimeout: strptr(tc.timeout),
					},
				},
			}

			err := payload.Validate(httptest.NewRequest("PUT", "/settings", nil))
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tc.timeout)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.timeout, err)
			}
		})
	}
}

// TestSettingsUpdatePayloadValidateNotificationWebhookURL covers the shared
// container-automation notification webhook URL: it is optional (empty is valid),
// must be a valid http(s) URL with a host when set, and accepts the
// "{{message}}" placeholder.
func TestSettingsUpdatePayloadValidateNotificationWebhookURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "empty is allowed (disabled)", url: "", wantErr: false},
		{name: "plain https URL is allowed", url: "https://example.com/notify", wantErr: false},
		{name: "http URL is allowed", url: "http://example.com/notify", wantErr: false},
		{name: "URL with placeholder is allowed", url: "https://example.com/notify?msg={{message}}", wantErr: false},
		{name: "missing scheme is rejected", url: "example.com/notify", wantErr: true},
		{name: "non-http scheme is rejected", url: "ftp://example.com/notify", wantErr: true},
		{name: "missing host is rejected", url: "https://", wantErr: true},
		{name: "garbage is rejected", url: "not a url", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := settingsUpdatePayload{
				ContainerAutomation: &containerAutomationSettingsPayload{
					Notification: &notificationSettingsPayload{
						WebhookURL: strptr(tc.url),
					},
				},
			}

			err := payload.Validate(httptest.NewRequest("PUT", "/settings", nil))
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.url, err)
			}
		})
	}
}
