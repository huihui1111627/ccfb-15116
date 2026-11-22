package sim

import "math"

// bwState 是一个组的带宽配额运行状态(对应 cfs_bandwidth)。
type bwState struct {
	global    int64   // 本周期全局池剩余
	local     []int64 // 每个处理器的本地池剩余
	localUsed []int64 // 本地池最后使用时间(空闲回收用)
	usage     int64   // 本周期已消耗

	throttled     bool
	throttleStart int64
	periodStart   int64
}

// bwRollover 处理周期边界:节流中的组在周期开头被放回运行队列,池子重置。
func (s *Sim) bwRollover(g *Cgroup, t int64) {
	bw := &g.bw
	for t >= bw.periodStart+g.PeriodUs {
		end := bw.periodStart + g.PeriodUs
		if bw.throttled {
			g.ThrottledUs += end - bw.throttleStart
			bw.throttled = false
			s.recThrottle(end, g, false)
		}
		bw.global = g.QuotaUs
		for i := range bw.local {
			bw.local[i] = 0
		}
		bw.usage = 0
		bw.periodStart = end
	}
}

// bwSweep 定时把空闲的本地池剩余小片还回全局池;若因此有配额可用则解除节流。
func (s *Sim) bwSweep(g *Cgroup, t int64) {
	bw := &g.bw
	dt := s.Cfg.TickUs
	for cpu := range bw.local {
		if bw.local[cpu] > 0 && t-bw.localUsed[cpu] >= s.Cfg.SliceReturnUs {
			bw.global += bw.local[cpu]
			bw.local[cpu] = 0
		}
	}
	if bw.throttled {
		for cpu := range bw.local {
			if bw.local[cpu] >= dt || bw.local[cpu]+bw.global >= dt {
				g.ThrottledUs += t - bw.throttleStart
				bw.throttled = false
				s.recThrottle(t, g, false)
				break
			}
		}
	}
}

// bwCanRun 检查组内任务能否在指定处理器上拿到本 tick 的配额;
// 拿不到就把沿途受限的组标记为节流(摘下运行队列)。
func (s *Sim) bwCanRun(leaf *Cgroup, cpu int, t int64) bool {
	dt := s.Cfg.TickUs
	ok := true
	for g := leaf; g != nil; g = g.Parent {
		if g.QuotaUs <= 0 {
			continue
		}
		bw := &g.bw
		if bw.local[cpu] >= dt || bw.local[cpu]+bw.global >= dt {
			continue
		}
		if !bw.throttled {
			bw.throttled = true
			bw.throttleStart = t
			g.NrThrottled++
			s.recThrottle(t, g, true)
		}
		ok = false
	}
	return ok
}

// bwConsume 实际消耗本 tick 的配额:本地池不足时从全局池按小片补。
func (s *Sim) bwConsume(leaf *Cgroup, cpu int, t int64) {
	dt := s.Cfg.TickUs
	for g := leaf; g != nil; g = g.Parent {
		if g.QuotaUs <= 0 {
			continue
		}
		bw := &g.bw
		if bw.local[cpu] < dt {
			take := s.Cfg.SliceUs
			if take > bw.global {
				take = bw.global
			}
			bw.global -= take
			bw.local[cpu] += take
		}
		bw.local[cpu] -= dt
		bw.localUsed[cpu] = t
		bw.usage += dt
	}
}

// maxDemand 返回子树最多能同时用掉的处理器数(每个可运行任务最多占一个)。
func (g *Cgroup) maxDemand() float64 {
	if g.leaf() {
		return float64(g.demand)
	}
	sum := 0.0
	for _, c := range g.Children {
		sum += c.maxDemand()
	}
	return sum
}

