package sim

// TaskState 任务状态(互斥)。
type TaskState int

const (
	StRunning  TaskState = iota // 本 tick 在处理器上运行
	StRunnable                  // 想要处理器但没排到(等 CPU)
	StMem                       // 被迫做直接回收/被软上限拖慢(等内存)
	StIO                        // 等读写
	StSleep                     // 睡眠(不计入全部停滞的分母)
	StFrozen                    // 所在组被冻结(不计入压力)
	StDead                      // 已被 OOM 杀掉
)

// PhaseKind 负载阶段类型。
type PhaseKind int

const (
	PhaseCPU PhaseKind = iota
	PhaseIO
	PhaseSleep
)

// Phase 是任务程序中的一个阶段。
type Phase struct {
	Kind     PhaseKind
	DurUs    int64   // 阶段时长
	AnonBps  float64 // CPU 阶段运行时每微秒分配的匿名页字节数
	CacheBps float64 // CPU 阶段运行时每微秒产生的页缓存字节数
	FreeFrac float64 // 阶段结束时释放自身匿名页的比例
}

// Task 是一个模拟线程,循环执行 prog 中的阶段序列。
type Task struct {
	ID    int
	Name  string
	Group *Cgroup

	state    TaskState
	prog     []Phase
	pi       int
	remainUs int64

	Anon int64 // 当前持有的匿名页

	pendAnon   int64 // 被软上限挡住、待解除后补上的分配
	pendCache  int64
	blocker    *Cgroup // 当前挡路的软上限组(等待计数挂在它上面)
	stallSince int64   // 进入内存停滞的时刻(停滞至少持续一个 tick 才会解除)

	CPUTimeUs int64
	StallUs   [NRes]int64 // 任务级停滞累计(供抽查)

	Dead      bool
	RespawnAt int64

	ranTick bool // 本 tick 是否真的跑过(阶段切换当 tick 仍算运行)
}

// AddTask 在叶子组里加一个任务,程序 prog 会被循环执行。
func (s *Sim) AddTask(g *Cgroup, name string, prog []Phase) *Task {
	tk := &Task{
		ID:    s.nextTaskID,
		Name:  name,
		Group: g,
		prog:  prog,
	}
	s.nextTaskID++
	tk.enterPhase(0)
	g.Tasks = append(g.Tasks, tk)
	s.Tasks = append(s.Tasks, tk)
	return tk
}

func (tk *Task) enterPhase(i int) {
	tk.pi = i
	ph := tk.prog[i]
	tk.remainUs = ph.DurUs
	switch ph.Kind {
	case PhaseCPU:
		tk.state = StRunnable
	case PhaseIO:
		tk.state = StIO
	case PhaseSleep:
		tk.state = StSleep
	}
}

// advancePhase 结束当前阶段(按 FreeFrac 释放内存)并进入下一阶段。
func (s *Sim) advancePhase(tk *Task) {
	ph := tk.prog[tk.pi]
	if ph.FreeFrac > 0 && tk.Anon > 0 {
		f := int64(float64(tk.Anon) * ph.FreeFrac)
		if f > 0 {
			tk.Anon -= f
			s.addAnon(tk.Group, -f)
		}
	}
	tk.enterPhase((tk.pi + 1) % len(tk.prog))
}

// reset 复活任务(用于演示模式的 OOM 重生)。
func (tk *Task) reset() {
	tk.Dead = false
	tk.Anon = 0
	tk.pendAnon, tk.pendCache = 0, 0
	tk.enterPhase(0)
}

// effState 返回计入压力统计时的有效状态(冻结优先)。
func (s *Sim) effState(tk *Task) TaskState {
	if tk.Dead {
		return StDead
	}
	if tk.ranTick {
		return StRunning
	}
	if s.effFrozen(tk.Group) {
		return StFrozen
	}
	return tk.state
}

// updateTask 处理任务计时:IO/睡眠倒计时、运行任务的内存分配尝试、OOM 重生。
func (s *Sim) updateTask(tk *Task, t int64) {
	if tk.Dead {
		if s.Respawn && t >= tk.RespawnAt {
			tk.reset()
		}
		return
	}
	if s.effFrozen(tk.Group) {
		return // 冻结期间计时全部暂停
	}
	switch tk.state {
	case StIO, StSleep:
		tk.remainUs -= s.Cfg.TickUs
		if tk.remainUs <= 0 {
			s.advancePhase(tk)
		}
	case StRunning:
		// 上一 tick 在跑,本 tick 先尝试这一 tick 的内存分配;
		// 被软上限挡住则转入内存停滞,否则回到可运行队列。
		s.tryAllocTick(tk, t)
		if tk.state == StRunning {
			tk.state = StRunnable
		}
	}
}

// tryAllocTick 为运行中的任务尝试本 tick 的内存分配。
func (s *Sim) tryAllocTick(tk *Task, t int64) {
	ph := tk.prog[tk.pi]
	anon := int64(ph.AnonBps * float64(s.Cfg.TickUs))
	cache := int64(ph.CacheBps * float64(s.Cfg.TickUs))
	if anon <= 0 && cache <= 0 {
		return
	}
	switch s.attemptAlloc(tk, anon, cache, t, false) {
	case allocStallHigh:
		tk.state = StMem
		tk.pendAnon, tk.pendCache = anon, cache
		tk.stallSince = t
		if tk.blocker != nil {
			tk.blocker.memWaiters++
		}
	case allocDead:
		// 任务自己在 OOM 中被选中,状态已置 StDead
	}
}
