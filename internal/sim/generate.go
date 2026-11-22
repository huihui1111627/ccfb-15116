package sim

// GenConfig 控制负载生成器的形状。
type GenConfig struct {
	CPUs         int
	Ticks        int64
	Seed         uint64
	Depth        int // 树深度 (根为第 0 层)
	MaxChildren  int // 每个内部组的最大子组数
	TasksPerLeaf int // 每个叶子组的任务数上下界
	MinTasks     int
	MaxTasks     int
	Windows      [3]int64
	Flush        int64
}

// DefaultGenConfig 给出与引擎默认参数匹配的生成配置。
func DefaultGenConfig(seed uint64) GenConfig {
	return GenConfig{
		CPUs: 4, Ticks: 20_000, Seed: seed,
		Depth: 3, MaxChildren: 3,
		TasksPerLeaf: 3, MinTasks: 2, MaxTasks: 4,
		Windows: [3]int64{10_000, 60_000, 300_000},
		Flush:   1_000,
	}
}

type genCtx struct {
	rng     *rng
	cfg     GenConfig
	groups  []GroupSpec
	tasks   []TaskSpec
	control []ControlEvent
	taskSeq int
}

// Generate 用固定种子生成一棵控制组树和几十种行为混合的任务负载。
// 同 seed 永远产出同一份 Scenario, 供测试逐 tick 复现。
func Generate(cfg GenConfig) Scenario {
	gx := &genCtx{rng: newRNG(cfg.Seed), cfg: cfg}

	// 根组: 不设带宽限额, 内存给一个很宽的硬上限。
	root := GroupSpec{
		Name: "root", Weight: 100,
		MemHard: 8 << 30, MemSoft: 6 << 30, MemLow: 1 << 30,
	}
	gx.groups = append(gx.groups, root)
	gx.buildLevel("root", 1)

	// 给每个叶子组生成混合任务。
	leaves := gx.leafGroups()
	for _, name := range leaves {
		n := cfg.MinTasks
		if cfg.MaxTasks > cfg.MinTasks {
			n += gx.rng.intn(cfg.MaxTasks - cfg.MinTasks + 1)
		}
		for i := 0; i < n; i++ {
			gx.genTask(name)
		}
	}

	// 少量冻结/解冻控制事件, 覆盖需求 8。
	if len(leaves) > 0 {
		fg := leaves[gx.rng.intn(len(leaves))]
		freezeAt := int64(gx.rng.intBetween(int(cfg.Ticks/5), int(cfg.Ticks/3)))
		thawAt := freezeAt + int64(gx.rng.intBetween(500, 2000))
		gx.control = append(gx.control,
			ControlEvent{Tick: freezeAt, Group: fg, Freeze: true},
			ControlEvent{Tick: thawAt, Group: fg, Freeze: false},
		)
	}

	return Scenario{
		Groups: gx.groups, Tasks: gx.tasks, Controls: gx.control,
		PSIWindows: cfg.Windows, PSIFlush: cfg.Flush,
	}
}

func (gx *genCtx) buildLevel(parent string, depth int) {
	if depth > gx.cfg.Depth {
		return
	}
	n := 1 + gx.rng.intn(gx.cfg.MaxChildren)
	for i := 0; i < n; i++ {
		name := parent + "_" + string(rune('a'+i))
		spec := GroupSpec{
			Name: name, Parent: parent,
			Weight: int64(gx.rng.intBetween(20, 200)),
		}
		leaf := depth == gx.cfg.Depth
		gx.decorateLimits(&spec, leaf)
		gx.groups = append(gx.groups, spec)
		gx.buildLevel(name, depth+1)
	}
}

