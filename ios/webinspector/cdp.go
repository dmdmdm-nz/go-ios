package webinspector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type CDPServer struct {
	client *Client
	host   string
	port   int
	server *http.Server
}

type CDPTarget struct {
	Description          string `json:"description"`
	ID                   string `json:"id"`
	Title                string `json:"title"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	DevtoolsFrontendURL  string `json:"devtoolsFrontendUrl"`
}

func NewCDPServer(client *Client, host string, port int) *CDPServer {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 9222
	}
	return &CDPServer{client: client, host: host, port: port}
}

func (s *CDPServer) Addr() string {
	return fmt.Sprintf("%s:%d", s.host, s.port)
}

func (s *CDPServer) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/json", s.handleTargets)
	mux.HandleFunc("/json/list", s.handleTargets)
	mux.HandleFunc("/json/version", s.handleVersion)
	mux.HandleFunc("/devtools/page/", s.handlePageWebSocket)
	s.server = &http.Server{Addr: s.Addr(), Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return ctx.Err()
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *CDPServer) handleTargets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	pages, err := s.client.ListPages(ctx, 250*time.Millisecond)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	targets := make([]CDPTarget, 0, len(pages))
	for _, appPage := range pages {
		page := appPage.Page
		if page.Type != WIRTypeWeb && page.Type != WIRTypeWebPage {
			continue
		}
		wsURL := fmt.Sprintf("ws://%s/devtools/page/%s", s.Addr(), page.Key)
		targets = append(targets, CDPTarget{
			ID:                   page.Key,
			Title:                page.Title,
			Type:                 "page",
			URL:                  page.URL,
			WebSocketDebuggerURL: wsURL,
			DevtoolsFrontendURL:  "/devtools/inspector.html?ws=" + s.Addr() + "/devtools/page/" + page.Key,
		})
	}
	writeJSON(w, targets)
}

func (s *CDPServer) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"Browser":              "Safari",
		"Protocol-Version":     "1.1",
		"User-Agent":           "go-ios",
		"WebKit-Version":       "",
		"webSocketDebuggerUrl": fmt.Sprintf("ws://%s/devtools/browser/%s", s.Addr(), s.client.connectionID),
	})
}

func (s *CDPServer) handlePageWebSocket(w http.ResponseWriter, r *http.Request) {
	pageKey := strings.TrimPrefix(r.URL.Path, "/devtools/page/")
	app, page, ok := s.client.FindPage(pageKey)
	if !ok {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID := strings.ToUpper(uuid.New().String())
	if err := s.client.SetupInspectorSocket(sessionID, app, page, false); err != nil {
		_ = ws.WriteJSON(cdpError(0, err))
		return
	}
	targetID := waitForTargetID(ctx, s.client, ws)
	if targetID == "" {
		return
	}

	errCh := make(chan error, 2)
	go func() {
		errCh <- s.forwardDeviceEvents(ctx, ws)
	}()
	go func() {
		errCh <- s.forwardBrowserCommands(ctx, ws, sessionID, app, page, targetID)
	}()
	<-errCh
}

func (s *CDPServer) forwardDeviceEvents(ctx context.Context, ws *websocket.Conn) error {
	for {
		event, err := s.client.NextEvent(ctx)
		if err != nil {
			return err
		}
		if dispatch, ok := unwrapDispatchMessage(event); ok {
			if normalized, drop := normalizeCDPEvent(dispatch); !drop {
				if err := ws.WriteJSON(normalized); err != nil {
					return err
				}
			}
			continue
		}
		if normalized, drop := normalizeCDPEvent(event); !drop {
			if err := ws.WriteJSON(normalized); err != nil {
				return err
			}
		}
	}
}

func (s *CDPServer) forwardBrowserCommands(ctx context.Context, ws *websocket.Conn, sessionID string, app Application, page Page, targetID string) error {
	for {
		var message map[string]any
		if err := ws.ReadJSON(&message); err != nil {
			return err
		}
		id, _ := numericInt(message["id"])

		if handled, response, extra := localCDPResponse(message, targetID, sessionID); handled {
			if err := ws.WriteJSON(response); err != nil {
				return err
			}
			for _, event := range extra {
				if err := ws.WriteJSON(event); err != nil {
					return err
				}
			}
			continue
		}

		message = translateCDPCommand(message)
		wrapped := map[string]any{
			"method": "Target.sendMessageToTarget",
			"params": map[string]any{
				"targetId": targetID,
				"message":  mustJSON(message),
			},
		}
		if _, err := s.client.SendCommand(ctx, sessionID, app, page, nextWIRID(), "Target.sendMessageToTarget", wrapped["params"].(map[string]any)); err != nil {
			if writeErr := ws.WriteJSON(cdpError(id, err)); writeErr != nil {
				return writeErr
			}
		}
	}
}

func waitForTargetID(ctx context.Context, client *Client, ws *websocket.Conn) string {
	for {
		event, err := client.NextEvent(ctx)
		if err != nil {
			_ = ws.WriteJSON(cdpError(0, err))
			return ""
		}
		targetInfo, ok := event["params"].(map[string]any)["targetInfo"].(map[string]any)
		if !ok {
			continue
		}
		targetID, _ := targetInfo["targetId"].(string)
		_ = ws.WriteJSON(event)
		return targetID
	}
}

func unwrapDispatchMessage(event map[string]any) (map[string]any, bool) {
	if event["method"] != "Target.dispatchMessageFromTarget" {
		return nil, false
	}
	params, ok := event["params"].(map[string]any)
	if !ok {
		return nil, false
	}
	message, _ := params["message"].(string)
	if message == "" {
		return nil, false
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(message), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

func shouldReplyLocally(method string) bool {
	switch method {
	case "Target.setDiscoverTargets",
		"Target.setAutoAttach",
		"Target.setRemoteLocations",
		"DOM.enable",
		"Log.enable",
		"Network.enable",
		"Page.enable",
		"Runtime.enable":
		return true
	default:
		return false
	}
}

func localCDPResponse(message map[string]any, targetID string, sessionID string) (bool, map[string]any, []map[string]any) {
	id, _ := numericInt(message["id"])
	method, _ := message["method"].(string)
	result := map[string]any{}
	var extra []map[string]any
	switch method {
	case "Target.setAutoAttach":
		extra = append(extra, map[string]any{
			"method": "Target.attachedToTarget",
			"params": map[string]any{
				"sessionId": sessionID,
				"targetInfo": map[string]any{
					"targetId": targetID,
				},
				"waitingForDebugger": true,
			},
		})
	case "Target.setDiscoverTargets",
		"Target.setRemoteLocations",
		"DOM.enable",
		"DOMDebugger.setBreakOnCSPViolation",
		"Emulation.setTouchEmulationEnabled",
		"Emulation.setFocusEmulationEnabled",
		"Emulation.setEmulatedVisionDeficiency",
		"Emulation.setEmitTouchEventsForMouse",
		"Emulation.setAutoDarkModeOverride",
		"HeapProfiler.enable",
		"Input.dispatchKeyEvent",
		"Input.emulateTouchFromMouseEvent",
		"Log.startViolationsReport",
		"Network.clearAcceptedEncodingsOverride",
		"Network.setAttachDebugStack",
		"Overlay.enable",
		"Overlay.hideHighlight",
		"Overlay.setPausedInDebuggerMessage",
		"Overlay.setShowContainerQueryOverlays",
		"Overlay.setShowFlexOverlays",
		"Overlay.setShowGridOverlays",
		"Overlay.setShowIsolatedElements",
		"Overlay.setShowScrollSnapOverlays",
		"Overlay.setShowViewportSizeOnResize",
		"Page.screencastFrameAck",
		"Page.startScreencast",
		"Page.stopScreencast",
		"Profiler.enable",
		"Runtime.runIfWaitingForDebugger":
	case "CSS.takeComputedStyleUpdates":
		result["nodeIds"] = []int{}
	case "Network.loadNetworkResource":
		result["resource"] = map[string]any{"success": true}
	case "Runtime.getIsolateId":
		result["id"] = 0
	case "Page.getNavigationHistory":
		result["currentIndex"] = 0
		result["entries"] = []map[string]any{{"id": 0, "url": "", "title": ""}}
	default:
		return false, nil, nil
	}
	return true, map[string]any{"id": id, "result": result}, extra
}

func translateCDPCommand(message map[string]any) map[string]any {
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)
	switch method {
	case "Audits.enable":
		message["method"] = "Audit.setup"
	case "DOM.getBoxModel", "Overlay.highlightNode":
		message["method"] = "DOM.highlightNode"
		if params != nil && method == "DOM.getBoxModel" {
			params["highlightConfig"] = map[string]any{
				"showInfo":     true,
				"contentColor": map[string]any{"r": 111, "g": 168, "b": 220, "a": 0.66},
				"paddingColor": map[string]any{"r": 147, "g": 196, "b": 125, "a": 0.55},
				"borderColor":  map[string]any{"r": 255, "g": 229, "b": 153, "a": 0.66},
				"marginColor":  map[string]any{"r": 246, "g": 178, "b": 107, "a": 0.66},
			}
		}
	case "Log.clear":
		message["method"] = "Console.clearMessages"
	case "Log.disable":
		message["method"] = "Console.disable"
	case "Log.enable":
		message["method"] = "Console.enable"
	case "Emulation.setEmulatedMedia":
		message["method"] = "Page.setEmulatedMedia"
	case "Emulation.setAutoDarkModeOverride":
		message["method"] = "Page.setForcedAppearance"
		if params != nil {
			enabled, _ := params["enabled"].(bool)
			message["params"] = map[string]any{"appearance": map[bool]string{true: "Dark", false: "Light"}[enabled]}
		}
	case "Network.setCacheDisabled":
		message["method"] = "Network.setResourceCachingDisabled"
		if params != nil {
			message["params"] = map[string]any{"disabled": params["cacheDisabled"]}
		}
	case "ServiceWorker.enable":
		message["method"] = "Worker.enable"
	case "CSS.addRule":
		if params != nil {
			if rule, _ := params["ruleText"].(string); rule != "" {
				params["selector"] = strings.Split(rule, "{")[0]
			}
		}
	}
	return message
}

func normalizeCDPEvent(message map[string]any) (map[string]any, bool) {
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)
	switch method {
	case "Target.targetCreated":
		message["method"] = "Target.targetInfoChanged"
		if targetInfo, ok := params["targetInfo"].(map[string]any); ok {
			if provisional, ok := targetInfo["isProvisional"]; ok {
				targetInfo["attached"] = provisional
				delete(targetInfo, "isProvisional")
			}
		}
	case "Target.didCommitProvisionalTarget", "Page.defaultAppearanceDidChange":
		return message, true
	case "Debugger.globalObjectCleared":
		return map[string]any{"method": "DOM.documentUpdated"}, false
	case "Runtime.executionContextCreated":
		if contextMap, ok := params["context"].(map[string]any); ok {
			params["context"] = map[string]any{
				"id":       contextMap["id"],
				"origin":   "default",
				"name":     "",
				"uniqueId": contextMap["frameId"],
			}
		}
	case "Console.messageAdded":
		entry := map[string]any{"source": "javascript", "level": "info", "timestamp": float64(time.Now().UnixMilli()) / 1000}
		if consoleMessage, ok := params["message"].(map[string]any); ok {
			entry["source"] = logSource(stringValue(consoleMessage["source"]))
			entry["level"] = logLevel(stringValue(consoleMessage["level"]))
			entry["text"] = consoleMessage["text"]
			if url := stringValue(consoleMessage["url"]); url != "" {
				entry["url"] = url
			}
			if line, ok := numericInt(consoleMessage["line"]); ok {
				entry["lineNumber"] = line
			}
			if requestID := stringValue(consoleMessage["networkRequestId"]); requestID != "" {
				entry["networkRequestId"] = requestID
			}
		}
		return map[string]any{"method": "Log.entryAdded", "params": map[string]any{"entry": entry}}, false
	case "Network.responseReceived":
		if response, ok := params["response"].(map[string]any); ok {
			resourceType := stringValue(params["type"])
			if !validNetworkResourceType(resourceType) {
				resourceType = "Other"
			}
			normalized := map[string]any{
				"loaderId":  params["loaderId"],
				"requestId": params["requestId"],
				"timestamp": params["timestamp"],
				"type":      resourceType,
				"response": map[string]any{
					"url":               response["url"],
					"status":            response["status"],
					"statusText":        response["statusText"],
					"headers":           response["headers"],
					"mimeType":          response["mimeType"],
					"connectionReused":  false,
					"encodedDataLength": 0,
					"securityState":     "unknown",
				},
			}
			if frameID := params["frameId"]; frameID != nil {
				normalized["frameId"] = frameID
			}
			message["params"] = normalized
		}
	case "Network.loadingFinished":
		metrics, _ := params["metrics"].(map[string]any)
		headerSize, _ := numericInt(metrics["responseHeaderBytesReceived"])
		bodySize, _ := numericInt(metrics["responseBodyBytesReceived"])
		message["params"] = map[string]any{
			"encodedDataLength": headerSize + bodySize,
			"requestId":         params["requestId"],
			"timestamp":         params["timestamp"],
		}
	}
	return message, false
}

func logSource(source string) string {
	switch source {
	case "xml", "javascript", "network", "storage", "appcache", "rendering", "security", "deprecation", "worker", "violation", "intervention", "recommendation", "other":
		return source
	case "console-api":
		return "javascript"
	default:
		return "other"
	}
}

func logLevel(level string) string {
	switch level {
	case "log", "info":
		return "info"
	case "warning":
		return "warning"
	case "error":
		return "error"
	case "debug":
		return "verbose"
	default:
		return "info"
	}
}

func validNetworkResourceType(resourceType string) bool {
	switch resourceType {
	case "Document", "Stylesheet", "Image", "Media", "Font", "Script", "TextTrack", "XHR", "Fetch", "EventSource", "WebSocket", "Manifest", "SignedExchange", "Ping", "CSPViolationReport", "Preflight", "Other":
		return true
	default:
		return false
	}
}

func cdpError(id int, err error) map[string]any {
	return map[string]any{
		"id": id,
		"error": map[string]any{
			"code":    -32000,
			"message": err.Error(),
		},
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func nextWIRID() int {
	return int(time.Now().UnixNano() & 0x7fffffff)
}
