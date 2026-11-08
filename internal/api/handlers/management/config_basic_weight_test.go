package management

import "testing"

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyResetDrain(t *testing.T) {
	for _, input := range []string{"reset-drain", "resetdrain", "reset-first", "resetfirst", "rd", " Reset-Drain "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "reset-drain" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want reset-drain, true", input, got, ok)
		}
	}
}
