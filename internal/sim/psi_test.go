package sim

import (
	"math"
	"testing"
)

// 需求 7:父组压力按子树全部任务一起算,重叠停滞只算一次。
func TestPSIParentUnionNotSum(t *testing.T) {
	s := New(Config{NumCPUs: 2})
	p := s.NewCgroup(s.Root, "p", 100)
	c1 := s.NewCgroup(p, "c1", 100)
	c2 := s.NewCgroup(p, "c2", 100)
	// 两个子组的任务在同一窗口 [10ms,110ms) 做 IO
	prog := []Phase{
		{Kind: PhaseSleep, DurUs: 10_000},
		{Kind: PhaseIO, DurUs: 100_000},
		{Kind: PhaseSleep, DurUs: 1_000_000},
	}
	s.AddTask(c1, "t", prog)
	s.AddTask(c2, "t", prog)
	s.Finalize()
	s.Run(200_000)

	for _, g := range []*Cgroup{c1, c2, p} {
		if got := g.PSITotal(ResIO, Some); got != 100_000 {
			t.Errorf("%s io some = %d, want 100000", g.Name, got)
		}
		if got := g.PSITotal(ResIO, Full); got != 100_000 {
			t.Errorf("%s io full = %d, want 100000", g.Name, got)
		}
	}
	// 若父组是子组简单相加,这里会是 200000
	childSum := c1.PSITotal(ResIO, Some) + c2.PSITotal(ResIO, Some)
	if p.PSITotal(ResIO, Some) == childSum {
		t.Error("父组压力等于子组之和,说明重叠停滞被重复计数")
	}
}

// 需求 8:冻结期间的时间不计入压力。
func TestPSIFrozenExcluded(t *testing.T) {
	s := New(Config{NumCPUs: 1})
	g := s.NewCgroup(s.Root, "g", 100)
	other := s.NewCgroup(s.Root, "other", 100)
	addHogs(s, g, 2)
	addHogs(s, other, 1)
	s.Finalize()

	s.Run(5_000_000)
	cpuSomeAt5 := g.PSITotal(ResCPU, Some)
	cpuTimeAt5 := g.SubtreeCpuUs
	if cpuSomeAt5 <= 0 {
		t.Fatal("冻结前应有 CPU 停滞(2 任务抢 1 核)")
	}

	s.Freeze(g, true)
	s.Run(8_000_000)
	if got := g.PSITotal(ResCPU, Some); got != cpuSomeAt5 {
		t.Errorf("冻结期间 g 的 cpu some 从 %d 涨到 %d", cpuSomeAt5, got)
	}
	if got := g.SubtreeCpuUs; got != cpuTimeAt5 {
		t.Errorf("冻结期间 g 的 CPU 时间从 %d 涨到 %d", cpuTimeAt5, got)
	}
	// 父组在冻结期间只反映 other 组:other 独占 1 核,无停滞
	rootDuring := s.Root.PSITotal(ResCPU, Some)
	s.Run(9_000_000)
	if got := s.Root.PSITotal(ResCPU, Some); got != rootDuring {
		t.Errorf("冻结期间 root 的 cpu some 不应增长(other 独占无竞争), %d -> %d", rootDuring, got)
	}

	s.Freeze(g, false)
	s.Run(10_000_000)
	if got := g.PSITotal(ResCPU, Some); got <= cpuSomeAt5 {
		t.Error("解冻后压力应恢复累计")
	}
}

// lineKey 标识一条压力线。
type lineKey struct{ gid, res, kind int }

// totalsAtBoundaries 从原始停滞变迁记录重算每个间隔边界的累计值。
func totalsAtBoundaries(trs []StallTransition, interval int64, n int) []int64 {
	totals := make([]int64, 0, n)
	var total, cur int64
	on := false
	ti := 0
	for k := int64(1); k <= int64(n); k++ {
		b := k * interval
		for ti < len(trs) && trs[ti].T < b {
			if on {
				total += trs[ti].T - cur
			}
			on = trs[ti].On
			cur = trs[ti].T
			ti++
		}
		if on {
			total += b - cur
			cur = b
		}
		totals = append(totals, total)
	}
	return totals
}

