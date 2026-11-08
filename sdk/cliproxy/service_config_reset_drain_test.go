package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestResetDrainRoutingSelector(t *testing.T) {
	for _, input := range []string{"reset-drain", "reset-first", "rd"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "reset-drain" {
			t.Fatalf("strategy(%q) = %q, want reset-drain", input, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.ResetDrainSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.ResetDrainSelector", newRoutingSelector(state))
		}
	}

	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "reset-drain", SessionAffinity: true},
	})
	if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector type with session affinity = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state))
	}
}
