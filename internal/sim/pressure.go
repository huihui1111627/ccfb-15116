package sim

// accountPressure 在每个 tick 末尾汇总层级压力。
//
// 核心规则 (需求 7): 父组压力按子树里所有任务一起算, 同一时刻多个
// 子组都在停滞只算一次 —— 因此这里绝不累加子组的 PSI 数值, 而是对
// 每个组重新扫描整棵子树的任务, 用 "是否存在/是否全部" 两个布尔
// 聚合得出 some/full。
//
// 冻结规则 (需求 8): 组 (含祖先) 冻结时该 tick 完全不进压力统计,
// 分子分母都不计。
func (e *Engine) accountPressure() {
	for _, g := range e.groups {
		frozen := g.frozenSelfOrAncestor()

		var some, full [3]bool
		if !frozen {
			some, full = e.subtreeStall(g)
		}
		g.psi.observe(e.now, frozen, some, full)
	}
	// flush 边界在所有组观测完后统一处理。
	for _, g := range e.groups {
		g.psi.maybeFlush(e.now, false)
	}
}

// subtreeStall 扫描组子树内全部任务, 返回三类资源的 some/full 停滞位。
// 活跃定义: 存活且未被冻结, 并且不是纯睡眠 (运行/等 CPU/等内存/等 IO)。
// 没有活跃任务时该 tick 不产生压力 (机器空闲不算停滞)。
func (e *Engine) subtreeStall(g *Group) (some, full [3]bool) {
	e.taskBuf = g.subtreeTasks(e.taskBuf)
	active := 0
	var anyStall [3]bool
	var allStall [3]bool
	for r := 0; r < 3; r++ {
		allStall[r] = true
	}

	for _, t := range e.taskBuf {
		if t.Group.frozenSelfOrAncestor() {
			continue
		}
		if !isActive(t) {
			continue
		}
		active++

		cpuWait := t.state == stRunning && t.ranTicks < 1
		memWait := t.state == stMemWait
		ioWait := t.state == stIOWait

		if cpuWait {
			anyStall[ResCPU] = true
		} else {
			allStall[ResCPU] = false
		}
		if memWait {
			anyStall[ResMem] = true
		} else {
			allStall[ResMem] = false
		}
		if ioWait {
			anyStall[ResIO] = true
		} else {
			allStall[ResIO] = false
		}
	}

	if active == 0 {
		return some, full // 全 false
	}
	some = anyStall
	full = allStall
	// CPU 的 "全部停滞" 只有在组整体被节流 (没有任何任务拿到 CPU) 时
	// 才成立; 上面的逐任务聚合在满节流时自然得到 true。
	return some, full
}

// isActive 判断任务本 tick 是否参与压力统计。
func isActive(t *Task) bool {
	switch t.state {
	case stRunning, stMemWait, stIOWait:
		return t.alive
	default:
		return false
	}
}
