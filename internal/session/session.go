package session

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/creack/pty"
)

type State string

const (
	StateDiscovered State = "discovered"
	StateTakeover   State = "takeover"
	StateCreated    State = "created"
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateWaiting    State = "waiting"
	StateCompleted  State = "completed"
	StateErrored    State = "errored"
	StateOffline    State = "offline"
)

type Session struct {
	mu sync.RWMutex

	ID                string
	Provider          string
	ExternalSessionID string
	ClaudeID          string
	Name              string
	WorkDir           string
	State             State
	PID               int
	StartTime         time.Time
	EndTime           time.Time
	ExitCode          int
	Error             string
	Owned             bool
	Killed            bool   // true if intentionally killed by user
	TmuxSession       string // tmux session name (e.g. "ws-myproject")
	Sandboxed         bool   // running inside Docker Desktop sandbox VM
	SandboxName       string // docker sandbox name (e.g. "ws-myproject")

	readerPTY *os.File // PTY for the tmux attach reader (for resize)
	output    *RingBuf
}

var validTransitions = map[State][]State{
	StateDiscovered: {StateTakeover},
	StateTakeover:   {StateRunning, StateErrored},
	StateCreated:    {StateRunning, StateStarting},
	StateStarting:   {StateRunning, StateErrored},
	StateRunning:    {StateWaiting, StateCompleted, StateErrored},
	StateWaiting:    {StateRunning, StateCompleted, StateErrored},
}

func (s *Session) Transition(to State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed, ok := validTransitions[s.State]
	if !ok {
		return fmt.Errorf("no transitions from state %s", s.State)
	}
	for _, valid := range allowed {
		if valid == to {
			s.State = to
			return nil
		}
	}
	return fmt.Errorf("invalid transition: %s -> %s", s.State, to)
}

func (s *Session) GetState() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.State
}

func (s *Session) GetError() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Error
}

func (s *Session) Output() *RingBuf {
	return s.output
}

// Resize resizes the tmux window and the reader PTY for this session.
func (s *Session) Resize(rows, cols uint16) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.TmuxSession == "" {
		return fmt.Errorf("no tmux session attached")
	}
	// Resize the reader PTY first (so tmux sees the new client size)
	if s.readerPTY != nil {
		_ = pty.Setsize(s.readerPTY, &pty.Winsize{Rows: rows, Cols: cols})
	}
	// Then resize the tmux window
	return tmuxResizeWindow(s.TmuxSession, int(cols), int(rows))
}

// SetReaderPTY stores the reader PTY file for resize operations.
func (s *Session) SetReaderPTY(f *os.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readerPTY = f
}
