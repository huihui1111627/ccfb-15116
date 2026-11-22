package sim

// Snapshot 是某一时刻整个引擎的可序列化视图。
type Snapshot struct {
	Tick       int64       `json:"tick"`
	TotalTicks int64       `json:"total_ticks"`
	CPUs       int         `json:"cpus"`
	Finished   bool        `json:"finished"`
	Root       string      `json:"root"`
	Groups     []GroupSnap `json:"groups"`
	Tasks      []TaskSnap  `json:"tasks"`
	Events     []LogEvent  `json:"events"`
}

// GroupSnap 是控制组面板需要的全部字段。
type GroupSnap struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Parent   string   `json:"parent"`
	Children []string `json:"children"`
	Weight   int64    `json:"weight"`
	Frozen   bool     `json:"frozen"`

	// CPU 带宽。
	Period      int64      `json:"period"`
	Quota       int64      `json:"quota"`
	QuotaLeft   int64      `json:"quota_left"`
	PeriodUsage int64      `json:"period_usage"`
	Throttled   bool       `json:"throttled"`
	ThrottleCnt int64      `json:"throttle_cnt"`
	ThrottleMs  int64      `json:"throttle_ms"`
	CPUUsed     float64    `json:"cpu_used"`
	LocalPools  []PoolSnap `json:"local_pools"`

	// 内存三档与回收。
	MemLow       int64 `json:"mem_low"`
	MemSoft      int64 `json:"mem_soft"`
	MemHard      int64 `json:"mem_hard"`
	MemCur       int64 `json:"mem_cur"`
	MemReclaimed int64 `json:"mem_reclaimed"`
	OOMKills     int64 `json:"oom_kills"`

	// 压力: 每个资源的 some/full 三窗口 EWMA (百分比) 与历史曲线。
	Pressure [3]PressureSnap `json:"pressure"`

	FrozenTicks int64 `json:"frozen_ticks"`
}

// PoolSnap 是单处理器本地池的状态。
type PoolSnap struct {
	CPU      int   `json:"cpu"`
	Local    int64 `json:"local"`
	Borrowed int64 `json:"borrowed"`
	IdleFor  int64 `json:"idle_for"`
}

// PressureSnap 是一类资源的压力视图。
type PressureSnap struct {
	Resource string     `json:"resource"`
	AvgSome  [3]float64 `json:"avg_some"` // 10s / 1m / 5m
	AvgFull  [3]float64 `json:"avg_full"`
	History  []PSIPoint `json:"history"`
}

// TaskSnap 是任务行视图。
type TaskSnap struct {
	ID      int     `json:"id"`
	Name    string  `json:"name"`
	Group   string  `json:"group"`
	State   string  `json:"state"`
	MemAnon int64   `json:"mem_anon"`
	MemFile int64   `json:"mem_file"`
	CPUUsed float64 `json:"cpu_used"`
	Stall   string  `json:"stall"`
}

// Snapshot 构建当前全量快照。PSI 曲线点数受 Config.HistoryMax 限制
// (0 表示不限)。
func (e *Engine) Snapshot(historyLimit int) Snapshot {
	if historyLimit == 0 {
		historyLimit = e.cfg.HistoryMax
	}
	snap := Snapshot{
		Tick:       e.now,
		TotalTicks: e.cfg.Ticks,
		CPUs:       e.cfg.CPUs,
		Finished:   e.finished,
		Root:       e.root.Spec.Name,
		Groups:     []GroupSnap{},
		Tasks:      []TaskSnap{},
		Events:     e.Events(),
	}
	for _, g := range e.groups {
		gs := GroupSnap{
			ID: g.ID, Name: g.Spec.Name, Weight: g.Spec.Weight,
			Frozen: g.frozenSelfOrAncestor(),
			Period: g.Spec.Period, Quota: g.Spec.Quota,
			QuotaLeft: g.quotaLeft, PeriodUsage: g.periodUsage,
			Throttled: g.throttled, ThrottleCnt: g.throttleCnt,
			ThrottleMs: g.throttleMs, CPUUsed: g.cpuUsed,
			MemLow: g.Spec.MemLow, MemSoft: g.Spec.MemSoft,
			MemHard: g.Spec.MemHard, MemCur: g.memCur,
			MemReclaimed: g.memReclaimed, OOMKills: g.oomKills,
			FrozenTicks: g.frozenTicks,
		}
		if g.Parent != nil {
			gs.Parent = g.Parent.Spec.Name
		}
		gs.Children = []string{}
		for _, c := range g.Children {
			gs.Children = append(gs.Children, c.Spec.Name)
		}
		gs.LocalPools = []PoolSnap{}
		for _, p := range g.pools {
			gs.LocalPools = append(gs.LocalPools, PoolSnap{
				CPU: p.cpu, Local: p.local, Borrowed: p.borrowed,
				IdleFor: e.now - p.lastUseTick,
			})
		}
		for r := 0; r < 3; r++ {
			ps := PressureSnap{Resource: resNames[r]}
			for k := 0; k < 3; k++ {
				ps.AvgSome[k] = g.psi.AvgSome[r][k]
				ps.AvgFull[k] = g.psi.AvgFull[r][k]
			}
			ps.History = g.psi.historyCopy(historyLimit)
			gs.Pressure[r] = ps
		}
		snap.Groups = append(snap.Groups, gs)
	}
	for _, t := range e.tasks {
		ts := TaskSnap{
			ID: t.ID, Name: t.Name, Group: t.Group.Spec.Name,
			State: stateName(t.state), MemAnon: t.Mem.anon,
			MemFile: t.Mem.file, CPUUsed: t.cpuUsed,
			Stall: stallName(t.stall),
		}
		snap.Tasks = append(snap.Tasks, ts)
	}
	return snap
}

func stateName(s taskState) string {
	switch s {
	case stRunning:
		return "running"
	case stSleeping:
		return "sleeping"
	case stMemWait:
		return "memwait"
	case stIOWait:
		return "iowait"
	case stDead:
		return "dead"
	default:
		return "unknown"
	}
}

func stallName(s Stall) string {
	if s == StallNone {
		return "none"
	}
	out := ""
	if s&StallCPU != 0 {
		out += "cpu,"
	}
	if s&StallMem != 0 {
		out += "mem,"
	}
	if s&StallIO != 0 {
		out += "io,"
	}
	return out[:len(out)-1]
}

// GroupByName 供外部 (web 控制接口/测试) 查找组。
func (e *Engine) GroupByName(name string) (*Group, bool) {
	g, ok := e.groupByName[name]
	return g, ok
}

// FrozenForTest 返回组当前的冻结标记 (含祖先冻结), 供外部包测试使用。
func (g *Group) FrozenForTest() bool { return g.frozenSelfOrAncestor() }

// ThrottledForTest 返回组当前节流态, 供外部包测试使用。
func (g *Group) ThrottledForTest() bool { return g.throttled }

// TaskByID 供测试查找任务。
func (e *Engine) TaskByID(id int) (*Task, bool) {
	t, ok := e.taskByID[id]
	return t, ok
}

// SetFrozen 是 web 控制接口: 整组冻结/解冻, 与脚本控制事件走同一语义。
func (e *Engine) SetFrozen(name string, freeze bool) bool {
	g, ok := e.groupByName[name]
	if !ok || g.frozen == freeze {
		return false
	}
	g.frozen = freeze
	evType := EvFreeze
	if !freeze {
		evType = EvUnfreeze
	}
	e.events = append(e.events, LogEvent{
		Tick: e.now, Type: evType, Group: name,
	})
	return true
}
