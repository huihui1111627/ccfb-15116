package sim

// 分配尝试结果
const (
	allocOK        = iota
	allocStallHigh // 被软上限挡住,任务转入内存停滞
	allocDead      // 任务在 OOM 中被杀
)

// attemptAlloc 尝试为任务分配 anon+cache 字节。
// 先查软上限(挡住则拖慢,等回收),再查硬上限(先紧急回收,不够就 OOM 杀)。
// force 为 true 时跳过软上限检查(用于子树已无可回收缓存时避免死锁)。
func (s *Sim) attemptAlloc(tk *Task, anon, cache int64, t int64, force bool) int {
	bytes := anon + cache
	if bytes <= 0 {
		return allocOK
	}
	if !force {
		for g := tk.Group; g != nil; g = g.Parent {
			if g.MemHigh > 0 && g.Usage()+bytes > g.MemHigh {
				g.Events[EvHigh]++
				s.recAlloc(t, g, bytes, AllocBlockedHigh)
				tk.blocker = g
				return allocStallHigh
			}
		}
	}
	for g := tk.Group; g != nil; g = g.Parent {
		if g.MemMax <= 0 || g.Usage()+bytes <= g.MemMax {
			continue
		}
		g.Events[EvMax]++
		s.recAlloc(t, g, bytes, AllocBlockedMax)
		// 硬上限前的紧急回收:把子树里的缓存全部压掉
		if g.TotalCache > 0 {
			s.reclaimFrom(g, g.TotalCache)
		}
		for g.Usage()+bytes > g.MemMax {
			victim := s.largestTask(g)
			if victim == nil {
				return allocDead // 组里已经没有可杀的任务
			}
			s.killTask(victim, g, t)
			if victim == tk {
				return allocDead
			}
		}
	}
	if anon > 0 {
		tk.Anon += anon
		s.addAnon(tk.Group, anon)
	}
	if cache > 0 {
		s.addCache(tk.Group, cache)
	}
	return allocOK
}

// reclaimIfNeeded 每个 tick 对超过软上限、或有任务停滞等待回收的组做强制回收。
func (s *Sim) reclaimIfNeeded(g *Cgroup) {
	if g.MemHigh <= 0 {
		return
	}
	over := g.Usage() - g.MemHigh
	if over <= 0 && g.memWaiters == 0 {
		return
	}
	n := s.reclaimPerTick
	if over > n {
		n = over
	}
	s.reclaimFrom(g, n)
}

// reclaimFrom 在 root 子树内回收 bytes 字节的页缓存。
// 第一遍只动用量超过保护线的组(且最多压到保护线);
// 不够时第二遍不再考虑保护。回收量按可回收量比例分摊,保证确定性。
func (s *Sim) reclaimFrom(root *Cgroup, bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	var victims []*Cgroup
	for _, c := range s.groups {
		if c.Cache > 0 && c.inSubtreeOf(root) {
			victims = append(victims, c)
		}
	}
	freed := int64(0)
	for pass := 0; pass < 2 && bytes > 0; pass++ {
		reclaimable := make([]int64, len(victims))
		var tot int64
		for i, v := range victims {
			var r int64
			if pass == 0 {
				r = v.Usage() - v.MemLow
				if r < 0 {
					r = 0
				}
				if r > v.Cache {
					r = v.Cache
				}
			} else {
				r = v.Cache
			}
			reclaimable[i] = r
			tot += r
		}
		if tot <= 0 {
			break
		}
		want := bytes
		if want > tot {
			want = tot
		}
		passFreed := int64(0)
		for i, v := range victims {
			r := reclaimable[i]
			if r <= 0 {
				continue
			}
			f := want * r / tot
			if f > r {
				f = r
			}
			if f <= 0 {
				continue
			}
			s.addCache(v, -f)
			v.ReclaimedBytes += f
			passFreed += f
		}
		freed += passFreed
		bytes -= passFreed
		if passFreed <= 0 {
			break
		}
	}
	return freed
}

// resolveMemStall 尝试解除任务的内存停滞:挡路的软上限都回到线内就补上分配。
// 若子树已无缓存可回收而用量仍超线(全是匿名页),强制放行以避免死锁。
func (s *Sim) resolveMemStall(tk *Task, t int64) {
	if tk.Dead || tk.state != StMem {
		return
	}
	if tk.stallSince >= t {
		return // 停滞至少持续一个 tick,本 tick 不解除
	}
	if s.effFrozen(tk.Group) {
		return
	}
	bytes := tk.pendAnon + tk.pendCache
	var bg *Cgroup
	for g := tk.Group; g != nil; g = g.Parent {
		if g.MemHigh > 0 && g.Usage()+bytes > g.MemHigh {
			bg = g
			break
		}
	}
	if bg != nil && bg.TotalCache > 0 {
		// 仍被挡:把等待计数挂到当前实际挡路的组上,驱动它回收
		if tk.blocker != bg {
			if tk.blocker != nil {
				tk.blocker.memWaiters--
			}
			tk.blocker = bg
			bg.memWaiters++
		}
		return
	}
	res := s.attemptAlloc(tk, tk.pendAnon, tk.pendCache, t, bg != nil)
	if res == allocStallHigh {
		return
	}
	if tk.blocker != nil {
		tk.blocker.memWaiters--
		tk.blocker = nil
	}
	if res != allocDead {
		tk.state = StRunnable
		tk.pendAnon, tk.pendCache = 0, 0
	}
}

// largestTask 找子树内匿名页最多的存活任务。
func (s *Sim) largestTask(g *Cgroup) *Task {
	var best *Task
	for _, tk := range s.Tasks {
		if tk.Dead || !tk.Group.inSubtreeOf(g) {
			continue
		}
		if best == nil || tk.Anon > best.Anon {
			best = tk
		}
	}
	return best
}

// killTask 在组 g 触发 OOM 时杀掉 victim,释放其匿名页。
func (s *Sim) killTask(victim *Task, g *Cgroup, t int64) {
	if victim.blocker != nil {
		victim.blocker.memWaiters--
		victim.blocker = nil
	}
	freed := victim.Anon
	if freed > 0 {
		s.addAnon(victim.Group, -freed)
	}
	victim.Anon = 0
	victim.Dead = true
	victim.state = StDead
	victim.pendAnon, victim.pendCache = 0, 0
	victim.RespawnAt = t + s.RespawnDelayUs
	g.Events[EvOOM]++
	g.Events[EvOOMKill]++
	s.recKill(t, g, victim, freed)
}
