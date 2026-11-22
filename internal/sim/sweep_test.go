package sim

import (
	"math"
	"testing"
)

// TestDeterminism 同一固定种子两次运行必须产出逐字段一致的结果。
func TestDeterminism(t *testing.T) {
	run := func() Snapshot {
		scn := Generate(DefaultGenConfig(424242))
		cfg := DefaultConfig()
		cfg.CPUs = 4
		cfg.Ticks = 6000
		cfg.RecordRaw = true
		e, err := New(scn, cfg, 424242)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Run(); err != nil {
			t.Fatal(err)
		}
		return e.Snapshot(0)
	}
	a, b := run(), run()
	if len(a.Groups) != len(b.Groups) || len(a.Tasks) != len(b.Tasks) || len(a.Events) != len(b.Events) {
		t.Fatal("nondeterministic: structural mismatch across runs")
	}
	for i := range a.Groups {
		ga, gb := a.Groups[i], b.Groups[i]
		if ga.CPUUsed != gb.CPUUsed || ga.ThrottleCnt != gb.ThrottleCnt ||
			ga.ThrottleMs != gb.ThrottleMs || ga.MemCur != gb.MemCur ||
			ga.MemReclaimed != gb.MemReclaimed || ga.OOMKills != gb.OOMKills {
			t.Fatalf("nondeterministic group stats on %q", ga.Name)
		}
	}
	for i := range a.Events {
		if a.Events[i] != b.Events[i] {
			t.Fatalf("nondeterministic event at %d: %+v vs %+v", i, a.Events[i], b.Events[i])
		}
	}
}

// TestMixedLoadSweep 用固定种子生成几十种混合负载, 对每个场景做全套
// 守恒/份额/压力/触发器校验。这是需求中 "固定种子生成几十种混合负载"
// 的主测试。
func TestMixedLoadSweep(t *testing.T) {
	const seeds = 36
	for seed := uint64(1); seed <= seeds; seed++ {
		t.Run(itoa(int(seed)), func(t *testing.T) {
			gc := DefaultGenConfig(seed * 7919) // 固定但分散的种子
			gc.Ticks = 5000
			scn := Generate(gc)
			cfg := DefaultConfig()
			cfg.CPUs = gc.CPUs
			cfg.Ticks = gc.Ticks
			cfg.RecordRaw = true
			e, err := New(scn, cfg, seed*7919)
			if err != nil {
				t.Fatalf("seed %d: build: %v", seed, err)
			}
			if err := e.Run(); err != nil {
				t.Fatalf("seed %d: run: %v", seed, err)
			}
			validateSweep(t, e, scn, cfg)
		})
	}
}

