package sim

import (
	"math"
	"testing"
)

// twoGroupScenario 构造根下挂两个子组的受控场景, 任务全程满载。
// quota<0 表示该组不设带宽限额。tasksPerGroup 控制每组满载任务数。
func twoGroupScenario(period, quotaA, quotaB int64, tasksPerGroup int) Scenario {
	groups := []GroupSpec{{Name: "root", Weight: 100, MemHard: 1 << 30}}
	add := func(name string, weight int64, quota int64) {
		gs := GroupSpec{Name: name, Parent: "root", Weight: weight, MemHard: 1 << 30}
		if quota >= 0 {
			gs.Period, gs.Quota, gs.Slice, gs.ReturnAfter = period, quota, period+1, period*10
		}
		groups = append(groups, gs)
	}
	add("a", 100, quotaA)
	add("b", 300, quotaB)
	var tasks []TaskSpec
	id := 1
	for i := 0; i < tasksPerGroup; i++ {
		tasks = append(tasks, TaskSpec{ID: id, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}})
		id++
	}
	for i := 0; i < tasksPerGroup; i++ {
		tasks = append(tasks, TaskSpec{ID: id, Group: "b", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}})
		id++
	}
	return Scenario{
		Groups: groups, Tasks: tasks,
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
}

// singleGroupScenario 构造根下只有一个满载任务组的场景, 用于逐周期手算节流。
func singleGroupScenario(period, quota int64) Scenario {
	return Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{
				Name: "a", Parent: "root", Weight: 100,
				Period: period, Quota: quota,
				Slice: period + 1, ReturnAfter: period * 10,
				MemHard: 1 << 30,
			},
		},
		Tasks: []TaskSpec{
			{ID: 1, Group: "a", Events: []TaskEvent{{Tick: 0, Type: EvtRun}}},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
}