// 需求 5+6:固定种子的几十个混合负载下,各组压力累计与 10s/1m/5m
// 滑动平均,与原始停滞记录离线重算的结果在浮点误差内一致。
func TestPSIOfflineRecompute(t *testing.T) {
	s := GenerateRandom(42, 4)
	s.Rec = &Recorder{On: true}
	const seconds = 30
	s.Run(seconds * 1_000_000)

	ivl := s.Cfg.PSIIntervalUs
	n := int(s.Now / ivl)
	if n != seconds {
		t.Fatalf("更新次数 = %d, want %d", n, seconds)
	}

	// 按压力线归集变迁记录
	trans := map[lineKey][]StallTransition{}
	for _, tr := range s.Rec.Stall {
		k := lineKey{tr.GroupID, tr.Res, tr.Kind}
		trans[k] = append(trans[k], tr)
	}

	f10 := math.Exp(-float64(ivl) / 10_000_000)
	f60 := math.Exp(-float64(ivl) / 60_000_000)
	f300 := math.Exp(-float64(ivl) / 300_000_000)

	checked := 0
	for _, g := range s.Groups() {
		for res := 0; res < NRes; res++ {
			for kind := 0; kind < NKind; kind++ {
				k := lineKey{g.ID, res, kind}
				totals := totalsAtBoundaries(trans[k], ivl, n)

				// 1) 最终累计值精确一致
				final := totals[n-1]
				// 边界之后的尾巴(本场景跑到整秒,无尾巴)
				if got := g.PSITotal(res, kind); got != final {
					t.Errorf("组 %s res=%d kind=%d 累计 %d, 离线重算 %d",
						g.Name, res, kind, got, final)
				}

				// 2) 滑动平均在浮点误差内一致
				var a10, a60, a300, prev float64
				var prevTotal int64
				for _, tot := range totals {
					pct := 100 * float64(tot-prevTotal) / float64(ivl)
					prevTotal = tot
					a10 = a10*f10 + pct*(1-f10)
					a60 = a60*f60 + pct*(1-f60)
					a300 = a300*f300 + pct*(1-f300)
					prev = pct
				}
				_ = prev
				g10, g60, g300 := g.PSIAvgs(res, kind)
				for _, d := range []struct {
					name     string
					got, ref float64
				}{{"avg10", g10, a10}, {"avg60", g60, a60}, {"avg300", g300, a300}} {
					if math.Abs(d.got-d.ref) > 1e-9 {
						t.Errorf("组 %s res=%d kind=%d %s: 模拟器 %v, 离线重算 %v",
							g.Name, res, kind, d.name, d.got, d.ref)
					}
				}
				checked++
			}
		}
	}
	t.Logf("对账压力线 %d 条(组数 %d)", checked, len(s.Groups()))
}

// 需求 5+触发器:触发器点火序列与原始记录离线重算完全一致,不漏报不多报。
func TestTriggersExact(t *testing.T) {
	s := GenerateRandom(7, 4)
	s.Rec = &Recorder{On: true}
	groups := s.Groups()
	s.AddTrigger(groups[1], ResCPU, Some, 50_000, 1_000_000)
	s.AddTrigger(groups[2], ResMem, Some, 10_000, 2_000_000)
	s.AddTrigger(s.Root, ResIO, Some, 100_000, 5_000_000)
	s.AddTrigger(groups[1], ResCPU, Full, 30_000, 1_000_000)
	s.Run(20_000_000)

	ivl := s.Cfg.PSIIntervalUs
	n := int(s.Now / ivl)

	trans := map[lineKey][]StallTransition{}
	for _, tr := range s.Rec.Stall {
		k := lineKey{tr.GroupID, tr.Res, tr.Kind}
		trans[k] = append(trans[k], tr)
	}

	// 离线按同一规则(边沿触发)重算点火序列
	var want []TriggerEvent
	for id, tr := range s.triggers {
		k := lineKey{tr.Group.ID, tr.Res, tr.Kind}
		totals := totalsAtBoundaries(trans[k], ivl, n)
		w := int(tr.WindowUs / ivl)
		fired := false
		for i, tot := range totals {
			var base int64
			if i-w >= 0 {
				base = totals[i-w]
			}
			cond := tot-base >= tr.ThresholdUs
			if cond && !fired {
				fired = true
				want = append(want, TriggerEvent{T: (int64(i) + 1) * ivl, TriggerID: id})
			} else if !cond {
				fired = false
			}
		}
	}
	// 模拟器按 (时间, 注册序) 产生事件;离线按触发器分组产生,排序后比较
	sortEvents := func(ev []TriggerEvent) {
		for i := 1; i < len(ev); i++ {
			for j := i; j > 0; j-- {
				a, b := ev[j-1], ev[j]
				if a.T > b.T || (a.T == b.T && a.TriggerID > b.TriggerID) {
					ev[j-1], ev[j] = b, a
				} else {
					break
				}
			}
		}
	}
	sortEvents(want)
	got := append([]TriggerEvent(nil), s.TriggerEvents...)
	sortEvents(got)

	if len(got) != len(want) {
		t.Fatalf("触发次数 %d, 离线重算 %d(漏报或多报)\ngot=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("第 %d 次触发: 模拟器 %+v, 离线重算 %+v", i, got[i], want[i])
		}
	}
	t.Logf("触发器点火 %d 次,与离线重算完全一致", len(got))
}
