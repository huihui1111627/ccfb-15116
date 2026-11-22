// Package sim 实现一个纯用户态、确定性的控制组 (cgroup v2 风格) 资源
// 控制模拟器: CPU 权重/带宽/配额分片、内存三档回收与 OOM、层级 PSI
// 压力指标与冻结器, 全部用整数 tick 推进, 不依赖宿主机内核能力。
package sim

import (
	"errors"
	"fmt"
)

// Config 是引擎参数。数值都带明确的模拟语义, 不做墙钟换算。
type Config struct {
	CPUs           int     // 处理器数量
	Ticks          int64   // 总模拟 tick 数 (1 tick = 1ms)
	ReclaimRate    int64   // 内存回收速率: 每 tick 能回收的字节数
	DefaultSlice   int64   // 配额分片默认大小 (ms·CPU)
	DefaultReturn  int64   // 本地池分片默认归还延时 (tick)
	AnonReclaimPct float64 // 匿名内存压缩可回收比例 (0~1)
	RecordRaw      bool    // 保存逐 tick 原始停滞记录 (测试离线重算用)
	HistoryMax     int     // 每组 PSI 历史点上限, 0 表示不限
}

// DefaultConfig 给出与说明文档一致的默认参数。
func DefaultConfig() Config {
	return Config{
		CPUs:           4,
		Ticks:          60_000,
		ReclaimRate:    8192,
		DefaultSlice:   5,
		DefaultReturn:  20,
		AnonReclaimPct: 0.3,
	}
}

// Engine 是模拟引擎。
type Engine struct {
	cfg Config
	scn Scenario
	now int64
	rng *rng

	root        *Group
	groups      []*Group
	groupByName map[string]*Group
	tasks       []*Task
	taskByID    map[int]*Task

	// 按 tick 索引的脚本/控制事件。
	taskEvents    map[int64][]scheduledTaskEvent
	controlEvents map[int64][]ControlEvent

	events   []LogEvent
	finished bool
	taskBuf  []*Task
}

type scheduledTaskEvent struct {
	taskID int
	ev     TaskEvent
}

// New 校验场景并构建引擎的运行时层级结构。
func New(scn Scenario, cfg Config, seed uint64) (*Engine, error) {
	if err := validate(scn, cfg); err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:           cfg,
		scn:           scn,
		rng:           newRNG(seed),
		groupByName:   map[string]*Group{},
		taskByID:      map[int]*Task{},
		taskEvents:    map[int64][]scheduledTaskEvent{},
		controlEvents: map[int64][]ControlEvent{},
	}
	if err := e.buildGroups(); err != nil {
		return nil, err
	}
	if err := e.buildTasks(); err != nil {
		return nil, err
	}
	e.indexEvents()
	return e, nil
}

func validate(scn Scenario, cfg Config) error {
	if cfg.CPUs <= 0 {
		return errors.New("config: CPUs must be positive")
	}
	if cfg.Ticks <= 0 {
		return errors.New("config: Ticks must be positive")
	}
	if cfg.ReclaimRate <= 0 {
		return errors.New("config: ReclaimRate must be positive")
	}
	if cfg.DefaultSlice <= 0 {
		return errors.New("config: DefaultSlice must be positive")
	}
	if cfg.DefaultReturn <= 0 {
		return errors.New("config: DefaultReturn must be positive")
	}
	if cfg.AnonReclaimPct <= 0 || cfg.AnonReclaimPct > 1 {
		return errors.New("config: AnonReclaimPct must be in (0,1]")
	}
	for k, w := range scn.PSIWindows {
		if w <= 0 {
			return fmt.Errorf("scenario: PSI window %d must be positive", k)
		}
	}
	if scn.PSIFlush <= 0 {
		return errors.New("scenario: PSIFlush must be positive")
	}
	return nil
}

