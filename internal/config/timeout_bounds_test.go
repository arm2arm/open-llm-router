package config

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestModelTimeoutBoundsAcrossFileAndManagedRouting(t *testing.T) {
	base, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"timeout_s", "idle_timeout_s", "queue_timeout_s"} {
		for _, value := range []int{MaxModelTimeoutS + 1, 9223372037} {
			t.Run(fmt.Sprintf("%s/%d", field, value), func(t *testing.T) {
				model := base.Models["chat-model"]
				model.TimeoutS = MaxModelTimeoutS
				switch field {
				case "timeout_s":
					model.TimeoutS = value
				case "idle_timeout_s":
					model.IdleTimeoutS = value
				case "queue_timeout_s":
					model.QueueTimeoutS = value
				}
				models := maps.Clone(base.Models)
				models["chat-model"] = model
				_, err := WithPolicies(base, models, base.Aliases, base.Groups, func(string, string) (string, bool) { return "", false })
				if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "86400") {
					t.Fatalf("managed oversized timeout accepted: %v", err)
				}
				input := fmt.Sprintf("models:\n  m:\n    protocols: [openai]\n    timeout_s: %d\n    idle_timeout_s: %d\n    queue_timeout_s: %d\n    pool: [{url: 'http://backend:8000', weight: 1, max_inflight: 4}]\n", model.TimeoutS, model.IdleTimeoutS, model.QueueTimeoutS)
				_, err = Parse([]byte(input), func(string) (string, bool) { return "", false })
				if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "86400") {
					t.Fatalf("file oversized timeout accepted: %v", err)
				}
			})
		}
	}
	model := base.Models["chat-model"]
	model.TimeoutS = MaxModelTimeoutS
	model.IdleTimeoutS = MaxModelTimeoutS
	model.QueueTimeoutS = MaxModelTimeoutS
	models := maps.Clone(base.Models)
	models["chat-model"] = model
	if _, err := WithPolicies(base, models, base.Aliases, base.Groups, func(string, string) (string, bool) { return "", false }); err != nil {
		t.Fatal("maximum timeout refused", err)
	}
}
