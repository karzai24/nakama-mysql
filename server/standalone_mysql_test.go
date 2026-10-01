package server

import (
	"testing"

	"github.com/dop251/goja"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The public runtime must expose standard Nakama APIs without private game hooks.
func TestStandaloneRuntimeSurface(t *testing.T) {
	module := &RuntimeJavascriptNakamaModule{}
	methods := module.mappings(goja.New())
	require.Contains(t, methods, "matchCreate")
	require.Contains(t, methods, "storageWrite")
	require.Contains(t, methods, "authenticateTokenGenerate")
	require.NotContains(t, methods, "carromPhysicsAdvance")
	require.NotContains(t, methods, "matchCreateOnce")
}

func TestStandaloneRuntimeEnvironment(t *testing.T) {
	config := ParseArgs(zap.NewNop(), []string{"nakama", "--runtime.env", "CUSTOM_MODULE_SETTING=preserved"})
	require.Equal(t, "preserved", config.GetRuntime().Environment["CUSTOM_MODULE_SETTING"])
	require.Len(t, config.GetRuntime().Environment, 1)
}
