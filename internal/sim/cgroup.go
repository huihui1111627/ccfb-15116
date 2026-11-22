package sim

// Cgroup 是控制组树中的一个节点。
// 任务只允许挂在叶子节点;内部节点通过权重把拿到的处理器份额分给有需求的子组。
type Cgroup struct {
	ID       int
	Name     string
	Parent   *Cgroup
	Children []*Cgroup

	Weight int64 // 处理器权重(对应 cpu.weight)

	// 处理器带宽限制(对应 cpu.max):QuotaUs<=0 表示不限。
	QuotaUs  int64
	PeriodUs int64
	bw       bwState

	// 内存三档(字节,0 表示不设置):
	//   MemLow  保护线:回收时尽量不动线以下的组
	//   MemHigh 软上限:超过后分配任务被拖慢并强制回收
	//   MemMax  硬上限:超过后在组内挑任务杀掉
	MemLow  int64
	MemHigh int64
	MemMax  int64

	Tasks []*Task // 仅叶子有任务

	// 内存用量(自身 / 子树合计)
	Anon       int64 // 自身匿名页(任务私有内存,不可回收)
	Cache      int64 // 自身页缓存(可回收)
	TotalAnon  int64 // 子树匿名页合计
	TotalCache int64 // 子树页缓存合计

	// 处理器统计
	SelfCpuUs    int64 // 自身任务累计
	SubtreeCpuUs int64 // 子树累计
	NrThrottled  int64 // 节流次数(进入节流状态的次数)
	ThrottledUs  int64 // 累计被节流时长

	// 内存统计
	ReclaimedBytes int64      // 从本组回收掉的缓存字节数
	Events         [NEv]int64 // low/high/max/oom/oom_kill 触发次数
	memWaiters     int        // 停滞等待本组回收的任务数

	// 调度器内部状态
	entitlement float64 // 本 tick 应得的处理器份额(单位:个 CPU)
	demand      int     // 本 tick 可运行任务数
	deficit     float64 // 亏欠计数(盈余为正)
	rr          int
	psiCnt      psiCounts

	psi [NRes][NKind]psiLine

	Frozen bool
}

func (g *Cgroup) leaf() bool { return len(g.Children) == 0 }

// Leaf 报告该组是否为叶子。
func (g *Cgroup) Leaf() bool { return g.leaf() }

// Usage 返回子树内存总用量。
func (g *Cgroup) Usage() int64 { return g.TotalAnon + g.TotalCache }

// Entitlement 返回本 tick 分到的处理器份额(个 CPU)。
func (g *Cgroup) Entitlement() float64 { return g.entitlement }

// PeriodUsage 返回当前带宽周期内已用配额(微秒)。
func (g *Cgroup) PeriodUsage() int64 { return g.bw.usage }

// ThrottledNow 报告当前是否处于节流状态。
func (g *Cgroup) ThrottledNow() bool { return g.QuotaUs > 0 && g.bw.throttled }

// PSITotal 返回某资源某停滞类型的累计停滞微秒数。
func (g *Cgroup) PSITotal(res, kind int) int64 { return g.psi[res][kind].total }

// PSIAvgs 返回 10s/60s/300s 指数滑动平均(百分比)。
func (g *Cgroup) PSIAvgs(res, kind int) (avg10, avg60, avg300 float64) {
	l := &g.psi[res][kind]
	return l.avg10, l.avg60, l.avg300
}

// PSIHistAvg10 返回 avg10 的历史序列(与 Sim.PSIHistT 对齐)。
func (g *Cgroup) PSIHistAvg10(res, kind int) []float64 { return g.psi[res][kind].histAvg10 }

// PSIHistT 返回压力历史序列的时间戳。
func (s *Sim) PSIHistT() []int64 { return s.psiHistT }

// NewCgroup 在 parent 下创建子组(parent 为 nil 时仅用于 root)。
func (s *Sim) NewCgroup(parent *Cgroup, name string, weight int64) *Cgroup {
	return s.newCgroup(parent, name, weight)
}

func (s *Sim) newCgroup(parent *Cgroup, name string, weight int64) *Cgroup {
	if weight <= 0 {
		weight = 1
	}
	g := &Cgroup{
		ID:       s.nextGroupID,
		Name:     name,
		Parent:   parent,
		Weight:   weight,
		PeriodUs: 100_000,
	}
	s.nextGroupID++
	if parent != nil {
		parent.Children = append(parent.Children, g)
	}
	s.groups = append(s.groups, g)
	return g
}

// SetQuota 设置组的带宽配额(微秒/周期),并初始化全局池与每 CPU 本地池。
func (s *Sim) SetQuota(g *Cgroup, quotaUs, periodUs int64) {
	g.QuotaUs = quotaUs
	if periodUs > 0 {
		g.PeriodUs = periodUs
	}
	g.bw = bwState{
		global:    quotaUs,
		local:     make([]int64, s.Cfg.NumCPUs),
		localUsed: make([]int64, s.Cfg.NumCPUs),
	}
}

// Finalize 在树构建完成后建立叶子列表(按 groups 顺序)。
func (s *Sim) Finalize() {
	s.leaves = s.leaves[:0]
	for _, g := range s.groups {
		if g.leaf() {
			s.leaves = append(s.leaves, g)
		}
	}
}

// addAnon 调整匿名页并沿树向上更新合计。
func (s *Sim) addAnon(g *Cgroup, b int64) {
	g.Anon += b
	for c := g; c != nil; c = c.Parent {
		c.TotalAnon += b
	}
}

// addCache 调整页缓存并沿树向上更新合计。
func (s *Sim) addCache(g *Cgroup, b int64) {
	g.Cache += b
	for c := g; c != nil; c = c.Parent {
		c.TotalCache += b
	}
}

// inSubtreeOf 报告 g 是否位于 root 的子树内(含 root 自身)。
func (g *Cgroup) inSubtreeOf(root *Cgroup) bool {
	for c := g; c != nil; c = c.Parent {
		if c == root {
			return true
		}
	}
	return false
}