// decorateLimits 随机但确定地给组挂带宽与内存限额, 保证模拟中
// 节流、回收、OOM 三类触发器都有机会出现。
func (gx *genCtx) decorateLimits(spec *GroupSpec, leaf bool) {
	// 约 60% 的组带 CFS 带宽限额。
	if gx.rng.chance(0.6) {
		period := int64(gx.rng.choice([]int{50, 100, 200}))
		// 配额: 10%~120% 的单 CPU 周期, 小配额组必然周期性节流。
		quota := period * int64(gx.rng.intBetween(10, 120)) / 100
		if quota < 1 {
			quota = 1
		}
		spec.Period = period
		spec.Quota = quota
		spec.Slice = int64(gx.rng.choice([]int{2, 5, 10}))
		spec.ReturnAfter = int64(gx.rng.choice([]int{10, 20, 50}))
	}
	// 内存三档。叶子组承载实际任务, 给得足够紧以制造回收与 OOM;
	// 内部组限额放宽, 主要承担层级聚合语义。
	var base int64
	if leaf {
		base = 4 << 20 // 4 MiB 级别的紧限额, 任务申请可达数十 MiB
	} else {
		base = 64 << 20
	}
	soft := base * int64(gx.rng.intBetween(8, 16)) / 10
	// 硬上限只比软上限高半个 base, 且叶子组基数小, 大申请会直接撞墙。
	hard := soft + base/2
	spec.MemLow = soft / 3
	spec.MemSoft = soft
	spec.MemHard = hard
}

func (gx *genCtx) leafGroups() []string {
	hasChild := map[string]bool{}
	for _, g := range gx.groups {
		if g.Parent != "" {
			hasChild[g.Parent] = true
		}
	}
	var leaves []string
	for _, g := range gx.groups {
		if !hasChild[g.Name] {
			leaves = append(leaves, g.Name)
		}
	}
	return leaves
}

// genTask 生成一个任务的完整事件脚本, 混合六种行为:
// CPU 突发、空闲睡眠、匿名/文件内存申请与释放、IO 阻塞、退出。
func (gx *genCtx) genTask(group string) {
	gx.taskSeq++
	id := gx.taskSeq
	t := TaskSpec{ID: id, Name: "task-" + itoa(id), Group: group}

	tick := int64(0)
	// 开场即进入运行态。
	t.Events = append(t.Events, TaskEvent{Tick: 0, Type: EvtRun})
	behavior := gx.rng.intn(6) // 决定这个任务的 "性格"

	for tick < gx.cfg.Ticks {
		roll := gx.rng.float64()
		switch {
		case behavior == 0 || roll < 0.45:
			// CPU 运行段: 推进 tick, 在新 tick 重新声明 run 状态。
			tick += int64(gx.rng.intBetween(50, 1500))
			t.Events = append(t.Events, TaskEvent{Tick: tick, Type: EvtRun})
		case roll < 0.75:
			// 内存申请 (与释放), 全部安排在当前 tick, 不导致回退。
			// 叶子组硬上限只有数 MiB, 这里允许到 16 MiB, 制造 OOM。
			size := int64(gx.rng.intBetween(1, 4096)) * PageSize
			kind := EvtAllocAnon
			if gx.rng.chance(0.5) {
				kind = EvtAllocFile
			}
			t.Events = append(t.Events, TaskEvent{Tick: tick, Type: kind, Bytes: size})
			if gx.rng.chance(0.6) {
				t.Events = append(t.Events, TaskEvent{Tick: tick, Type: EvtFree, Bytes: size})
			}
			// 下一轮必须推进 tick, 避免事件堆积。
			tick += int64(gx.rng.intBetween(10, 200))
		default:
			// IO 阻塞段: 当前 tick 发起, tick 推进后安排唤醒。
			ioFor := int64(gx.rng.intBetween(20, 400))
			t.Events = append(t.Events, TaskEvent{Tick: tick, Type: EvtIO, Bytes: ioFor})
			tick += ioFor
			t.Events = append(t.Events, TaskEvent{Tick: tick, Type: EvtRun})
		}
		// 少量睡眠段, 制造非满负载窗口。
		if gx.rng.chance(0.15) {
			tick += int64(gx.rng.intBetween(100, 1000))
			t.Events = append(t.Events, TaskEvent{Tick: tick, Type: EvtRun})
		}
	}
	// 约三分之一的任务提前退出, 覆盖死任务路径。退出 tick 必须严格晚于
	// 最后一个已生成事件, 保证整个事件序列非降序。
	if gx.rng.chance(0.33) && len(t.Events) > 0 {
		exitAt := int64(gx.rng.intBetween(int(gx.cfg.Ticks/2), int(gx.cfg.Ticks)-1))
		if last := t.Events[len(t.Events)-1].Tick; exitAt <= last {
			exitAt = last + 1
		}
		if exitAt < gx.cfg.Ticks {
			t.Events = append(t.Events, TaskEvent{Tick: exitAt, Type: EvtExit})
		}
	}
	gx.tasks = append(gx.tasks, t)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
