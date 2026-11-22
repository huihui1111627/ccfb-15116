package sim

import "sort"

// memUsage 汇总一个组 (含整棵子树) 的当前内存。
func (e *Engine) memUsage(g *Group) int64 {
	e.taskBuf = g.subtreeTasks(e.taskBuf)
	var sum int64
	for _, t := range e.taskBuf {
		sum += t.Mem.total()
	}
	return sum
}

// limitBreach 描述一次分配在某个祖先组上撞到的档位。
type limitBreach struct {
	group *Group
	hard  bool // true=硬上限; false=软上限
}

// checkLimits 沿任务所在组向上检查三档阈值, 返回最深 (最严格) 的冲突。
// 保护线 low 不拒绝分配, 只约束回收深度, 因此不在这里返回。
func (e *Engine) checkLimits(t *Task, bytes int64) (limitBreach, bool) {
	var worst limitBreach
	found := false
	for g := t.Group; g != nil; g = g.Parent {
		usage := e.memUsage(g)
		hard := g.Spec.MemHard > 0 && usage+bytes > g.Spec.MemHard
		soft := !hard && g.Spec.MemSoft > 0 && usage+bytes > g.Spec.MemSoft
		if hard || soft {
			// 取最深的祖先: 深层限额语义最具体, 回收/OOM 都在该子树内执行。
			worst = limitBreach{group: g, hard: hard}
			found = true
		}
	}
	return worst, found
}

// requestAlloc 处理一次内存申请。越限不立即失败:
//   - 超软上限: 任务进入 memWait, 被拖慢并强制回收, 回收腾出空间后完成
//   - 超硬上限: 先尝试回收, 仍放不下则在该组子树内挑任务杀掉
func (e *Engine) requestAlloc(t *Task, bytes int64, kind memKind) {
	if bytes <= 0 || !t.alive {
		return
	}
	br, bad := e.checkLimits(t, bytes)
	if !bad {
		e.commitAlloc(t, bytes, kind)
		return
	}
	if br.hard {
		// 硬上限: 先尽一次回收努力。
		reclaimed := e.reclaim(br.group, br.group.Spec.MemHard, bytes)
		e.logReclaim(br.group, reclaimed)
		if e.memUsage(br.group)+bytes <= br.group.Spec.MemHard {
			e.enterMemWait(t, bytes, kind)
			return
		}
		// 回收无效 -> OOM, 在组内挑任务杀掉, 再重试本次分配。
		victim := e.pickOOMVictim(br.group)
		if victim == nil {
			// 子树里没有可杀的任务 (极端配置), 丢弃本次申请。
			return
		}
		e.events = append(e.events, LogEvent{
			Tick: e.now, Type: EvOOMKill, Group: br.group.Spec.Name,
			TaskID: victim.ID,
		})
		e.killTask(victim, true)
		if t.alive {
			e.requestAlloc(t, bytes, kind)
		}
		return
	}
	// 软上限: 进入等待并在等待期持续回收。
	e.enterMemWait(t, bytes, kind)
}

func (e *Engine) enterMemWait(t *Task, bytes int64, kind memKind) {
	t.memAlloc = bytes
	t.memKind = kind
	// 拖慢时长: 至少一个 tick, 与申请量和回收速率挂钩, 确定性可手算。
	t.memWait = int((bytes+e.cfg.ReclaimRate-1)/e.cfg.ReclaimRate) + 1
	if t.memWait < 1 {
		t.memWait = 1
	}
	t.state = stMemWait
}

func (e *Engine) commitAlloc(t *Task, bytes int64, kind memKind) {
	if kind == memAnon {
		t.Mem.anon += bytes
	} else {
		t.Mem.file += bytes
	}
}

// pickOOMVictim 在组子树内挑 OOM 牺牲者: 内存占用最大者胜,
// 并列时取任务 ID 小者, 保证确定性。
func (e *Engine) pickOOMVictim(g *Group) *Task {
	e.taskBuf = g.subtreeTasks(e.taskBuf)
	var victim *Task
	var best int64 = -1
	for _, t := range e.taskBuf {
		cur := t.Mem.total()
		if cur > best || (cur == best && victim != nil && t.ID < victim.ID) {
			best = cur
			victim = t
		}
	}
	return victim
}

