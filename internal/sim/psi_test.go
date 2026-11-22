package sim

import (
	"math"
	"testing"
)

const psiEps = 1e-9

// offlineEWMAHistory 返回离线重算的完整 EWMA 序列 (每个 flush 点三个窗口),
// 与引擎 history 里的点逐个比对。这是完全独立于引擎增量记账的参考实现:
// 直接消费逐 tick 原始停滞记录, 按 flush 分桶算区间占比, 再用
// exp(-flush/window) 衰减公式重算三个窗口的 EWMA。
type offlinePoint struct {
	tick int64
	avg  [3]float64
}

func offlineEWMAHistory(raw []rawTick, windows [3]int64, flush int64, res int, full bool) []offlinePoint {
	decays := [3]float64{}
	for k, w := range windows {
		decays[k] = math.Exp(-float64(flush) / float64(w))
	}
	avg := [3]float64{}
	var pts []offlinePoint

	// 按墙钟 flush 边界分桶 (与引擎一致); 冻结 tick 只跳过计数, 不推动边界。
	var cnt, stalls int64
	flushTick := flush
	for _, rt := range raw {
		if !rt.frozen {
			cnt++
			isStall := rt.some[res]
			if full {
				isStall = rt.full[res]
			}
			if isStall {
				stalls++
			}
		}
		if rt.tick+1 == flushTick {
			if cnt > 0 { // 空桶 (整段冻结) 不产出点
				ratio := float64(stalls) / float64(cnt)
				for k := 0; k < 3; k++ {
					avg[k] = avg[k]*decays[k] + ratio*100*(1-decays[k])
				}
				pts = append(pts, offlinePoint{tick: rt.tick + 1, avg: avg})
			}
			cnt, stalls = 0, 0
			flushTick += flush
		}
	}
	// 收尾 partial flush: 最后一个不完整区间, 有观测才产出。
	if cnt > 0 {
		ratio := float64(stalls) / float64(cnt)
		for k := 0; k < 3; k++ {
			avg[k] = avg[k]*decays[k] + ratio*100*(1-decays[k])
		}
		pts = append(pts, offlinePoint{tick: raw[len(raw)-1].tick + 1, avg: avg})
	}
	return pts
}

// TestPSIvsOfflineRecomputation 在一个会同时产生 CPU/内存/IO 停滞的
// 受控场景里, 验证引擎每个 flush 点的三窗口 EWMA 与离线重算逐点一致。
func TestPSIvsOfflineRecomputation(t *testing.T) {
	windows := [3]int64{100, 400, 2000}
	const flush int64 = 50
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "a", Parent: "root", Weight: 100, MemHard: 1 << 30},
		},
		Tasks: []TaskSpec{
			{ // 满载任务, 在 1 CPU 下与其它任务竞争产生 CPU 等待
				ID: 1, Group: "a",
				Events: []TaskEvent{{Tick: 0, Type: EvtRun}},
			},
			{ // 周期性 IO 阻塞
				ID: 2, Group: "a",
				Events: []TaskEvent{
					{Tick: 0, Type: EvtRun},
					{Tick: 200, Type: EvtIO, Bytes: 120},
					{Tick: 600, Type: EvtIO, Bytes: 200},
					{Tick: 1200, Type: EvtIO, Bytes: 80},
				},
			},
			{ // 内存申请, 配小软上限制造回收等待
				ID: 3, Group: "a",
				Events: []TaskEvent{
					{Tick: 0, Type: EvtAllocAnon, Bytes: 4 << 20},
					{Tick: 400, Type: EvtAllocAnon, Bytes: 8 << 20},
					{Tick: 900, Type: EvtAllocFile, Bytes: 4 << 20},
				},
			},
		},
		PSIWindows: windows, PSIFlush: flush,
	}
	// 给 a 组配一个很紧的软上限, 触发回收等待。
	scn.Groups[1].MemSoft = 2 << 20
	scn.Groups[1].MemHard = 64 << 20
	scn.Groups[1].MemLow = 1 << 20

	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 3000
	cfg.RecordRaw = true
	e, err := New(scn, cfg, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")

	for res := 0; res < 3; res++ {
		for _, full := range []bool{false, true} {
			pts := offlineEWMAHistory(ga.psi.raw, windows, flush, res, full)
			hist := ga.psi.history
			if len(hist) != len(pts) {
				t.Fatalf("res=%d full=%v: history points=%d offline=%d",
					res, full, len(hist), len(pts))
			}
			for i, op := range pts {
				gotAvg := hist[i].AvgSome[res]
				if full {
					gotAvg = hist[i].AvgFull[res]
				}
				for k := 0; k < 3; k++ {
					if math.Abs(gotAvg[k]-op.avg[k]) > psiEps {
						t.Fatalf("res=%d full=%v point=%d window=%d: engine=%.12f offline=%.12f",
							res, full, i, k, gotAvg[k], op.avg[k])
					}
				}
			}
		}
	}
}