func (e *Engine) buildGroups() error {
	// 先全部实例化。
	for i, gs := range e.scn.Groups {
		if _, dup := e.groupByName[gs.Name]; dup {
			return fmt.Errorf("scenario: duplicate group name %q", gs.Name)
		}
		if gs.Weight <= 0 {
			return fmt.Errorf("scenario: group %q weight must be positive", gs.Name)
		}
		g := &Group{
			ID:          i,
			Spec:        gs,
			periodStart: 0,
			poolByCPU:   map[int]*localPool{},
			psi:         newPSI(e.scn.PSIWindows, e.scn.PSIFlush, e.cfg.RecordRaw),
		}
		if gs.Quota > 0 {
			g.quotaLeft = gs.Quota
		}
		e.groupByName[gs.Name] = g
		e.groups = append(e.groups, g)
	}
	// 再连边。
	var root *Group
	for _, g := range e.groups {
		if g.Spec.Parent == "" {
			if root != nil {
				return errors.New("scenario: multiple root groups")
			}
			root = g
			continue
		}
		p, ok := e.groupByName[g.Spec.Parent]
		if !ok {
			return fmt.Errorf("scenario: group %q references unknown parent %q", g.Spec.Name, g.Spec.Parent)
		}
		g.Parent = p
		p.Children = append(p.Children, g)
	}
	if root == nil {
		return errors.New("scenario: no root group")
	}
	// 防止成环: DFS 计数。
	seen := map[*Group]bool{}
	var check func(*Group) error
	check = func(g *Group) error {
		if seen[g] {
			return fmt.Errorf("scenario: cycle detected at %q", g.Spec.Name)
		}
		seen[g] = true
		for _, c := range g.Children {
			if err := check(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(root); err != nil {
		return err
	}
	if len(seen) != len(e.groups) {
		return errors.New("scenario: some groups are not reachable from root")
	}
	e.root = root
	return nil
}

func (e *Engine) buildTasks() error {
	for _, ts := range e.scn.Tasks {
		g, ok := e.groupByName[ts.Group]
		if !ok {
			return fmt.Errorf("scenario: task %d references unknown group %q", ts.ID, ts.Group)
		}
		if _, dup := e.taskByID[ts.ID]; dup {
			return fmt.Errorf("scenario: duplicate task id %d", ts.ID)
		}
		// 事件必须按 tick 升序。
		for i := 1; i < len(ts.Events); i++ {
			if ts.Events[i].Tick < ts.Events[i-1].Tick {
				return fmt.Errorf("scenario: task %d events not sorted", ts.ID)
			}
		}
		t := &Task{
			ID:    ts.ID,
			Name:  ts.Name,
			Group: g,
			state: stSleeping,
			alive: true,
		}
		g.Tasks = append(g.Tasks, t)
		e.tasks = append(e.tasks, t)
		e.taskByID[ts.ID] = t
	}
	return nil
}

func (e *Engine) indexEvents() {
	for _, ts := range e.scn.Tasks {
		for _, ev := range ts.Events {
			e.taskEvents[ev.Tick] = append(e.taskEvents[ev.Tick], scheduledTaskEvent{taskID: ts.ID, ev: ev})
		}
	}
	for _, c := range e.scn.Controls {
		e.controlEvents[c.Tick] = append(e.controlEvents[c.Tick], c)
	}
}

// Now 返回当前模拟 tick。
func (e *Engine) Now() int64 { return e.now }

// Events 返回事件日志的拷贝。
func (e *Engine) Events() []LogEvent {
	out := make([]LogEvent, len(e.events))
	copy(out, e.events)
	return out
}

// Run 跑到配置的总 tick 数并收尾 (partial flush + 完成标记),
// 对应需求里的 "统计完再交"。
func (e *Engine) Run() error {
	for e.now < e.cfg.Ticks {
		if err := e.Step(); err != nil {
			return err
		}
	}
	e.finalize()
	return nil
}

func (e *Engine) finalize() {
	if e.finished {
		return
	}
	// 收尾: 把最后一个不完整 flush 区间也刷出去, 保证看板和测试拿到
	// 全部原始停滞记录对应的 EWMA。
	for _, g := range e.groups {
		g.psi.maybeFlush(e.now, true)
	}
	e.finished = true
}

// Step 推进一个 tick。
func (e *Engine) Step() error {
	if e.finished {
		return errors.New("engine: already finished")
	}
	if e.now >= e.cfg.Ticks {
		e.finalize()
		return nil
	}
	e.applyControls()
	e.applyTaskEvents()
	e.handlePeriods()
	e.handlePoolReturns()
	e.scheduleCPU()
	e.handleMemoryAndIO()
	e.accountPressure()
	e.tickThrottleClocks()
	e.now++
	if e.now >= e.cfg.Ticks {
		e.finalize()
	}
	return nil
}
