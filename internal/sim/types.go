package sim

// 时间与容量单位约定:
//   - 引擎按固定步长 tick 推进, 1 tick = 1 模拟毫秒 (TickMillis)
//   - CPU 时间以 "毫秒·CPU" 为单位: 一台 CPU 跑一个 tick 贡献 1ms 容量
//   - 内存以字节为单位, 页大小 PageSize = 4096
//   - 所有统计窗口 (周期/回收定时器/PSI 窗口) 都是 tick 的整数倍
const (
	TickMillis = 1
	PageSize   = 4096
)

// Stall 资源位, 用于按任务记录一个 tick 里在等什么。
type Stall uint8

const (
	StallNone Stall = 0
	StallCPU  Stall = 1 << iota // 等待处理器 (可运行但没拿到时间)
	StallMem                    // 等待内存回收
	StallIO                     // 等待读写
)

// 压力事件日志里的事件类型, 测试用它做 "不漏报不多报" 的触发器审计。
type EventType string

const (
	EvThrottle   EventType = "throttle"   // 组在周期内耗尽配额被摘下运行队列
	EvUnthrottle EventType = "unthrottle" // 下个周期开头重新放回
	EvReclaim    EventType = "reclaim"    // 触发回收 (软上限/分配压力)
	EvOOMKill    EventType = "oom_kill"   // 硬上限触发, 组内杀进程
	EvFreeze     EventType = "freeze"     // 整组冻结
	EvUnfreeze   EventType = "unfreeze"   // 整组解冻
)

// LogEvent 是一条带模拟时间戳的事件。
type LogEvent struct {
	Tick   int64     `json:"tick"`
	Type   EventType `json:"type"`
	Group  string    `json:"group"`
	TaskID int       `json:"task,omitempty"` // OOM 杀的是哪个任务
	Bytes  int64     `json:"bytes,omitempty"`
}

// GroupSpec 描述一个控制组的静态配置。
type GroupSpec struct {
	Name   string
	Parent string // 空串表示根
	Weight int64  // CPU 权重, 父组按权重在子组间分配份额

	// CFS 带宽控制 (单位 ms·CPU): 每 Period 个 tick 允许最多 Quota 的运行时间。
	// Quota <= 0 表示不限制。
	Period int64
	Quota  int64

	// 内存三档 (字节):
	//   Low: 保护线, 回收尽量不碰这条线以下的内存
	//   Soft: 软上限, 超过后分配被拖慢并强制回收
	//   Hard: 硬上限, 超过且回收无效时在组内杀任务; <=0 表示无硬上限
	MemLow  int64
	MemSoft int64
	MemHard int64

	// 配额分片: 全局池每次向本地池下发 Slice 毫秒, 本地池用不完的部分
	// 在 ReturnAfter 个 tick 后归还全局池; <=0 时取引擎默认值。
	Slice       int64
	ReturnAfter int64
}

// TaskSpec 描述一个任务及其脚本化的负载。
type TaskSpec struct {
	ID     int
	Name   string
	Group  string
	Events []TaskEvent // 按 Tick 升序的脚本事件
}

// 任务事件类型。
const (
	EvtRun       = "run"        // 产生 CPU 需求, Bytes 字段忽略, 持续到下一个事件
	EvtAllocAnon = "alloc_anon" // 申请匿名内存 Bytes 字节
	EvtAllocFile = "alloc_file" // 申请可回收的文件类内存 Bytes 字节
	EvtFree      = "free"       // 释放 Bytes 字节 (0 表示释放全部)
	EvtIO        = "io"         // 发起一次读写, 阻塞 Bytes 个 tick (借用字段存时长)
	EvtExit      = "exit"       // 任务退出
)

type TaskEvent struct {
	Tick  int64
	Type  string
	Bytes int64
}

