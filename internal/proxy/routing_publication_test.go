package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestRoutingPublicationWinsOverPreparedEndpointSave(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()
	for _, routingFirst := range []bool{false, true} {
		t.Run(map[bool]string{true: "routing publishes first", false: "endpoint publishes first"}[routingFirst], func(t *testing.T) {
			base := &config.Config{Models: map[string]config.Model{"m": {Protocols: []string{"openai"}, TimeoutS: 5, Pool: []config.Replica{{URL: backend.URL, Weight: 1, MaxInflight: 4}}}}}
			h := New(base)
			defer h.Close()
			endpoint := *base
			endpoint.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
			managed := *base
			rpm := 1
			managed.Groups = map[string]config.Group{"staff": {Models: []string{"m"}, RPM: &rpm}}
			endpointPublish, _, err := h.Prepare(t.Context(), &endpoint, "m")
			if err != nil {
				t.Fatal(err)
			}
			routingPublish, _, err := h.PrepareRouting(t.Context(), &managed, []string{"m"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if routingFirst {
				routingPublish()
				endpointPublish()
			} else {
				endpointPublish()
				routingPublish()
			}
			current, _ := h.Snapshot()
			if current != &managed || current.Groups["staff"].RPM == nil || *current.Groups["staff"].RPM != 1 {
				t.Fatal("prepared endpoint replaced managed policy")
			}
		})
	}
}