// TestPSIParentNoDoubleCount 验证需求 7: 两个子组同一 tick 都在停滞时,
// 父组只算一次, 而不是子组数值相加。
func TestPSIParentNoDoubleCount(t *testing.T) {
	// 1 CPU, 两个子组各放 2 个满载任务 -> 每个 tick 都有任务等 CPU。
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "a", Parent: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "b", Parent: "root", Weight: 100, MemHard: 1 << 30},
		},
		Tasks: []TaskSpec{
			{ID: 1, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
			{ID: 2, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
			{ID: 3, Group: "b", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
			{ID: 4, Group: "b", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 500
	cfg.RecordRaw = true
	e, err := New(scn, cfg, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	root, _ := e.GroupByName("root")
	ga, _ := e.GroupByName("a")
	gb, _ := e.GroupByName("b")

	// 每个非冻结 tick, 4 个任务抢 1 个 CPU: 父组 some-cpu 必为 100%。
	// 若错误地把子组相加会得到 200% (或被截断), 这里严格检查区间占比。
	for _, pt := range root.psi.history {
		if pt.Some[ResCPU] != 1.0 {
			t.Fatalf("root some-cpu=%.3f want 1.0 (dedup failed)", pt.Some[ResCPU])
		}
	}
	// 父组的 some 停滞 tick 总数必须等于模拟时长, 而不是两组之和。
	if root.psi.TotalSome[ResCPU] != cfg.Ticks {
		t.Fatalf("root total some-cpu=%d want %d", root.psi.TotalSome[ResCPU], cfg.Ticks)
	}
	sumChildren := ga.psi.TotalSome[ResCPU] + gb.psi.TotalSome[ResCPU]
	if root.psi.TotalSome[ResCPU] == sumChildren {
		t.Fatalf("root count equals child sum %d: double counting not removed", sumChildren)
	}
}

// TestPSIFreezeExcluded 验证需求 8: 冻结期间的 tick 完全不进压力统计。
func TestPSIFreezeExcluded(t *testing.T) {
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "a", Parent: "root", Weight: 100, MemHard: 1 << 30},
		},
		Tasks: []TaskSpec{
			{ID: 1, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
			{ID: 2, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
		},
		Controls: []ControlEvent{
			{Tick: 100, Group: "a", Freeze: true},
			{Tick: 300, Group: "a", Freeze: false},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 500
	cfg.RecordRaw = true
	e, err := New(scn, cfg, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")
	root, _ := e.GroupByName("root")

	// 冻结 200 个 tick: 原始记录里这些 tick 必须带 frozen 标记。
	var frozenRaw int
	for _, rt := range ga.psi.raw {
		if rt.frozen {
			frozenRaw++
		}
	}
	if frozenRaw != 200 {
		t.Fatalf("frozen raw ticks=%d want 200", frozenRaw)
	}
	// 组与父组的观测 tick 数都应剔除冻结区间。
	var observed int
	for _, rt := range ga.psi.raw {
		if !rt.frozen {
			observed++
		}
	}
	if observed != 300 {
		t.Fatalf("observed ticks=%d want 300", observed)
	}
	// 冻结 tick 上不允许任何停滞记录。
	for _, rt := range ga.psi.raw {
		if rt.frozen {
			for r := 0; r < 3; r++ {
				if rt.some[r] || rt.full[r] {
					t.Fatalf("stall recorded during freeze at tick %d", rt.tick)
				}
			}
		}
	}
	// 冻结 tick 数记账。
	if ga.frozenTicks != 200 {
		t.Fatalf("frozenTicks=%d want 200", ga.frozenTicks)
	}
	// 离线重算 (同样跳过冻结 tick) 必须与引擎一致。
	pts := offlineEWMAHistory(ga.psi.raw, scn.PSIWindows, scn.PSIFlush, ResCPU, false)
	if len(pts) != len(ga.psi.history) {
		t.Fatalf("history len mismatch: %d vs %d", len(ga.psi.history), len(pts))
	}
	for i, op := range pts {
		for k := 0; k < 3; k++ {
			if math.Abs(ga.psi.history[i].AvgSome[ResCPU][k]-op.avg[k]) > psiEps {
				t.Fatalf("freeze EWMA mismatch at %d/%d", i, k)
			}
		}
	}
	// 父组在子树冻结期间不得统计到该子树的停滞: cpu some 的 tick 数
	// 不能超过非冻结 tick 总数。
	if root.psi.TotalSome[ResCPU] > cfg.Ticks-ga.frozenTicks {
		t.Fatalf("root counted stalls during child freeze: %d > %d",
			root.psi.TotalSome[ResCPU], cfg.Ticks-ga.frozenTicks)
	}
}

// TestPSIFullVsSome 验证 full (全部停滞) 与 some (部分停滞) 的语义:
// 1 CPU 两个满载任务, 每 tick 一个跑一个等 -> some 恒真, full 恒假。
func TestPSIFullVsSome(t *testing.T) {
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "a", Parent: "root", Weight: 100, MemHard: 1 << 30},
		},
		Tasks: []TaskSpec{
			{ID: 1, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
			{ID: 2, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 300
	e, err := New(scn, cfg, 11)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")
	for _, pt := range ga.psi.history {
		if pt.Some[ResCPU] != 1.0 {
			t.Fatalf("some-cpu=%.3f want 1.0", pt.Some[ResCPU])
		}
		if pt.Full[ResCPU] != 0.0 {
			t.Fatalf("full-cpu=%.3f want 0.0 (one task always runs)", pt.Full[ResCPU])
		}
	}
}