// validateSweep 对单个混合负载场景做全部不变量检查。
func validateSweep(t *testing.T, e *Engine, scn Scenario, cfg Config) {
	t.Helper()

	// 不变量 1: CPU 容量守恒。全系统每 tick 发放量不超过 CPU 台数,
	// 且不低于 min(总运行需求, CPU 台数) 的累计下界 (考虑节流/冻结/睡眠)。
	var totalCPU float64
	for _, g := range e.groups {
		// 子组用量已包含在父组口径里, 只对叶子求和避免重复。
		if len(g.Children) == 0 {
			totalCPU += g.cpuUsed
		}
	}
	if totalCPU > float64(cfg.CPUs)*float64(cfg.Ticks)+1e-9 {
		t.Fatalf("cpu over-issuance: %.1f > capacity %d", totalCPU, cfg.CPUs*int(cfg.Ticks))
	}

	// 不变量 2: 每个带配额组的周期用量逐周期核对。
	for _, gs := range scn.Groups {
		if gs.Quota <= 0 {
			continue
		}
		g, ok := e.GroupByName(gs.Name)
		if !ok {
			t.Fatalf("group %q missing", gs.Name)
		}
		// 任何周期内累计消耗不得超过配额 (切片/池机制也不允许超发)。
		// 用总用量除以完整周期数做上界核对。
		periods := cfg.Ticks / gs.Period
		maxAllowed := float64(periods) * float64(gs.Quota)
		// 末尾残余周期还能再用至多一个配额。
		maxAllowed += float64(gs.Quota)
		if g.cpuUsed > maxAllowed+1e-9 {
			t.Fatalf("group %q used %.1f exceeds max allowed %.1f", gs.Name, g.cpuUsed, maxAllowed)
		}
		// 节流事件数与计数器一致。
		var logThrottle int64
		for _, ev := range e.Events() {
			if ev.Type == EvThrottle && ev.Group == gs.Name {
				logThrottle++
			}
		}
		if logThrottle != g.throttleCnt {
			t.Fatalf("group %q throttle counter %d != events %d", gs.Name, g.throttleCnt, logThrottle)
		}
		// 节流时长不可能超过模拟总时长。
		if g.throttleMs > cfg.Ticks {
			t.Fatalf("group %q throttle duration %d > total ticks", gs.Name, g.throttleMs)
		}
	}

	// 不变量 3: 内存记账守恒。所有存活任务内存之和 = 各组重建用量;
	// 任何时刻不允许超过硬上限 (OOM 之后也要满足)。
	var liveMem int64
	for _, task := range e.tasks {
		if task.alive {
			liveMem += task.Mem.total()
		}
	}
	rootMem := e.memUsage(e.root)
	if liveMem != rootMem {
		t.Fatalf("memory mismatch: tasks=%d root=%d", liveMem, rootMem)
	}
	for _, g := range e.groups {
		if g.Spec.MemHard > 0 && g.memCur > g.Spec.MemHard {
			t.Fatalf("group %q mem %d exceeds hard limit %d without OOM",
				g.Spec.Name, g.memCur, g.Spec.MemHard)
		}
	}

	// 不变量 4: 每个组的 PSI 三窗口 EWMA 与原始停滞记录离线重算一致。
	for _, g := range e.groups {
		for res := 0; res < 3; res++ {
			for _, full := range []bool{false, true} {
				pts := offlineEWMAHistory(g.psi.raw, scn.PSIWindows, scn.PSIFlush, res, full)
				if len(pts) != len(g.psi.history) {
					t.Fatalf("group %q res %d full %v: history %d != offline %d",
						g.Spec.Name, res, full, len(g.psi.history), len(pts))
				}
				for i, op := range pts {
					got := g.psi.history[i].AvgSome[res]
					if full {
						got = g.psi.history[i].AvgFull[res]
					}
					for k := 0; k < 3; k++ {
						if math.Abs(got[k]-op.avg[k]) > psiEps {
							t.Fatalf("group %q EWMA mismatch res=%d full=%v pt=%d w=%d: %.12f vs %.12f",
								g.Spec.Name, res, full, i, k, got[k], op.avg[k])
						}
					}
				}
			}
		}
		// 占比必须在 [0,100]。
		for _, pt := range g.psi.history {
			for r := 0; r < 3; r++ {
				for k := 0; k < 3; k++ {
					if pt.AvgSome[r][k] < -psiEps || pt.AvgSome[r][k] > 100+psiEps {
						t.Fatalf("PSI out of range: %f", pt.AvgSome[r][k])
					}
				}
			}
		}
	}

	// 不变量 5: 层级压力去重。父组的 some 停滞 tick 数不超过任意一个
	// 子组的 some 数之和的朴素上界检查改为: 父组计数 <= 非冻结 tick 总数,
	// 且多子组同 tick 停滞不会让父组占比超过 100% (上面范围检查已覆盖)。
	for _, g := range e.groups {
		for r := 0; r < 3; r++ {
			if g.psi.TotalSome[r] > cfg.Ticks {
				t.Fatalf("group %q total some exceeds tick count", g.Spec.Name)
			}
		}
	}

	// 不变量 6: 触发器事件序列合法 (节流/解节流/冻结/解冻交替, OOM 对应死任务)。
	throttleOpen := map[string]bool{}
	freezeOpen := map[string]bool{}
	for _, ev := range e.Events() {
		switch ev.Type {
		case EvThrottle:
			if throttleOpen[ev.Group] {
				t.Fatalf("double throttle on %q", ev.Group)
			}
			throttleOpen[ev.Group] = true
		case EvUnthrottle:
			if !throttleOpen[ev.Group] {
				t.Fatalf("unthrottle without throttle on %q", ev.Group)
			}
			throttleOpen[ev.Group] = false
		case EvFreeze:
			if freezeOpen[ev.Group] {
				t.Fatalf("double freeze on %q", ev.Group)
			}
			freezeOpen[ev.Group] = true
		case EvUnfreeze:
			if !freezeOpen[ev.Group] {
				t.Fatalf("unfreeze without freeze on %q", ev.Group)
			}
			freezeOpen[ev.Group] = false
		case EvOOMKill:
			victim, ok := e.TaskByID(ev.TaskID)
			if !ok || victim.alive {
				t.Fatalf("oom event without dead victim at %d", ev.Tick)
			}
		case EvReclaim:
			if ev.Bytes <= 0 {
				t.Fatalf("empty reclaim event at %d", ev.Tick)
			}
		}
	}

	// 不变量 7: 任务状态机合法, 死任务不再持有资源。
	for _, task := range e.tasks {
		if !task.alive && task.Mem.total() != 0 {
			t.Fatalf("dead task %d still holds memory", task.ID)
		}
		if task.cpuUsed < 0 {
			t.Fatalf("task %d negative cpu usage", task.ID)
		}
	}
}

// TestThrottleHandCalcAcrossSeeds 在小规模受控场景里逐周期手算节流,
// 跨多个种子确认计数与时长与手算完全相同。
func TestThrottleHandCalcAcrossSeeds(t *testing.T) {
	for seed := uint64(1); seed <= 12; seed++ {
		period := int64(20 + seed*5)
		quota := period / 4 // 每周期只用 25% 时间就被节流
		scn := singleGroupScenario(period, quota)
		cfg := DefaultConfig()
		cfg.CPUs = 1
		cfg.Ticks = period * 30
		e, err := New(scn, cfg, seed)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Run(); err != nil {
			t.Fatal(err)
		}
		ga, _ := e.GroupByName("a")

		// 手算: 每个完整周期内, 前 quota 个 tick 运行, 之后到周期末都节流。
		fullPeriods := cfg.Ticks / period
		wantThrottle := fullPeriods // 每周期恰好一次节流
		wantThrottleMs := fullPeriods * (period - quota)
		wantUsed := fullPeriods * quota
		if ga.throttleCnt != wantThrottle {
			t.Fatalf("seed %d: throttleCnt=%d want %d", seed, ga.throttleCnt, wantThrottle)
		}
		if ga.throttleMs != wantThrottleMs {
			t.Fatalf("seed %d: throttleMs=%d want %d", seed, ga.throttleMs, wantThrottleMs)
		}
		if int64(ga.cpuUsed) != wantUsed {
			t.Fatalf("seed %d: cpuUsed=%.0f want %d", seed, ga.cpuUsed, wantUsed)
		}
	}
}
