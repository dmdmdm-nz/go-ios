package webinspector

import "testing"

func TestLocalCDPResponse(t *testing.T) {
	handled, response, extra := localCDPResponse(map[string]any{"id": 4, "method": "Runtime.getIsolateId"}, "target-1", "session-1")
	if !handled {
		t.Fatal("expected Runtime.getIsolateId to be handled locally")
	}
	result := response["result"].(map[string]any)
	if result["id"] != 0 {
		t.Fatalf("unexpected isolate id result: %#v", response)
	}
	if len(extra) != 0 {
		t.Fatalf("unexpected extra events: %#v", extra)
	}
}

func TestTranslateCDPCommand(t *testing.T) {
	message := translateCDPCommand(map[string]any{
		"id":     1,
		"method": "Network.setCacheDisabled",
		"params": map[string]any{"cacheDisabled": true},
	})
	if message["method"] != "Network.setResourceCachingDisabled" {
		t.Fatalf("unexpected method: %#v", message["method"])
	}
	params := message["params"].(map[string]any)
	if params["disabled"] != true {
		t.Fatalf("unexpected params: %#v", params)
	}
}

func TestNormalizeConsoleEvent(t *testing.T) {
	normalized, drop := normalizeCDPEvent(map[string]any{
		"method": "Console.messageAdded",
		"params": map[string]any{
			"message": map[string]any{
				"source": "console-api",
				"level":  "debug",
				"text":   "hello",
			},
		},
	})
	if drop {
		t.Fatal("expected console event to be emitted")
	}
	if normalized["method"] != "Log.entryAdded" {
		t.Fatalf("unexpected normalized method: %#v", normalized)
	}
}