// TestCPUWeightedShare 验证满载下两组按 1:3 权重分一台 CPU。
// 每组 2 个满载任务 (每 tick 共 4ms 需求), 理想份额为 1ms 与 3ms,
// 长期累计严格收敛到权重比例, 且每 tick 容量守恒 (无丢失无超发)。
func TestCPUWeightedShare(t *testing.T) {
	scn := twoGroupScenario(0, -1, -1, 2)
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 20_000
	cfg.RecordRaw = true
	e, err := New(scn, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")
	gb, _ := e.GroupByName("b")
	total := ga.cpuUsed + gb.cpuUsed
	// 守恒: 4 个满载任务抢 1 CPU, 每 tick 恰好发出 1ms。
	if math.Abs(total-float64(cfg.Ticks)) > 1e-9 {
		t.Fatalf("capacity not conserved: %.1f != %d", total, cfg.Ticks)
	}
	// 理论份额: a = 1/4, b = 3/4。万分之一容差覆盖整数取整残差。
	gotA := ga.cpuUsed / total
	if math.Abs(gotA-0.25) > 1e-4 {
		t.Fatalf("a share=%.5f want 0.25", gotA)
	}
	gotB := gb.cpuUsed / total
	if math.Abs(gotB-0.75) > 1e-4 {
		t.Fatalf("b share=%.5f want 0.75", gotB)
	}
}

// TestCPUWeightedShareMultiCPU 多处理器、三层权重的比例核对。
func TestCPUWeightedShareMultiCPU(t *testing.T) {
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "x", Parent: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "y", Parent: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "z", Parent: "root", Weight: 200, MemHard: 1 << 30},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
	nextID := 1
	for _, g := range []string{"x", "y", "z"} {
		for i := 0; i < 3; i++ {
			scn.Tasks = append(scn.Tasks, TaskSpec{
				ID: nextID, Group: g,
				Events: []TaskEvent{{Tick: 0, Type: EvtRun}},
			})
			nextID++
		}
	}
	cfg := DefaultConfig()
	cfg.CPUs = 4
	cfg.Ticks = 20_000
	e, err := New(scn, cfg, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	var sums [3]float64
	var grand float64
	for i, name := range []string{"x", "y", "z"} {
		g, _ := e.GroupByName(name)
		sums[i] = g.cpuUsed
		grand += g.cpuUsed
	}
	// 权重 1:1:2 -> 期望 25%/25%/50%。
	want := [3]float64{0.25, 0.25, 0.5}
	for i := range want {
		if math.Abs(sums[i]/grand-want[i]) > 1e-3 {
			t.Fatalf("group %d share=%.5f want %.3f", i, sums[i]/grand, want[i])
		}
	}
	// 9 个满载任务跑 4 CPU, 总发放 = 4*Ticks, 误差不超过 1ms。
	if math.Abs(grand-4*float64(cfg.Ticks)) > 1 {
		t.Fatalf("grand=%.1f want %d", grand, 4*cfg.Ticks)
	}
}

// TestCPUThrottleHandCalc 单组独占 1 CPU, 周期 5 配额 3:
// 每周期跑 3 tick 后在第 4 个 tick 开头被摘下, 节流 2 tick,
// 下个周期开头放回。逐 tick 手算事件时刻与计数。
func TestCPUThrottleHandCalc(t *testing.T) {
	scn := singleGroupScenario(5, 3)
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 100
	e, err := New(scn, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")

	// 20 个完整周期 (0..99), 每周期节流一次。
	if ga.throttleCnt != 20 {
		t.Fatalf("throttleCnt=%d want 20", ga.throttleCnt)
	}
	// 每周期节流 2 tick -> 40ms。
	if ga.throttleMs != 40 {
		t.Fatalf("throttleMs=%d want 40", ga.throttleMs)
	}
	// 用量 = 20 周期 * 3ms。
	if int64(ga.cpuUsed) != 60 {
		t.Fatalf("cpuUsed=%.0f want 60", ga.cpuUsed)
	}
	// 事件时刻手算:
	//   tick 0,1,2 各消耗 1ms (tick2 末配额空); tick3 开头任务仍要运行,
	//   此刻被摘下 -> 节流事件在 3,8,13,...,98 共 20 次;
	//   解节流发生在周期边界 tick 5,10,...,95 共 19 次。
	var thTicks, unthTicks []int64
	for _, ev := range e.Events() {
		if ev.Group != "a" {
			continue
		}
		switch ev.Type {
		case EvThrottle:
			thTicks = append(thTicks, ev.Tick)
		case EvUnthrottle:
			unthTicks = append(unthTicks, ev.Tick)
		}
	}
	if len(thTicks) != 20 {
		t.Fatalf("throttle events=%d want 20", len(thTicks))
	}
	for i, tick := range thTicks {
		if want := int64(3 + 5*i); tick != want {
			t.Fatalf("throttle #%d at %d want %d", i, tick, want)
		}
	}
	if len(unthTicks) != 19 {
		t.Fatalf("unthrottle events=%d want 19", len(unthTicks))
	}
	for i, tick := range unthTicks {
		if want := int64(5 + 5*i); tick != want {
			t.Fatalf("unthrottle #%d at %d want %d", i, tick, want)
		}
	}
}

// TestCPUQuotaEqualsPeriodNoThrottle 配额恰好等于周期容量时, 耗尽点与
// 周期边界重合, 墙钟上没有节流时长, 也不应产生节流事件。
func TestCPUQuotaEqualsPeriodNoThrottle(t *testing.T) {
	scn := singleGroupScenario(5, 5)
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 100
	e, err := New(scn, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")
	if ga.throttleCnt != 0 || ga.throttleMs != 0 {
		t.Fatalf("unexpected throttle: cnt=%d ms=%d", ga.throttleCnt, ga.throttleMs)
	}
	if int64(ga.cpuUsed) != 100 {
		t.Fatalf("cpuUsed=%.0f want 100", ga.cpuUsed)
	}
}

// TestCPUSlicePoolBorrowReturn 验证配额在全局池与本地池间分片下发、
// 用不完定时归还的记账过程 (单元级核对)。
func TestCPUSlicePoolBorrowReturn(t *testing.T) {
	g := &Group{
		Spec:      GroupSpec{Name: "g", Quota: 10, Period: 100, Slice: 3, ReturnAfter: 5},
		quotaLeft: 10,
		poolByCPU: map[int]*localPool{},
	}
	e := &Engine{cfg: Config{DefaultSlice: 3, DefaultReturn: 5, CPUs: 2}, now: 0, groups: []*Group{g}}

	// tick0: CPU0 借 3, 全局池 10->7, 消耗 1 后本地池余 2。
	if !e.chargeOne(g, 0) {
		t.Fatal("charge 1 failed")
	}
	if g.quotaLeft != 7 || g.poolByCPU[0].local != 2 {
		t.Fatalf("after charge1: quotaLeft=%d local=%d", g.quotaLeft, g.poolByCPU[0].local)
	}
	// tick3: 本地 2 用完后再借 3 (7->4), 借后消耗 1, 本地余 2。
	e.now = 3
	e.chargeOne(g, 0)
	e.chargeOne(g, 0)
	e.chargeOne(g, 0)
	if g.quotaLeft != 4 || g.poolByCPU[0].local != 2 {
		t.Fatalf("after charge4: quotaLeft=%d local=%d", g.quotaLeft, g.poolByCPU[0].local)
	}
	// 静默 5 tick 后, handlePoolReturns 把本地余额 2 归还全局池。
	e.now = 8
	e.handlePoolReturns()
	if g.poolByCPU[0].local != 0 || g.quotaLeft != 6 {
		t.Fatalf("after return: local=%d quotaLeft=%d", g.poolByCPU[0].local, g.quotaLeft)
	}
}

// TestCPUQuotaCappedShare 配额上限切断权重分配: a 权重更高但每 5 tick
// 只能用 2ms, 富余容量必须让给 b。
func TestCPUQuotaCappedShare(t *testing.T) {
	scn := twoGroupScenario(5, 2, -1, 1)
	scn.Groups[1].Weight = 300
	scn.Groups[2].Weight = 100
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 20
	e, err := New(scn, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")
	gb, _ := e.GroupByName("b")
	// a 每周期最多 2ms, 4 个周期共 8ms; 其余 12ms 全归 b。
	if int64(ga.cpuUsed) != 8 {
		t.Fatalf("a cpuUsed=%.0f want 8 (quota cap)", ga.cpuUsed)
	}
	if int64(gb.cpuUsed) != 12 {
		t.Fatalf("b cpuUsed=%.0f want 12 (spillover)", gb.cpuUsed)
	}
}