// reclaim 在 g 子树内回收内存, 尽量把组用量压到 target 以下、
// 或至少腾出 need 字节。回收规则:
//  1. 保护线 low 以下的内存不动
//  2. 先回收文件类内存 (直接丢弃), 再按 AnonReclaimPct 压缩匿名内存
//  3. 单 tick 回收量受 ReclaimRate 限制; 任务在 memWait 期间每 tick 调用一次
//
// 返回实际回收字节数。
func (e *Engine) reclaim(g *Group, target, need int64) int64 {
	usage := e.memUsage(g)
	goalUsage := target - need
	if goalUsage < 0 {
		goalUsage = 0
	}
	// 保护线: 不把内存压到 low 以下。
	floor := g.Spec.MemLow
	if floor > 0 && floor > goalUsage {
		goalUsage = floor
	}
	budget := e.cfg.ReclaimRate
	var reclaimed int64

	e.taskBuf = g.subtreeTasks(e.taskBuf)
	// 确定性顺序: 先按任务 ID 排序, 回收行为可逐字节手算。
	ids := make([]int, 0, len(e.taskBuf))
	idToTask := map[int]*Task{}
	for _, t := range e.taskBuf {
		ids = append(ids, t.ID)
		idToTask[t.ID] = t
	}
	sort.Ints(ids)

	take := func(t *Task, kind memKind, want int64) int64 {
		if want <= 0 || budget <= 0 || usage <= goalUsage {
			return 0
		}
		avail := t.Mem.file
		if kind == memAnon {
			avail = t.Mem.anon
		}
		n := want
		if n > avail {
			n = avail
		}
		if n > budget {
			n = budget
		}
		if n <= 0 {
			return 0
		}
		if kind == memAnon {
			t.Mem.anon -= n
		} else {
			t.Mem.file -= n
		}
		budget -= n
		usage -= n
		reclaimed += n
		return n
	}

	// 第一轮: 文件类。
	for _, id := range ids {
		t := idToTask[id]
		for t.Mem.file > 0 && budget > 0 && usage > goalUsage {
			take(t, memFile, t.Mem.file)
		}
	}
	// 第二轮: 匿名压缩, 每个任务最多压掉当前匿名量的 AnonReclaimPct。
	for _, id := range ids {
		t := idToTask[id]
		reclaimable := int64(float64(t.Mem.anon) * e.cfg.AnonReclaimPct)
		for reclaimable > 0 && budget > 0 && usage > goalUsage {
			n := take(t, memAnon, reclaimable)
			if n <= 0 {
				break
			}
			reclaimable -= n
		}
	}

	if reclaimed > 0 {
		g.memReclaimed += reclaimed
	}
	return reclaimed
}

func (e *Engine) logReclaim(g *Group, bytes int64) {
	if bytes <= 0 {
		return
	}
	e.events = append(e.events, LogEvent{
		Tick: e.now, Type: EvReclaim, Group: g.Spec.Name, Bytes: bytes,
	})
}

// handleMemoryAndIO 推进阻塞中的任务, 并在每个 tick 做持续回收。
func (e *Engine) handleMemoryAndIO() {
	for _, t := range e.tasks {
		if !t.alive || t.Group.frozenSelfOrAncestor() {
			continue
		}
		switch t.state {
		case stMemWait:
			// 等待期每个 tick 在所属组上做一轮回收, 体现 "被迫回收"。
			g := t.Group
			target := g.Spec.MemSoft
			if g.Spec.MemHard > 0 && (target <= 0 || g.Spec.MemHard < target) {
				target = g.Spec.MemHard
			}
			if target <= 0 {
				target = e.memUsage(g)
			}
			r := e.reclaim(g, target, t.memAlloc)
			e.logReclaim(g, r)
			t.memWait--
			if t.memWait <= 0 {
				// 等待结束: 若仍超硬上限则触发 OOM 路径, 否则落账。
				if br, bad := e.checkLimits(t, t.memAlloc); bad && br.hard {
					bytes, kind := t.memAlloc, t.memKind
					t.state = stRunning
					t.memAlloc = 0
					e.requestAlloc(t, bytes, kind)
				} else {
					e.commitAlloc(t, t.memAlloc, t.memKind)
					t.memAlloc = 0
					t.state = stRunning
				}
			}
		case stIOWait:
			t.ioWait--
			if t.ioWait <= 0 {
				t.state = stRunning
			}
		}
	}

	// 重建各组内存用量快照, 供看板与本 tick 的压力统计使用。
	for _, g := range e.groups {
		g.memCur = e.memUsage(g)
	}
}
