package main

import (
	"encoding/json"
	"testing"
)

func TestManifest(t *testing.T) {
	var m struct {
		PluginID     string `json:"plugin_id"`
		Capabilities []struct {
			Type          string   `json:"type"`
			Subscriptions []string `json:"subscriptions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	if m.PluginID != "neilmc78.collections.boxsets" || len(m.Capabilities) != 2 {
		t.Fatalf("manifest %+v", m)
	}
	if m.Capabilities[1].Type != "event_consumer.v1" || len(m.Capabilities[1].Subscriptions) != 2 {
		t.Fatalf("event consumer %+v", m.Capabilities[1])
	}
}
