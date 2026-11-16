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

func TestResetPaceRoutingSelector(t *testing.T) {
	for _, input := range []string{"reset-pace", "resetpace", "reset-drain-paced", "rp"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "reset-pace" {
			t.Fatalf("strategy(%q) = %q, want reset-pace", input, state.strategy)
		}
		selector, ok := newRoutingSelector(state).(*coreauth.ResetDrainSelector)
		if !ok || !selector.Paced {
			t.Fatalf("selector = %#v, want paced *auth.ResetDrainSelector", newRoutingSelector(state))
		}
	}
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "reset-drain"},
	})
	if selector := newRoutingSelector(state).(*coreauth.ResetDrainSelector); selector.Paced {
		t.Fatal("reset-drain selector is paced, want unpaced")
	}
}
