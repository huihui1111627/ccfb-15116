package sim

// 本文件实现简化的层级处理器调度与 CFS 带宽控制。
//
// 调度模型 (逐层 stride / 虚拟运行时间调度):
//   - 每个调度实体 (子组或直属任务) 持有一个虚拟运行时间 vRuntime
//   - 每个 tick, 父组把本 tick 的容量按单位 (1ms) 逐个发放: 每次在
//     同层可运行、未触顶的实体里选 vRuntime 最小者, 发放后给该实体
//     累加一个步长 stride = 本层权重和 / 实体权重
//   - 实体睡眠时不参与, 唤醒时 vRuntime 对齐到同层可运行同伴最小值
//     (防止睡醒后一次性补领), 与 CFS 的 min_vruntime 对齐同理
//   - 同输入下浮点运算顺序固定, 结果逐 tick 可复现; 长期占比严格
//     收敛到权重比例
//
// 带宽模型:
//   - 组按 Period/Quota 限周期带宽, 配额在全局池 (quotaLeft) 与每处理器
//     本地池之间按 Slice 分片下发, 本地池静默 ReturnAfter 后归还
//   - 周期内配额耗尽且仍有需求 -> 整组节流, 全部任务摘运行队列,
//     下个周期开头放回

// handlePeriods 在每个 tick 开头处理 CFS 周期边界:
// 周期起点重置配额, 并把上个周期耗尽配额而被摘下的组重新放回运行队列。
func (e *Engine) handlePeriods() {
	for _, g := range e.groups {
		if g.Spec.Quota <= 0 {
			continue
		}
		if e.now-g.periodStart >= g.Spec.Period {
			g.periodStart = e.now
			g.quotaLeft = g.Spec.Quota
			g.periodUsage = 0
			if g.throttled {
				g.throttled = false
				e.events = append(e.events, LogEvent{
					Tick: e.now, Type: EvUnthrottle, Group: g.Spec.Name,
				})
			}
			// 新周期开始, 本地池作废, 未用的借用配额回到全局池口径。
			g.pools = g.pools[:0]
			g.poolByCPU = map[int]*localPool{}
		}
	}
}

// markThrottled 是 tick 开头的预扫描: 上一 tick 末配额已空、本 tick 仍有
// 需求的组在这一刻被整组摘下运行队列 (只标记一次, 直到下个周期解节流)。
func (e *Engine) markThrottled() {
	for _, g := range e.groups {
		if g.Spec.Quota <= 0 || g.throttled || g.frozenSelfOrAncestor() {
			continue
		}
		if e.groupAvailQuota(g) != 0 {
			continue
		}
		if e.subtreeDemand(g) > 0 {
			e.throttleNow(g)
		}
	}
}

// handlePoolReturns 处理本地池 "用不完定时还回去":
// 每个本地池记录最近一次消耗时间, 静默超过 ReturnAfter 个 tick 就把
// 剩余本地配额归还 (清零), 下次需要时重新向全局池申请分片。
func (e *Engine) handlePoolReturns() {
	for _, g := range e.groups {
		if g.Spec.Quota <= 0 {
			continue
		}
		ret := g.Spec.ReturnAfter
		if ret <= 0 {
			ret = e.cfg.DefaultReturn
		}
		kept := g.pools[:0]
		for _, p := range g.pools {
			if p.local > 0 && e.now-p.lastUseTick >= ret {
				// 借出时已从 quotaLeft 扣过, 这里补回全局池。
				g.quotaLeft += p.local
				p.local = 0
			}
			if p.local > 0 {
				kept = append(kept, p)
			}
		}
		g.pools = kept
	}
}

// sliceSize 返回组的配额分片大小。
func (e *Engine) sliceSize(g *Group) int64 {
	if g.Spec.Slice > 0 {
		return g.Spec.Slice
	}
	return e.cfg.DefaultSlice
}

// borrowSlice 在 cpu 的本地池耗尽时, 从组全局池 (quotaLeft) 下发一个分片。
// 返回本地池拿到的配额 (可能因全局池余额不足而小于标准分片)。
func (e *Engine) borrowSlice(g *Group, cpu int) int64 {
	p := g.poolByCPU[cpu]
	if p == nil {
		p = &localPool{cpu: cpu, lastUseTick: e.now}
		g.poolByCPU[cpu] = p
		g.pools = append(g.pools, p)
	}
	want := e.sliceSize(g)
	if want > g.quotaLeft {
		want = g.quotaLeft
	}
	if want <= 0 {
		return 0
	}
	p.local += want
	p.borrowed += want
	p.lastUseTick = e.now
	g.quotaLeft -= want
	return want
}