// Scenario 是一次完整模拟的静态输入。
type Scenario struct {
	Groups []GroupSpec
	Tasks  []TaskSpec
	// 控制事件: 在指定 tick 冻结/解冻某个组 (含其整个子树)。
	Controls []ControlEvent
	// PSI 三个 EWMA 窗口 (tick 数) 和更新间隔。
	PSIWindows [3]int64
	PSIFlush   int64
}

type ControlEvent struct {
	Tick   int64
	Group  string
	Freeze bool
}

// 内存按可回收性分两类: 文件类可被直接回收, 匿名类只能在压缩后回收一部分。
type memKind uint8

const (
	memAnon memKind = iota
	memFile
)

type taskMem struct {
	anon int64
	file int64
}

func (m taskMem) total() int64 { return m.anon + m.file }

// Task 是任务的运行时状态。
type Task struct {
	ID    int
	Name  string
	Group *Group
	Mem   taskMem

	state   taskState
	eventAt int // 下一条脚本事件在 events 切片中的下标

	// 等待内存回收: 剩余阻塞 tick 数与正在申请的内存。
	memWait  int
	memAlloc int64
	memKind  memKind
	// 等待读写: 剩余阻塞 tick 数。
	ioWait int

	// 本 tick 的调度记账。
	ranTicks float64 // 本 tick 实际拿到的 CPU 毫秒
	stall    Stall   // 本 tick 的停滞位 (PSI 用)

	// 累计统计。
	cpuUsed float64
	alive   bool

	// stride 调度记账。
	vRuntime    float64
	lastRunTick int64
}

type taskState uint8

const (
	stRunning  taskState = iota // 有 CPU 需求, 在运行队列上
	stSleeping                  // 无 CPU 需求 (空闲)
	stMemWait                   // 等内存回收
	stIOWait                    // 等读写
	stDead                      // 已退出/被 OOM 杀掉
)

// localPool 是单个处理器上的配额本地池。
type localPool struct {
	cpu         int
	local       int64 // 本地剩余配额 (ms·CPU, 实际为整 tick 数)
	borrowed    int64 // 累计从全局池借到的分片
	lastUseTick int64 // 最近一次消耗配额的 tick, 用于定时归还
}

// Group 是控制组的运行时状态。
type Group struct {
	ID   int
	Spec GroupSpec

	Parent   *Group
	Children []*Group
	Tasks    []*Task

	// ---- CPU 带宽控制 ----
	periodStart int64   // 当前周期起始 tick
	quotaLeft   int64   // 当前周期剩余配额 (ms·CPU)
	throttled   bool    // 是否被摘下运行队列
	throttleAt  int64   // 本次节流开始的 tick
	throttleCnt int64   // 累计被摘下次数
	throttleMs  int64   // 累计节流时长 (tick 数, 冻结期间照算墙钟)
	cpuUsed     float64 // 累计实际使用的 CPU 毫秒
	periodUsage int64   // 本周期已用配额 (整数 tick 部分)
	pools       []*localPool
	poolByCPU   map[int]*localPool

	// ---- 内存记账 ----
	memCur       int64 // 本组合计 (含子孙), 每 tick 重建
	memReclaimed int64 // 累计回收字节
	oomKills     int64

	// ---- 冻结 ----
	frozen      bool
	frozenTicks int64 // 累计被冻结的 tick 数

	// ---- stride 调度 ----
	vRuntime    float64
	lastRunTick int64

	// ---- PSI ----
	psi *psiState
}

// frozenSelfOrAncestor 判断组是否处于冻结态 (祖先冻结等价于自己冻结)。
func (g *Group) frozenSelfOrAncestor() bool {
	for x := g; x != nil; x = x.Parent {
		if x.frozen {
			return true
		}
	}
	return false
}

// subtreeTasks 收集子树内全部存活任务 (调用方保证缓冲区可复用)。
func (g *Group) subtreeTasks(out []*Task) []*Task {
	out = out[:0]
	var walk func(*Group)
	walk = func(n *Group) {
		for _, t := range n.Tasks {
			if t.alive {
				out = append(out, t)
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(g)
	return out
}
