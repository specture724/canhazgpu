package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithVisibleDevicesEnv(t *testing.T) {
	tests := []struct {
		name     string
		base     []string
		provider string
		want     []string
	}{
		{
			name:     "NVIDIA replaces CUDA visibility",
			base:     []string{"PATH=/bin", "CUDA_VISIBLE_DEVICES=7", "OTHER=value"},
			provider: "nvidia",
			want:     []string{"PATH=/bin", "OTHER=value", "CUDA_VISIBLE_DEVICES=1,3"},
		},
		{
			name:     "Ascend replaces Ascend visibility only",
			base:     []string{"PATH=/bin", "CUDA_VISIBLE_DEVICES=7", "ASCEND_RT_VISIBLE_DEVICES=4"},
			provider: "ascend",
			want:     []string{"PATH=/bin", "CUDA_VISIBLE_DEVICES=7", "ASCEND_RT_VISIBLE_DEVICES=1,3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, withVisibleDevicesEnv(tt.base, tt.provider, "1,3"))
		})
	}
}
