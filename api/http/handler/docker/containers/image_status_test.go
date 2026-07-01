package containers

import (
	"encoding/json"
	"testing"

	"github.com/portainer/portainer/api/docker/images"
)

// TestImageStatusResponse_JSONShape verifies the wire format of the image status
// response: the status string is mapped from the engine enum and the message is
// omitted when empty, matching what the CE frontend badge expects.
func TestImageStatusResponse_JSONShape(t *testing.T) {
	tests := []struct {
		name     string
		resp     imageStatusResponse
		expected string
	}{
		{
			name:     "outdated without message",
			resp:     imageStatusResponse{Status: string(images.Outdated)},
			expected: `{"Status":"outdated"}`,
		},
		{
			name:     "updated without message",
			resp:     imageStatusResponse{Status: string(images.Updated)},
			expected: `{"Status":"updated"}`,
		},
		{
			name:     "skipped without message",
			resp:     imageStatusResponse{Status: string(images.Skipped)},
			expected: `{"Status":"skipped"}`,
		},
		{
			name:     "error carries a message",
			resp:     imageStatusResponse{Status: string(images.Error), Message: "registry unreachable"},
			expected: `{"Status":"error","Message":"registry unreachable"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.resp)
			if err != nil {
				t.Fatalf("unexpected marshal error: %v", err)
			}

			if string(got) != tt.expected {
				t.Errorf("unexpected JSON\n got: %s\nwant: %s", got, tt.expected)
			}
		})
	}
}
