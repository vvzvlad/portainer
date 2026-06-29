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
