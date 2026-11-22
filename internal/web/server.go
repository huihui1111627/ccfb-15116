// Package web 提供模拟器的看板页面与 JSON 接口。
package web

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"

	"cgroupsim/internal/sim"
)

//go:embed index.html
var indexHTML []byte

// Server 包装模拟器,对外提供只读快照和冻结操作。
type Server struct {
	s  *sim.Sim
	mu *sync.Mutex
}

func New(s *sim.Sim, mu *sync.Mutex) *Server { return &Server{s: s, mu: mu} }

// Handler 返回路由。
func (sv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", sv.handleState)
	mux.HandleFunc("GET /api/history", sv.handleHistory)
	mux.HandleFunc("POST /api/freeze", sv.handleFreeze)
	return mux
}

type psiJSON struct {
	TotalUs int64   `json:"totalUs"`
	Avg10   float64 `json:"avg10"`
	Avg60   float64 `json:"avg60"`
	Avg300  float64 `json:"avg300"`
}

type groupJSON struct {
	Name        string  `json:"name"`
	Depth       int     `json:"depth"`
	Weight      int64   `json:"weight"`
	Frozen      bool    `json:"frozen"`
	EffFrozen   bool    `json:"effFrozen"`
	Tasks       int     `json:"tasks"`
	LiveTasks   int     `json:"liveTasks"`
	QuotaUs     int64   `json:"quotaUs"`
	PeriodUs    int64   `json:"periodUs"`
	PeriodUsed  int64   `json:"periodUsedUs"`
	Throttled   bool    `json:"throttled"`
	NrThrottled int64   `json:"nrThrottled"`
	ThrottledUs int64   `json:"throttledUs"`
	CpuTotalUs  int64   `json:"cpuTotalUs"`
	Entitlement float64 `json:"entitlement"`

	MemUsage  int64    `json:"memUsage"`
	MemAnon   int64    `json:"memAnon"`
	MemCache  int64    `json:"memCache"`
	MemLow    int64    `json:"memLow"`
	MemHigh   int64    `json:"memHigh"`
	MemMax    int64    `json:"memMax"`
	Reclaimed int64    `json:"reclaimedBytes"`
	Events    [5]int64 `json:"events"`

	PSI map[string]psiJSON `json:"psi"`
}

type stateJSON struct {
	NowUs   int64       `json:"nowUs"`
	NumCPUs int         `json:"numCpus"`
	Groups  []groupJSON `json:"groups"`
}

var resNames = [3]string{"cpu", "mem", "io"}
var kindNames = [2]string{"some", "full"}

func (sv *Server) handleState(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	st := stateJSON{NowUs: sv.s.Now, NumCPUs: sv.s.Cfg.NumCPUs}
	depths := map[*sim.Cgroup]int{}
	for _, g := range sv.s.Groups() {
		d := 0
		if g.Parent != nil {
			d = depths[g.Parent] + 1
		}
		depths[g] = d
		gj := groupJSON{
			Name: g.Name, Depth: d, Weight: g.Weight,
			Frozen: g.Frozen, EffFrozen: effFrozen(sv.s, g),
			Tasks:   len(g.Tasks),
			QuotaUs: g.QuotaUs, PeriodUs: g.PeriodUs,
			PeriodUsed: g.PeriodUsage(), Throttled: g.ThrottledNow(),
			NrThrottled: g.NrThrottled, ThrottledUs: g.ThrottledUs,
			CpuTotalUs: g.SubtreeCpuUs, Entitlement: g.Entitlement(),
			MemUsage: g.Usage(), MemAnon: g.TotalAnon, MemCache: g.TotalCache,
			MemLow: g.MemLow, MemHigh: g.MemHigh, MemMax: g.MemMax,
			Reclaimed: g.ReclaimedBytes, Events: g.Events,
			PSI: map[string]psiJSON{},
		}
		for _, tk := range g.Tasks {
			if !tk.Dead {
				gj.LiveTasks++
			}
		}
		for res := 0; res < 3; res++ {
			for kind := 0; kind < 2; kind++ {
				a10, a60, a300 := g.PSIAvgs(res, kind)
				gj.PSI[resNames[res]+"_"+kindNames[kind]] = psiJSON{
					TotalUs: g.PSITotal(res, kind),
					Avg10:   a10, Avg60: a60, Avg300: a300,
				}
			}
		}
		st.Groups = append(st.Groups, gj)
	}
	writeJSON(w, st)
}

func effFrozen(s *sim.Sim, g *sim.Cgroup) bool {
	for c := g; c != nil; c = c.Parent {
		if c.Frozen {
			return true
		}
	}
	return false
}

type historyJSON struct {
	T      []int64                         `json:"t"`
	Groups map[string]map[string][]float64 `json:"groups"`
}

func (sv *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	h := historyJSON{T: sv.s.PSIHistT(), Groups: map[string]map[string][]float64{}}
	for _, g := range sv.s.Groups() {
		m := map[string][]float64{}
		for res := 0; res < 3; res++ {
			for kind := 0; kind < 2; kind++ {
				m[resNames[res]+"_"+kindNames[kind]] = g.PSIHistAvg10(res, kind)
			}
		}
		h.Groups[g.Name] = m
	}
	writeJSON(w, h)
}

func (sv *Server) handleFreeze(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	on := r.URL.Query().Get("frozen") == "1"
	sv.mu.Lock()
	defer sv.mu.Unlock()
	g := sv.s.FindGroup(name)
	if g == nil {
		http.Error(w, "group not found", 404)
		return
	}
	sv.s.Freeze(g, on)
	writeJSON(w, map[string]any{"ok": true, "name": name, "frozen": on})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
