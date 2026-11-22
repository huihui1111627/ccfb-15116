package sim

import "math"

// psiCounts 一个组(子树)在一个 tick 里的任务状态计数。
type psiCounts struct {
	run, cpu, mem, io int
}

// psiLine 是 (组, 资源, some/full) 一条压力线。
type psiLine struct {
	total     int64 // 累计停滞微秒
	avg10     float64
	avg60     float64
	avg300    float64
	lastTotal int64
	on        bool // 上一 tick 是否处于该停滞状态

	histT     []int64 // 触发器窗口查询用:(时间, 累计) 样本
	histTotal []int64
	histAvg10 []float64 // 页面曲线用
}

const psiHistKeep = 720 // 历史样本保留上限(秒级粒度)

// psiAccumulate 统计本 tick 各组(子树合并)的停滞并累计。
// 父组按子树全部任务一起算:同一时刻多个子组停滞只算一次。
func (s *Sim) psiAccumulate(dt int64) {
	for _, l := range s.leaves {
		var c psiCounts
		for _, tk := range l.Tasks {
			switch s.effState(tk) {
			case StRunning:
				c.run++
			case StRunnable:
				c.cpu++
			case StMem:
				c.mem++
			case StIO:
				c.io++
			}
		}
		l.psiCnt = c
	}
	// groups 为先序,倒序即子先于父,自底向上合并
	for i := len(s.groups) - 1; i >= 0; i-- {
		g := s.groups[i]
		if !g.leaf() {
			var c psiCounts
			for _, ch := range g.Children {
				c.run += ch.psiCnt.run
				c.cpu += ch.psiCnt.cpu
				c.mem += ch.psiCnt.mem
				c.io += ch.psiCnt.io
			}
			g.psiCnt = c
		}
	}
	for _, g := range s.groups {
		c := g.psiCnt
		active := c.run + c.cpu + c.mem + c.io
		s.psiApply(g, ResCPU, Some, c.cpu > 0, dt)
		s.psiApply(g, ResCPU, Full, c.cpu > 0 && c.run == 0 && c.cpu == active, dt)
		s.psiApply(g, ResMem, Some, c.mem > 0, dt)
		s.psiApply(g, ResMem, Full, c.mem > 0 && c.run == 0 && c.mem == active, dt)
		s.psiApply(g, ResIO, Some, c.io > 0, dt)
		s.psiApply(g, ResIO, Full, c.io > 0 && c.run == 0 && c.io == active, dt)
	}
	for _, tk := range s.Tasks {
		switch s.effState(tk) {
		case StRunnable:
			tk.StallUs[ResCPU] += dt
		case StMem:
			tk.StallUs[ResMem] += dt
		case StIO:
			tk.StallUs[ResIO] += dt
		}
	}
}

func (s *Sim) psiApply(g *Cgroup, res, kind int, on bool, dt int64) {
	l := &g.psi[res][kind]
	if on {
		l.total += dt
	}
	if on != l.on {
		l.on = on
		s.recStall(s.Now, g, res, kind, on)
	}
}

// psiUpdate 到固定间隔时更新 10s/60s/300s 指数滑动平均并评估触发器。
// 公式与内核一致:avg = avg*e^(-dt/W) + pct*(1-e^(-dt/W)),pct 为本周期停滞占比。
func (s *Sim) psiUpdate(t int64) {
	ivl := float64(s.Cfg.PSIIntervalUs)
	f10 := math.Exp(-ivl / 10_000_000)
	f60 := math.Exp(-ivl / 60_000_000)
	f300 := math.Exp(-ivl / 300_000_000)
	for _, g := range s.groups {
		for res := 0; res < NRes; res++ {
			for kind := 0; kind < NKind; kind++ {
				l := &g.psi[res][kind]
				delta := l.total - l.lastTotal
				l.lastTotal = l.total
				pct := 100 * float64(delta) / ivl
				l.avg10 = l.avg10*f10 + pct*(1-f10)
				l.avg60 = l.avg60*f60 + pct*(1-f60)
				l.avg300 = l.avg300*f300 + pct*(1-f300)
				l.histT = append(l.histT, t)
				l.histTotal = append(l.histTotal, l.total)
				l.histAvg10 = append(l.histAvg10, l.avg10)
				if len(l.histT) > psiHistKeep {
					l.histT = l.histT[len(l.histT)-psiHistKeep:]
					l.histTotal = l.histTotal[len(l.histTotal)-psiHistKeep:]
					l.histAvg10 = l.histAvg10[len(l.histAvg10)-psiHistKeep:]
				}
			}
		}
	}
	s.psiHistT = append(s.psiHistT, t)
	if len(s.psiHistT) > psiHistKeep {
		s.psiHistT = s.psiHistT[len(s.psiHistT)-psiHistKeep:]
	}
	for _, tr := range s.triggers {
		s.evalTrigger(tr, t)
	}
}

// Trigger 是一个压力触发器:window 内累计停滞达到 threshold 即触发(边沿触发)。
type Trigger struct {
	ID          int
	Group       *Cgroup
	Res, Kind   int
	ThresholdUs int64
	WindowUs    int64
	fired       bool
}

// TriggerEvent 是一次触发器点火记录。
type TriggerEvent struct {
	T         int64
	TriggerID int
}

// AddTrigger 注册触发器,返回其 ID。
func (s *Sim) AddTrigger(g *Cgroup, res, kind int, thresholdUs, windowUs int64) int {
	id := len(s.triggers)
	s.triggers = append(s.triggers, &Trigger{
		ID: id, Group: g, Res: res, Kind: kind,
		ThresholdUs: thresholdUs, WindowUs: windowUs,
	})
	return id
}

// totalAt 取压力线在 ts 时刻(不晚于 ts 的最近样本)的累计值。
func (l *psiLine) totalAt(ts int64) int64 {
	v := int64(0)
	for i := len(l.histT) - 1; i >= 0; i-- {
		if l.histT[i] <= ts {
			return l.histTotal[i]
		}
	}
	return v
}

func (s *Sim) evalTrigger(tr *Trigger, t int64) {
	l := &tr.Group.psi[tr.Res][tr.Kind]
	stall := l.total - l.totalAt(t-tr.WindowUs)
	cond := stall >= tr.ThresholdUs
	if cond && !tr.fired {
		tr.fired = true
		s.TriggerEvents = append(s.TriggerEvents, TriggerEvent{T: t, TriggerID: tr.ID})
	} else if !cond {
		tr.fired = false
	}
}