// distribute 把 budget 个处理器按权重在有需求的子组间分(水填法,
// 需求不足的子组先按自身需求封顶,剩余继续按权重分给其他子组),
// 返回实际用掉的份额,并把叶子应得份额写进 entitlement。
func (g *Cgroup) distribute(budget float64) float64 {
	if g.leaf() {
		u := math.Min(budget, float64(g.demand))
		g.entitlement = u
		return u
	}
	var active []*Cgroup
	for _, c := range g.Children {
		if c.maxDemand() > 0 {
			active = append(active, c)
		}
	}
	if len(active) == 0 {
		return 0
	}
	rem := budget
	used := 0.0
	for len(active) > 0 && rem > 1e-9 {
		var totW int64
		for _, c := range active {
			totW += c.Weight
		}
		if totW <= 0 {
			break
		}
		var saturated []*Cgroup
		for _, c := range active {
			if c.maxDemand() <= rem*float64(c.Weight)/float64(totW)+1e-12 {
				saturated = append(saturated, c)
			}
		}
		if len(saturated) == 0 {
			for _, c := range active {
				share := rem * float64(c.Weight) / float64(totW)
				used += c.distribute(share)
			}
			return used
		}
		next := active[:0]
		for _, c := range active {
			isSat := false
			for _, sc := range saturated {
				if sc == c {
					isSat = true
					break
				}
			}
			if isSat {
				u := c.distribute(c.maxDemand())
				used += u
				rem -= u
			} else {
				next = append(next, c)
			}
		}
		active = next
	}
	return used
}

// schedule 做一次 tick 的处理器调度。
func (s *Sim) schedule(t int64) {
	dt := s.Cfg.TickUs

	for _, tk := range s.Tasks {
		tk.ranTick = false
	}

	// 统计每个叶子的可运行任务数(冻结/节流中的组视为无需求)
	for _, l := range s.leaves {
		l.demand = 0
		l.entitlement = 0
		if s.effFrozen(l) || s.effThrottled(l) {
			continue
		}
		for _, tk := range l.Tasks {
			if !tk.Dead && tk.state == StRunnable {
				l.demand++
			}
		}
	}

	s.Root.distribute(float64(s.Cfg.NumCPUs))

	// 亏欠累计:应得份额随时间累积,运行则扣减
	const clampTicks = 1000.0
	for _, l := range s.leaves {
		if l.demand > 0 {
			l.deficit += l.entitlement * float64(dt)
		}
		clamp := clampTicks * float64(dt)
		if l.deficit > clamp {
			l.deficit = clamp
		} else if l.deficit < -clamp {
			l.deficit = -clamp
		}
	}

	// 逐个处理器挑选亏欠最多的叶子运行;没有正亏欠时仍然挑(工作守恒)
	for cpu := 0; cpu < s.Cfg.NumCPUs; cpu++ {
		for {
			var best *Cgroup
			bestDef := math.Inf(-1)
			for _, l := range s.leaves {
				if l.demand <= 0 {
					continue
				}
				if l.deficit > bestDef {
					bestDef = l.deficit
					best = l
				}
			}
			if best == nil {
				break
			}
			if !s.bwCanRun(best, cpu, t) {
				best.demand = 0 // 本 tick 剩余处理器不再考虑它
				continue
			}
			tk := best.pickTask()
			if tk == nil {
				best.demand = 0
				continue
			}
			s.runTask(tk, best, cpu, t)
			best.deficit -= float64(dt)
			best.demand--
			break
		}
	}
}

// pickTask 在组内轮转挑一个可运行任务。
func (g *Cgroup) pickTask() *Task {
	n := len(g.Tasks)
	for i := 0; i < n; i++ {
		g.rr = (g.rr + 1) % n
		tk := g.Tasks[g.rr]
		if !tk.Dead && tk.state == StRunnable {
			return tk
		}
	}
	return nil
}

// runTask 让任务在指定处理器上跑一个 tick。
func (s *Sim) runTask(tk *Task, leaf *Cgroup, cpu int, t int64) {
	dt := s.Cfg.TickUs
	s.bwConsume(leaf, cpu, t)
	tk.ranTick = true
	tk.state = StRunning
	tk.CPUTimeUs += dt
	tk.remainUs -= dt
	leaf.SelfCpuUs += dt
	for g := leaf; g != nil; g = g.Parent {
		g.SubtreeCpuUs += dt
	}
	if tk.remainUs <= 0 {
		s.advancePhase(tk)
	}
}
