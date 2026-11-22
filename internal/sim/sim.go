// Package sim 实现一个容器资源控制组(cgroup)的简化内核模拟器。
// 模型约定:
//   - 虚拟时钟按固定 tick(默认 1ms)推进,所有统计都以 tick 为最小粒度。
//   - 任务只会挂在叶子控制组里(对应 cgroup v2 的 no-internal-process 约束)。
//   - 任务状态互斥:运行中 / 等 CPU / 等内存回收 / 等 IO / 睡眠 / 冻结。
package sim

import "math/rand"

// 压力资源类型
const (
	ResCPU = iota // 等待处理器
	ResMem        // 等待内存回收
	ResIO         // 等待读写
	NRes
)

// 停滞类型
const (
	Some = iota // 部分停滞:至少一个任务停滞
	Full        // 全部停滞:所有非空闲任务同时停滞
	NKind
)

// 内存事件计数下标(对应 memory.events)
const (
	EvLow = iota
	EvHigh
	EvMax
	EvOOM
	EvOOMKill
	NEv
)

// Config 是模拟器全局参数,零值字段由 defaults 补齐。
type Config struct {
	NumCPUs            int   // 处理器个数
	TickUs             int64 // 每 tick 微秒数,默认 1000(1ms)
	SliceUs            int64 // 带宽小片大小,默认 5000(5ms)
	SliceReturnUs      int64 // 本地池空闲多久后把剩余小片还回全局池,默认 5000
	PSIIntervalUs      int64 // 压力滑动平均更新间隔,默认 1s
	ReclaimBytesPerSec int64 // 回收速率,默认 512MB/s
}

func (c *Config) defaults() {
	if c.NumCPUs <= 0 {
		c.NumCPUs = 4
	}
	if c.TickUs <= 0 {
		c.TickUs = 1000
	}
	if c.SliceUs <= 0 {
		c.SliceUs = 5000
	}
	if c.SliceUs < c.TickUs {
		c.SliceUs = c.TickUs
	}
	if c.SliceReturnUs <= 0 {
		c.SliceReturnUs = 5000
	}
	if c.PSIIntervalUs <= 0 {
		c.PSIIntervalUs = 1_000_000
	}
	if c.ReclaimBytesPerSec <= 0 {
		c.ReclaimBytesPerSec = 512 << 20
	}
}

// Sim 是模拟器主体。
type Sim struct {
	Cfg   Config
	Now   int64 // 当前虚拟时间(微秒)
	Root  *Cgroup
	Tasks []*Task

	// Respawn 为 true 时,被 OOM 杀掉的任务在 RespawnDelayUs 后按原程序复活(演示用)。
	Respawn        bool
	RespawnDelayUs int64

	Rec *Recorder // 原始事件记录,测试对账用;nil 或 On=false 时不记录

	TriggerEvents []TriggerEvent

	groups []*Cgroup // 先序遍历,父先于子
	leaves []*Cgroup

	triggers []*Trigger

	rng            *rand.Rand
	reclaimPerTick int64
	nextSweep      int64
	nextGroupID    int
	nextTaskID     int

	psiHistT []int64 // 每次压力更新时的时间戳(供曲线)
}

// New 创建模拟器并建立根控制组。
func New(cfg Config) *Sim {
	cfg.defaults()
	s := &Sim{
		Cfg:            cfg,
		RespawnDelayUs: 2_000_000,
		rng:            rand.New(rand.NewSource(1)),
		reclaimPerTick: cfg.ReclaimBytesPerSec * cfg.TickUs / 1_000_000,
	}
	if s.reclaimPerTick <= 0 {
		s.reclaimPerTick = 1
	}
	s.Root = s.newCgroup(nil, "root", 100)
	return s
}

// Groups 返回全部控制组(先序,root 在首位)。
func (s *Sim) Groups() []*Cgroup { return s.groups }

// Leaves 返回全部叶子控制组。
func (s *Sim) Leaves() []*Cgroup { return s.leaves }

// Run 推进模拟直到虚拟时间达到 untilUs。
func (s *Sim) Run(untilUs int64) {
	for s.Now < untilUs {
		s.Step()
	}
}

// Step 推进一个 tick。顺序固定,保证测试可复现:
// A. 带宽周期滚动 -> B. 小片回收扫描 -> C. 任务计时(含分配尝试)
// D. 超软限回收 -> E. 内存停滞解除 -> F. CPU 调度 -> G. 压力累计 -> H. 滑动平均/触发器
func (s *Sim) Step() {
	t := s.Now
	dt := s.Cfg.TickUs

	for _, g := range s.groups {
		if g.QuotaUs > 0 {
			s.bwRollover(g, t)
		}
	}
	if t >= s.nextSweep {
		for _, g := range s.groups {
			if g.QuotaUs > 0 {
				s.bwSweep(g, t)
			}
		}
		s.nextSweep = t + s.Cfg.SliceReturnUs
	}
	for _, tk := range s.Tasks {
		s.updateTask(tk, t)
	}
	for _, g := range s.groups {
		s.reclaimIfNeeded(g)
	}
	for _, tk := range s.Tasks {
		s.resolveMemStall(tk, t)
	}
	s.schedule(t)
	s.psiAccumulate(dt)
	if (t+dt)%s.Cfg.PSIIntervalUs == 0 {
		s.psiUpdate(t + dt)
	}
	s.Now = t + dt
}

// effFrozen 判断组是否处于冻结状态(含祖先冻结)。
func (s *Sim) effFrozen(g *Cgroup) bool {
	for c := g; c != nil; c = c.Parent {
		if c.Frozen {
			return true
		}
	}
	return false
}

// effThrottled 判断叶子组当前是否被带宽节流(含祖先节流)。
func (s *Sim) effThrottled(leaf *Cgroup) bool {
	for c := leaf; c != nil; c = c.Parent {
		if c.QuotaUs > 0 && c.bw.throttled {
			return true
		}
	}
	return false
}

// Freeze 冻结/解冻一个组(对子树生效)。
func (s *Sim) Freeze(g *Cgroup, on bool) { g.Frozen = on }

// FindGroup 按名字找组。
func (s *Sim) FindGroup(name string) *Cgroup {
	for _, g := range s.groups {
		if g.Name == name {
			return g
		}
	}
	return nil
}
