package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	gatewayWSPort       = 18789
	gatewayHookPort     = 18790
	maxWSMessageSize    = 1 << 20 // 1MB
	wsPingInterval      = 30 * time.Second
	wsWriteTimeout      = 10 * time.Second
)

// --- Protocol types (OpenClaw-compatible subset) ---

// GatewayMessage is the wire format for WebSocket messages.
type GatewayMessage struct {
	Type    string `json:"type"`              // "message", "status", "command", "error", "history"
	Session string `json:"session,omitempty"` // session name
	Content string `json:"content,omitempty"` // message content
	Channel string `json:"channel,omitempty"` // source channel
	Role    string `json:"role,omitempty"`    // "user", "assistant"
	Action  string `json:"action,omitempty"`  // for commands: "new", "restart", "list", "kill"
	State   string `json:"state,omitempty"`   // for status: "active", "idle", "stopped"
	Error   string `json:"error,omitempty"`
}

// HookPayload is POSTed by hooks to the internal endpoint.
type HookPayload struct {
	Session string `json:"session"`
	Role    string `json:"role"`    // "user", "assistant"
	Content string `json:"content"`
	Event   string `json:"event"`   // "message", "stop", "compact", "notification"
}

// --- WebSocket client ---

type wsClient struct {
	conn    *websocket.Conn
	session string // subscribed session ("" = all)
	ctx     context.Context
	cancel  context.CancelFunc
}

// --- Gateway ---

// Gateway manages WebSocket connections, broadcasting, and the internal hook endpoint.
type Gateway struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	token   string
	config  *Config
}

var gateway *Gateway

// loadGatewayToken reads the auth token from OpenClaw config or generates one.
func loadGatewayToken() string {
	// Try OpenClaw config first
	home, _ := os.UserHomeDir()
	ocPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if data, err := os.ReadFile(ocPath); err == nil {
		var ocConfig map[string]interface{}
		if json.Unmarshal(data, &ocConfig) == nil {
			if gw, ok := ocConfig["gateway"].(map[string]interface{}); ok {
				if auth, ok := gw["auth"].(map[string]interface{}); ok {
					if token, ok := auth["token"].(string); ok && token != "" {
						return token
					}
				}
			}
		}
	}

	// Fallback: use a token from CCC config or env
	if token := os.Getenv("CCC_GATEWAY_TOKEN"); token != "" {
		return token
	}

	// Generate and persist a random token
	token := generateToken()
	fmt.Fprintf(os.Stderr, "gateway: generated new token: %s\n", token)
	return token
}

func generateToken() string {
	b := make([]byte, 24)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		// Fallback
		return "ccc-default-token"
	}
	defer f.Close()
	f.Read(b)
	return fmt.Sprintf("%x", b)
}

// startGateway launches the WebSocket server and internal hook endpoint.
func startGateway(config *Config) {
	token := loadGatewayToken()
	gateway = &Gateway{
		clients: make(map[*wsClient]struct{}),
		token:   token,
		config:  config,
	}

	// Start WebSocket server
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/ws", gateway.handleWS)
		mux.HandleFunc("/sessions", gateway.handleSessions)
		mux.HandleFunc("/sessions/", gateway.handleSessionMessages)
		mux.HandleFunc("/health", gateway.handleHealth)

		addr := fmt.Sprintf("127.0.0.1:%d", gatewayWSPort)
		fmt.Fprintf(os.Stderr, "gateway: WebSocket server on ws://%s/ws\n", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Fprintf(os.Stderr, "gateway: WS server error: %v\n", err)
		}
	}()

	// Start internal hook endpoint
	go func() {
		hookMux := http.NewServeMux()
		hookMux.HandleFunc("/hook", gateway.handleHookPost)

		addr := fmt.Sprintf("127.0.0.1:%d", gatewayHookPort)
		fmt.Fprintf(os.Stderr, "gateway: hook endpoint on http://%s/hook\n", addr)
		if err := http.ListenAndServe(addr, hookMux); err != nil {
			fmt.Fprintf(os.Stderr, "gateway: hook server error: %v\n", err)
		}
	}()
}

// --- Auth ---

func (g *Gateway) authenticate(r *http.Request) bool {
	// Check Authorization header
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ") == g.token
	}
	// Check query param (for websocat compatibility)
	if t := r.URL.Query().Get("token"); t != "" {
		return t == g.token
	}
	return false
}

// --- WebSocket handler ---

