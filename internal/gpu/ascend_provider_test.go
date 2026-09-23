package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ascendSMIOutput = `
+------------------------------------------------------------------------------------------------+
| npu-smi 25.5.0                   Version: 25.5.0                                               |
+---------------------------+---------------+----------------------------------------------------+
| NPU   Name                | Health        | Power(W)    Temp(C)           Hugepages-Usage(page)|
| Chip                      | Bus-Id        | AICore(%)   Memory-Usage(MB)  HBM-Usage(MB)        |
+===========================+===============+====================================================+
| 0     910B1               | OK            | 92.4        48                0    / 0             |
| 0                         | 0000:C1:00.0  | 73          0    / 0          3452 / 65536         |
+===========================+===============+====================================================+
| 1     910B1               | OK            | 97.8        50                0    / 0             |
| 0                         | 0000:01:00.0  | 12          0    / 0          3443 / 65536         |
+===========================+===============+====================================================+
+---------------------------+---------------+----------------------------------------------------+
| NPU     Chip              | Process id    | Process name             | Process memory(MB)      |
+===========================+===============+====================================================+
| 0       0                 | 2241871       | vLLM-OmniDiff            | 8118                    |
+===========================+===============+====================================================+
| No running processes found in NPU 1                                                            |
+===========================+===============+====================================================+
`

func TestParseAscendSMIOutput(t *testing.T) {
	usage, err := parseAscendSMIOutput(ascendSMIOutput)
	require.NoError(t, err)
	require.Len(t, usage, 2)

	assert.Equal(t, 0, usage[0].GPUID)
	assert.Equal(t, "910B1", usage[0].Model)
	assert.Equal(t, "Ascend", usage[0].Provider)
	assert.Equal(t, 73, usage[0].UtilizationPercent)
	assert.Equal(t, 8118, usage[0].MemoryMB)
	require.Len(t, usage[0].Processes, 1)
	assert.Equal(t, 2241871, usage[0].Processes[0].PID)
	assert.Equal(t, "vLLM-OmniDiff", usage[0].Processes[0].ProcessName)
	assert.Equal(t, 8118, usage[0].Processes[0].MemoryMB)

	assert.Equal(t, 1, usage[1].GPUID)
	assert.Equal(t, "910B1", usage[1].Model)
	assert.Equal(t, 12, usage[1].UtilizationPercent)
	assert.Zero(t, usage[1].MemoryMB, "persistent HBM baseline is not user process memory")
	assert.Empty(t, usage[1].Processes)
}

func TestAscendProviderNamesAndVisibility(t *testing.T) {
	assert.Equal(t, AscendProviderName, NewAscendProvider().Name())

	for _, providerName := range []string{"ascend", "npu", "ascend-npu", " ASCEND "} {
		assert.Equal(t, AscendProviderName, CanonicalProviderName(providerName))
		assert.Equal(t, AscendVisibleDevicesEnv, VisibleDevicesEnvVar(providerName))
	}
	assert.Equal(t, "nvidia", CanonicalProviderName(" NVIDIA "))
	assert.Equal(t, "CUDA_VISIBLE_DEVICES", VisibleDevicesEnvVar("amd"))
	assert.Empty(t, CanonicalProviderName("unknown"))
}

func TestNewProviderManagerFromNamesAscend(t *testing.T) {
	manager := NewProviderManagerFromNames([]string{"npu"})
	require.Len(t, manager.providers, 1)
	assert.IsType(t, &AscendProvider{}, manager.providers[0])
}