// scheduleCPU 是本 tick 的处理器调度。
func (e *Engine) scheduleCPU() {
	// 1. 重置任务本 tick 记账。
	for _, t := range e.tasks {
		t.ranTicks = 0
		t.stall = StallNone
		if !t.alive || t.Group.frozenSelfOrAncestor() {
			continue
		}
	}

	// 2. tick 开头先把配额耗尽的组标记为节流。
	e.markThrottled()

	// 3. 自根递归做 stride 调度, 根容量 = CPU 台数 (ms·CPU)。
	e.schedGroup(e.root, int64(e.cfg.CPUs))

	// 4. 结算: 未被满足的运行需求记为 CPU 等待 (PSI 用)。
	for _, t := range e.tasks {
		if !t.alive || t.Group.frozenSelfOrAncestor() {
			continue
		}
		if t.state == stRunning {
			t.cpuUsed += t.ranTicks
			if t.ranTicks < 1 {
				t.stall |= StallCPU
			}
		}
	}
}

// groupAvailQuota 返回组本 tick 可消耗的配额上限 (ms)。
// 无限额组返回 0 (约定 0 = 无上限); 已节流组也返回 0。
func (e *Engine) groupAvailQuota(g *Group) int64 {
	if g.Spec.Quota <= 0 {
		return 0
	}
	if g.throttled {
		return 0
	}
	var local int64
	for _, p := range g.pools {
		local += p.local
	}
	return g.quotaLeft + local
}

// schedCand 是同一父组下参与本 tick 竞争的一个调度实体。
type schedCand struct {
	group  *Group // 与 task 二选一
	task   *Task
	weight int64
	cap    int64 // 本 tick 还能发放的上限 (ms); 0 表示无上限
	vr     *float64
}

// schedGroup 把 capacity 毫秒的容量在 g 的子组与直属任务间按 stride 规则
// 分配, 再递归向下。返回本子树本 tick 实际消耗的毫秒数。
func (e *Engine) schedGroup(g *Group, capacity int64) int64 {
	if capacity <= 0 || g.frozenSelfOrAncestor() {
		return 0
	}

	cands := make([]schedCand, 0, len(g.Children)+len(g.Tasks))

	// 子组候选: 有运行需求、未冻结、未节流。
	for _, c := range g.Children {
		if c.frozenSelfOrAncestor() || c.throttled {
			continue
		}
		d := e.subtreeDemand(c)
		if d <= 0 {
			continue
		}
		// 每 tick 上限 = min(子树运行任务数, 周期配额可用余额)。
		capv := d
		if q := e.groupAvailQuota(c); q > 0 && q < capv {
			capv = q
		}
		cands = append(cands, schedCand{
			group: c, weight: c.Spec.Weight, cap: capv, vr: &c.vRuntime,
		})
	}

	// 直属任务候选: 与子组同层竞争, 任务默认权重 100, 每 tick 上限 1ms。
	for _, t := range g.Tasks {
		if t.alive && !g.frozenSelfOrAncestor() && t.state == stRunning {
			cands = append(cands, schedCand{
				task: t, weight: 100, cap: 1, vr: &t.vRuntime,
			})
		}
	}
	if len(cands) == 0 {
		return 0
	}

	// 唤醒对齐: 本 tick 新进入可运行集合的实体, vruntime 对齐到同伴最小值。
	minVR := 0.0
	first := true
	for i := range cands {
		if first || *cands[i].vr < minVR {
			minVR = *cands[i].vr
			first = false
		}
	}
	for i := range cands {
		if e.candLastRun(cands[i]) != e.now-1 && *cands[i].vr < minVR {
			*cands[i].vr = minVR
		}
		e.setCandLastRun(cands[i], e.now)
	}

	var wsum float64
	for i := range cands {
		wsum += float64(cands[i].weight)
	}

	// 总需求与实际可发容量。
	var totalDemand int64
	for i := range cands {
		totalDemand += cands[i].cap
	}
	units := capacity
	if units > totalDemand {
		units = totalDemand
	}

	// 逐单位选 vruntime 最小且未触顶的实体, 发放后累加步长。
	got := make([]int64, len(cands))
	for k := int64(0); k < units; k++ {
		best := -1
		for i := range cands {
			if cands[i].cap > 0 && got[i] >= cands[i].cap {
				continue
			}
			if best == -1 || *cands[i].vr < *cands[best].vr {
				best = i
			}
		}
		if best == -1 {
			break
		}
		got[best]++
		*cands[best].vr += wsum / float64(cands[best].weight)
	}

	var used int64
	nextCPU := 0
	for i := range cands {
		if got[i] <= 0 {
			continue
		}
		if cands[i].group != nil {
			used += e.schedGroup(cands[i].group, got[i])
		} else {
			used += e.chargeTask(g, cands[i].task, got[i], &nextCPU)
		}
	}
	return used
}