func (g *Gateway) handleWS(w http.ResponseWriter, r *http.Request) {
	if !g.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Allow any origin for local use
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: ws accept error: %v\n", err)
		return
	}
	conn.SetReadLimit(maxWSMessageSize)

	ctx, cancel := context.WithCancel(r.Context())
	client := &wsClient{
		conn:    conn,
		session: r.URL.Query().Get("session"), // Optional session filter
		ctx:     ctx,
		cancel:  cancel,
	}

	g.mu.Lock()
	g.clients[client] = struct{}{}
	g.mu.Unlock()

	fmt.Fprintf(os.Stderr, "gateway: client connected (session=%s)\n", client.session)

	// Send current session statuses
	g.sendSessionStatuses(client)

	// Read loop
	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.clients, client)
			g.mu.Unlock()
			cancel()
			conn.Close(websocket.StatusNormalClosure, "bye")
			fmt.Fprintf(os.Stderr, "gateway: client disconnected\n")
		}()

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}

			var msg GatewayMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				g.sendError(client, "invalid JSON")
				continue
			}

			g.handleClientMessage(client, &msg)
		}
	}()

	// Ping loop to keep connection alive
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wsPingInterval):
			if err := conn.Ping(ctx); err != nil {
				return
			}
		}
	}
}

func (g *Gateway) handleClientMessage(client *wsClient, msg *GatewayMessage) {
	switch msg.Type {
	case "message":
		g.handleIncomingMessage(client, msg)
	case "command":
		g.handleCommand(client, msg)
	default:
		g.sendError(client, fmt.Sprintf("unknown message type: %s", msg.Type))
	}
}

func (g *Gateway) handleIncomingMessage(client *wsClient, msg *GatewayMessage) {
	if msg.Session == "" {
		g.sendError(client, "session is required")
		return
	}
	if msg.Content == "" {
		g.sendError(client, "content is required")
		return
	}

	config, err := loadConfig()
	if err != nil {
		g.sendError(client, "failed to load config")
		return
	}

	// Find the session
	info, exists := config.Sessions[msg.Session]
	if !exists {
		g.sendError(client, fmt.Sprintf("session '%s' not found", msg.Session))
		return
	}

	tmuxName := sessionName(msg.Session)

	// Auto-start if not running
	if !tmuxSessionExists(tmuxName) {
		workDir := info.Path
		if _, err := os.Stat(workDir); os.IsNotExist(err) {
			os.MkdirAll(workDir, 0755)
		}
		if err := createTmuxSession(tmuxName, workDir, false); err != nil {
			g.sendError(client, fmt.Sprintf("failed to start session: %v", err))
			return
		}
		time.Sleep(3 * time.Second)
	}

	// Persist original, enrich before sending to Claude
	persistMessage(msg.Session, "user", msg.Content, "websocket")
	enriched := enrichMessage(msg.Content)
	if err := sendToTmux(tmuxName, enriched); err != nil {
		g.sendError(client, fmt.Sprintf("failed to send to tmux: %v", err))
		return
	}

	// Echo back to confirm
	g.broadcast(&GatewayMessage{
		Type:    "message",
		Session: msg.Session,
		Role:    "user",
		Content: msg.Content,
		Channel: "websocket",
	})
}

