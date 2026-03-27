package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/IgorDeo/claude-websessions/internal/config"
	"github.com/IgorDeo/claude-websessions/internal/notification"
	"github.com/IgorDeo/claude-websessions/internal/session"
	"github.com/IgorDeo/claude-websessions/internal/store"
	"github.com/IgorDeo/claude-websessions/web"
	"github.com/go-chi/chi/v5"
)

type SnoozeFunc func(sessionID string, minutes int)

type Server struct {
	cfg      *config.Config
	mgr      *session.Manager
	bus      *notification.Bus
	sink     *notification.InAppSink
	sound    *notification.SoundSink
	store    *store.Store
	hub      *wsHub
	handler  http.Handler
	snoozeFn SnoozeFunc
	version  string
}

func (s *Server) SetVersion(v string) { s.version = v }

func (s *Server) SetSnoozeFunc(fn SnoozeFunc) { s.snoozeFn = fn }

func New(cfg *config.Config, mgr *session.Manager, bus *notification.Bus, sink *notification.InAppSink, st ...*store.Store) *Server {
	soundSink := notification.NewSoundSink(cfg.Notifications.Sound, cfg.Notifications.AudioDevice)
	s := &Server{cfg: cfg, mgr: mgr, bus: bus, sink: sink, sound: soundSink, hub: newWSHub()}
	if len(st) > 0 {
		s.store = st[0]
	}
	s.handler = s.routes()
	s.setupNotificationBridge()
	mgr.OnOutput(func(sessionID string, data []byte) { s.hub.broadcast(sessionID, data) })
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Addr() string {
	return fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	// Static files
	staticFS, _ := fs.Sub(web.Static, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Pages
	r.Get("/", s.handleIndex)
	r.Get("/settings", s.handleSettings)
	r.Post("/settings", s.handleSaveSettings)
	r.Post("/settings/hooks", s.handleInstallHooks)
	r.Get("/api/check-update", s.handleCheckUpdate)
	r.Post("/api/self-update", s.handleSelfUpdate)
	r.Post("/api/kill-all", s.handleKillAll)
	r.Get("/api/preferences", s.handleGetPreferences)
	r.Put("/api/preferences", s.handleSetPreference)
	r.Get("/api/audio-devices", s.handleAudioDevices)
	r.Post("/api/audio-device", s.handleSetAudioDevice)
	r.Post("/api/test-sound", s.handleTestSound)
	r.Post("/settings/service", s.handleService)
	r.Get("/sidebar", s.handleSidebar)
	r.Get("/notifications", s.handleNotifications)

	// APIs
	r.Post("/api/hook", s.handleHookCallback)
	r.Get("/api/dirs", s.handleListDirs)
	r.Get("/api/recent", s.handleRecentProjects)
	r.Get("/api/sessions", s.handleListSessions)
	r.Get("/api/provider-sessions", s.handleProviderSessions)
	r.Get("/api/claude-sessions", s.handleClaudeSessions)

	// Sessions
	r.Get("/sessions/new", s.handleNewSessionModal)
	r.Post("/sessions", s.handleCreateSession)
	r.Post("/sessions/terminal", s.handleCreateTerminal)
	r.Post("/sessions/{sessionID}/open", func(w http.ResponseWriter, r *http.Request) {
		s.handleOpenSession(w, r, chi.URLParam(r, "sessionID"))
	})
	r.Post("/sessions/{sessionID}/rename", func(w http.ResponseWriter, r *http.Request) {
		s.handleRenameSession(w, r, chi.URLParam(r, "sessionID"))
	})
	r.Get("/sessions/{sessionID}/diff", func(w http.ResponseWriter, r *http.Request) {
		s.handleGitDiff(w, r, chi.URLParam(r, "sessionID"))
	})
	r.Post("/sessions/{sessionID}/kill", func(w http.ResponseWriter, r *http.Request) {
		s.handleKillSession(w, r, chi.URLParam(r, "sessionID"))
	})
	r.Post("/sessions/{sessionID}/restart", func(w http.ResponseWriter, r *http.Request) {
		s.handleRestartSession(w, r, chi.URLParam(r, "sessionID"))
	})
	r.Post("/sessions/{sessionID}/takeover", func(w http.ResponseWriter, r *http.Request) {
		s.handleTakeover(w, r, chi.URLParam(r, "sessionID"))
	})

	// Iframe panes
	r.Post("/panes/iframe/open", s.handleOpenIframe)
	r.Post("/api/panes/iframe", s.handleCreateIframePane)
	r.Post("/settings/plannotator", s.handlePlannotatorIntegration)

	// WebSocket
	r.Get("/ws/notifications", s.handleNotificationWS)
	r.Get("/ws/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
		s.handleWS(w, r, chi.URLParam(r, "sessionID"), s.mgr)
	})
	r.Post("/notifications/snooze", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			SessionID string `json:"session_id"`
			Minutes   int    `json:"minutes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if payload.Minutes <= 0 {
			payload.Minutes = 15
		}
		if s.snoozeFn != nil {
			s.snoozeFn(payload.SessionID, payload.Minutes)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "minutes": payload.Minutes})
	})
	r.Post("/notifications/clear", func(w http.ResponseWriter, r *http.Request) {
		if s.store != nil {
			_ = s.store.MarkAllNotificationsRead()
		}
		s.sink.Clear()
		w.WriteHeader(http.StatusOK)
	})
	r.Post("/notifications/{id}/read", func(w http.ResponseWriter, r *http.Request) {
		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if s.store != nil {
			_ = s.store.MarkNotificationRead(id)
		}
		w.WriteHeader(http.StatusOK)
	})
	return r
}
