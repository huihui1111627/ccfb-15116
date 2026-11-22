// Package web 提供模拟器的看板服务: 内嵌静态页面 + SSE 实时快照推送
// + 冻结/解冻控制接口。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"cgsim/internal/sim"
)

// Server 持有模拟引擎并对外暴露看板。
type Server struct {
	mu     sync.Mutex
	engine *sim.Engine
	mux    *http.ServeMux

	// SSE 订阅者。
	subMu sync.Mutex
	subs  map[chan sim.Snapshot]struct{}
}

// NewServer 包装一个已构建的引擎。
func NewServer(e *sim.Engine) *Server {
	s := &Server{
		engine: e,
		subs:   map[chan sim.Snapshot]struct{}{},
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	s.mux.HandleFunc("/api/events", s.handleSSE)
	s.mux.HandleFunc("/api/freeze", s.handleFreeze)
	s.mux.HandleFunc("/api/step", s.handleStep)
	return s
}

// Step 推进引擎一个 tick 并广播, 供看板单步按钮调用。
func (s *Server) Step() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.engine.Step(); err != nil {
		return err
	}
	s.broadcastLocked()
	return nil
}

// RunToEnd 推进到模拟结束 (CLI 的 headless 模式用)。
func (s *Server) RunToEnd() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.engine.Run(); err != nil {
		return err
	}
	s.broadcastLocked()
	return nil
}

// broadcastLocked 的无锁版本, 调用方必须持有 s.mu。
func (s *Server) broadcastLocked() {
	snap := s.engine.Snapshot(120)
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- snap:
		default:
		}
	}
}

// Handler 返回 HTTP 处理器。
func (s *Server) Handler() http.Handler { return s.mux }

// Broadcast 向所有 SSE 订阅者推送当前快照 (引擎每步后调用)。
func (s *Server) Broadcast() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcastLocked()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	snap := s.engine.Snapshot(120)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// handleSSE 用 Server-Sent Events 推送快照, 看板无需轮询。
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan sim.Snapshot, 4)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
		close(ch)
	}()

	// 连接建立先推一帧。
	s.Broadcast()
	for {
		select {
		case <-r.Context().Done():
			return
		case snap := <-ch:
			data, _ := json.Marshal(snap)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleFreeze 接收 POST {"group":"name","freeze":true}。
func (s *Server) handleFreeze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Group  string `json:"group"`
		Freeze bool   `json:"freeze"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	ok := s.engine.SetFrozen(req.Group, req.Freeze)
	snap := s.engine.Snapshot(120)
	s.mu.Unlock()
	if !ok {
		http.Error(w, "group not found or state unchanged", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// handleStep 推进一个模拟 tick。
func (s *Server) handleStep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := s.Step(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