// candLastRun / setCandLastRun 读写候选实体的最近运行 tick。
func (e *Engine) candLastRun(c schedCand) int64 {
	if c.group != nil {
		return c.group.lastRunTick
	}
	return c.task.lastRunTick
}

func (e *Engine) setCandLastRun(c schedCand, tick int64) {
	if c.group != nil {
		c.group.lastRunTick = tick
	} else {
		c.task.lastRunTick = tick
	}
}

// chargeTask 给任务发放 got 毫秒 (任务每 tick 上限 1ms, 正常情况 got=1),
// 逐毫秒走 "本地池 -> 全局池分片 -> 耗尽" 的配额消耗路径。
// 配额在本 tick 中途耗尽时立即把组标记为节流 (事件时刻 = 耗尽的 tick)。
func (e *Engine) chargeTask(g *Group, t *Task, got int64, nextCPU *int) int64 {
	var consumed int64
	for k := int64(0); k < got; k++ {
		cpu := e.pickCPU(g, *nextCPU)
		*nextCPU = cpu + 1
		if !e.chargeOne(g, cpu) {
			e.throttleNow(g)
			return consumed
		}
		t.ranTicks++
		consumed++
	}
	return consumed
}

// throttleNow 把组整组摘下运行队列并记录一次节流事件。
func (e *Engine) throttleNow(g *Group) {
	if g.throttled {
		return
	}
	g.throttled = true
	g.throttleAt = e.now
	g.throttleCnt++
	e.events = append(e.events, LogEvent{
		Tick: e.now, Type: EvThrottle, Group: g.Spec.Name,
	})
}

// subtreeDemand 统计子树内本 tick 可运行任务的总需求 (ms)。
func (e *Engine) subtreeDemand(g *Group) int64 {
	e.taskBuf = g.subtreeTasks(e.taskBuf)
	var d int64
	for _, t := range e.taskBuf {
		if t.state == stRunning && !t.Group.frozenSelfOrAncestor() {
			d++
		}
	}
	return d
}

// pickCPU 给一个待消耗的毫秒挑处理器: 优先该组已有余额的本地池,
// 否则轮询 (确定性, 测试可重放)。
func (e *Engine) pickCPU(g *Group, hint int) int {
	for _, p := range g.pools {
		if p.local > 0 {
			return p.cpu
		}
	}
	cpu := hint % e.cfg.CPUs
	if cpu < 0 {
		cpu = 0
	}
	return cpu
}

// chargeOne 消耗组在指定 CPU 上的 1ms 配额。
// 路径: 本地池有余额直接扣; 否则向全局池借一个分片; 全局池也空了返回 false。
func (e *Engine) chargeOne(g *Group, cpu int) bool {
	if g.Spec.Quota <= 0 {
		g.cpuUsed += 1 // 无限额组只做用量记账
		return true
	}
	p := g.poolByCPU[cpu]
	if p == nil || p.local <= 0 {
		if e.borrowSlice(g, cpu) <= 0 {
			return false
		}
		p = g.poolByCPU[cpu]
	}
	p.local--
	p.lastUseTick = e.now
	g.periodUsage++
	g.cpuUsed++
	return true
}

// tickThrottleClocks 在每个 tick 末尾累计节流墙钟时长与冻结 tick 数。
// 冻结期间调度停摆, 但墙钟照走, 节流时长仍然累计 (与压力统计互斥)。
func (e *Engine) tickThrottleClocks() {
	for _, g := range e.groups {
		if g.throttled {
			g.throttleMs++
		}
		if g.frozenSelfOrAncestor() {
			g.frozenTicks++
		}
	}
}