func (g *Gateway) handleCommand(client *wsClient, msg *GatewayMessage) {
	config, err := loadConfig()
	if err != nil {
		g.sendError(client, "failed to load config")
		return
	}

	switch msg.Action {
	case "list":
		var sessions []GatewayMessage
		for name := range config.Sessions {
			tmuxName := sessionName(name)
			state := "stopped"
			if tmuxSessionExists(tmuxName) {
				state = "active"
			}
			sessions = append(sessions, GatewayMessage{
				Type:    "status",
				Session: name,
				State:   state,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{
			"type":     "sessions",
			"sessions": sessions,
		})
		client.conn.Write(client.ctx, websocket.MessageText, data)

	case "new":
		if msg.Session == "" {
			g.sendError(client, "session name required")
			return
		}
		if err := createSession(config, msg.Session); err != nil {
			g.sendError(client, fmt.Sprintf("create failed: %v", err))
			return
		}
		g.broadcast(&GatewayMessage{
			Type:    "status",
			Session: msg.Session,
			State:   "active",
		})

	case "restart":
		if msg.Session == "" {
			g.sendError(client, "session name required")
			return
		}
		tmuxName := sessionName(msg.Session)
		if tmuxSessionExists(tmuxName) {
			killTmuxSession(tmuxName)
			time.Sleep(300 * time.Millisecond)
		}
		info, exists := config.Sessions[msg.Session]
		if !exists {
			g.sendError(client, fmt.Sprintf("session '%s' not found", msg.Session))
			return
		}
		if err := createTmuxSession(tmuxName, info.Path, false); err != nil {
			g.sendError(client, fmt.Sprintf("restart failed: %v", err))
			return
		}
		g.broadcast(&GatewayMessage{
			Type:    "status",
			Session: msg.Session,
			State:   "active",
		})

	case "kill":
		if msg.Session == "" {
			g.sendError(client, "session name required")
			return
		}
		tmuxName := sessionName(msg.Session)
		if tmuxSessionExists(tmuxName) {
			killTmuxSession(tmuxName)
		}
		g.broadcast(&GatewayMessage{
			Type:    "status",
			Session: msg.Session,
			State:   "stopped",
		})

	default:
		g.sendError(client, fmt.Sprintf("unknown action: %s", msg.Action))
	}
}

// --- REST endpoints ---

func (g *Gateway) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !g.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	config, err := loadConfig()
	if err != nil {
		http.Error(w, "failed to load config", http.StatusInternalServerError)
		return
	}

	type sessionStatus struct {
		Name  string `json:"name"`
		State string `json:"state"`
		Path  string `json:"path"`
	}

	var sessions []sessionStatus
	for name, info := range config.Sessions {
		state := "stopped"
		if tmuxSessionExists(sessionName(name)) {
			state = "active"
		}
		path := ""
		if info != nil {
			path = info.Path
		}
		sessions = append(sessions, sessionStatus{
			Name:  name,
			State: state,
			Path:  path,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

func (g *Gateway) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	if !g.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract session name from /sessions/<name>/messages
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")
	if len(parts) < 2 || parts[1] != "messages" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sessionName := parts[0]

	if err := initStore(); err != nil {
		http.Error(w, "store unavailable", http.StatusInternalServerError)
		return
	}

	messages, err := store.GetMessages(sessionName, 100)
	if err != nil {
		http.Error(w, fmt.Sprintf("query error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	clientCount := len(g.clients)
	g.mu.RUnlock()

	health := map[string]interface{}{
		"status":  "ok",
		"version": version,
		"clients": clientCount,
		"time":    time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

// --- Internal hook endpoint ---

func (g *Gateway) handleHookPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Only accept from localhost
	if !strings.HasPrefix(r.RemoteAddr, "127.0.0.1") && !strings.HasPrefix(r.RemoteAddr, "[::1]") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxWSMessageSize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var payload HookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Broadcast to subscribed WebSocket clients
	g.broadcast(&GatewayMessage{
		Type:    "message",
		Session: payload.Session,
		Role:    payload.Role,
		Content: payload.Content,
		Channel: "claude",
	})

	// If it's a stop event, also send status
	if payload.Event == "stop" {
		g.broadcast(&GatewayMessage{
			Type:    "status",
			Session: payload.Session,
			State:   "stopped",
		})
	}

	w.WriteHeader(http.StatusOK)
}

// --- Broadcasting ---

func (g *Gateway) broadcast(msg *GatewayMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	for client := range g.clients {
		// Filter by session subscription
		if client.session != "" && client.session != msg.Session {
			continue
		}

		go func(c *wsClient) {
			ctx, cancel := context.WithTimeout(c.ctx, wsWriteTimeout)
			defer cancel()
			c.conn.Write(ctx, websocket.MessageText, data)
		}(client)
	}
}

func (g *Gateway) sendError(client *wsClient, errMsg string) {
	data, _ := json.Marshal(GatewayMessage{
		Type:  "error",
		Error: errMsg,
	})
	ctx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
	defer cancel()
	client.conn.Write(ctx, websocket.MessageText, data)
}

func (g *Gateway) sendSessionStatuses(client *wsClient) {
	config, err := loadConfig()
	if err != nil {
		return
	}

	for name := range config.Sessions {
		state := "stopped"
		if tmuxSessionExists(sessionName(name)) {
			state = "active"
		}
		msg := GatewayMessage{
			Type:    "status",
			Session: name,
			State:   state,
		}
		data, _ := json.Marshal(msg)
		ctx, cancel := context.WithTimeout(client.ctx, wsWriteTimeout)
		client.conn.Write(ctx, websocket.MessageText, data)
		cancel()
	}
}

// notifyGateway sends a hook event to the gateway's internal endpoint synchronously.
// Uses a short timeout so it doesn't block hooks if the gateway is down.
func notifyGateway(session, role, content, event string) {
	payload := HookPayload{
		Session: session,
		Role:    role,
		Content: content,
		Event:   event,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	client := &http.Client{Timeout: 1 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/hook", gatewayHookPort)
	resp, err := client.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		// Gateway might not be running, that's OK
		return
	}
	resp.Body.Close()
}
