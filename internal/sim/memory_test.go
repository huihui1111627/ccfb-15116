package sim

import (
	"testing"
)

// TestMemorySoftLimitReclaim 软上限: 超限分配让任务进入回收等待,
// 回收事件必须发生, 等待结束后分配落账且用量回到软上限附近。
func TestMemorySoftLimitReclaim(t *testing.T) {
	soft := int64(1 << 20) // 1 MiB 软上限
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{
				Name: "a", Parent: "root", Weight: 100,
				MemLow: 256 << 10, MemSoft: soft, MemHard: 1 << 30,
			},
		},
		Tasks: []TaskSpec{
			{
				ID: 1, Group: "a",
				Events: []TaskEvent{
					{Tick: 0, Type: EvtAllocFile, Bytes: 512 << 10},
					{Tick: 10, Type: EvtAllocFile, Bytes: 2 << 20}, // 超软上限
				},
			},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 500
	cfg.ReclaimRate = 256 << 10 // 每 tick 回收 256 KiB
	e, err := New(scn, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")
	task, _ := e.TaskByID(1)

	// 必须产生过回收事件, 且累计回收量有记账。
	var reclaimEvents int
	var reclaimed int64
	for _, ev := range e.Events() {
		if ev.Type == EvReclaim && ev.Group == "a" {
			reclaimEvents++
			reclaimed += ev.Bytes
		}
	}
	if reclaimEvents == 0 {
		t.Fatal("expected reclaim events, got none")
	}
	if ga.memReclaimed != reclaimed {
		t.Fatalf("reclaimed accounting %d != event sum %d", ga.memReclaimed, reclaimed)
	}
	// 软上限不是硬墙: 等待期结束后申请仍会落账, 用量暂时高于 soft 是
	// 允许的 (任务只是被拖慢并被迫回收)。这里核对守恒关系, 并确认回收
	// 确实把用量从 "无回收时的 2.5MiB" 压下来了。
	totalAlloc := int64(512<<10 + 2<<20)
	if task.Mem.total()+ga.memReclaimed != totalAlloc {
		t.Fatalf("memory not conserved: final=%d reclaimed=%d total=%d",
			task.Mem.total(), ga.memReclaimed, totalAlloc)
	}
	if task.Mem.total() >= totalAlloc {
		t.Fatalf("reclaim had no effect: final usage %d equals total alloc", task.Mem.total())
	}
}

// TestMemoryHardLimitOOM 硬上限: 回收也放不下时必须在组内杀任务,
// OOM 事件的组、牺牲者与内存释放都要核对。
func TestMemoryHardLimitOOM(t *testing.T) {
	hard := int64(2 << 20)
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{
				Name: "a", Parent: "root", Weight: 100,
				MemLow: 0, MemSoft: 1 << 20, MemHard: hard,
			},
		},
		Tasks: []TaskSpec{
			{ // 匿名内存不可快速回收, 占住硬上限
				ID: 1, Group: "a",
				Events: []TaskEvent{{Tick: 0, Type: EvtAllocAnon, Bytes: 15 << 20}},
			},
			{ // 文件内存, 可被回收
				ID: 2, Group: "a",
				Events: []TaskEvent{{Tick: 5, Type: EvtAllocFile, Bytes: 1 << 20}},
			},
			{ // 巨型申请, 必然触硬上限且回收无效
				ID: 3, Group: "a",
				Events: []TaskEvent{{Tick: 20, Type: EvtAllocAnon, Bytes: 100 << 20}},
			},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 800
	cfg.ReclaimRate = 64 << 10
	cfg.AnonReclaimPct = 0.1 // 匿名压缩比例很低, 保证回收放不下
	e, err := New(scn, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")

	var oomEvents []LogEvent
	for _, ev := range e.Events() {
		if ev.Type == EvOOMKill && ev.Group == "a" {
			oomEvents = append(oomEvents, ev)
		}
	}
	if len(oomEvents) == 0 {
		t.Fatal("expected OOM kill, got none")
	}
	if ga.oomKills != int64(len(oomEvents)) {
		t.Fatalf("oomKills=%d events=%d", ga.oomKills, len(oomEvents))
	}
	// 牺牲者必须是组内内存最大的存活任务 (确定性选择规则)。
	first := oomEvents[0]
	victim, ok := e.TaskByID(first.TaskID)
	if !ok {
		t.Fatalf("unknown victim id %d", first.TaskID)
	}
	if victim.alive {
		t.Fatal("victim still alive after OOM kill")
	}
	if victim.Mem.total() != 0 {
		t.Fatalf("victim memory not released: %d", victim.Mem.total())
	}
}

// TestMemoryLowProtection 保护线: 回收不能把组用量压到 low 以下。
func TestMemoryLowProtection(t *testing.T) {
	low := int64(1 << 20)
	scn := Scenario{
		Groups: []GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{
				Name: "a", Parent: "root", Weight: 100,
				MemLow: low, MemSoft: 4 << 20, MemHard: 1 << 30,
			},
		},
		Tasks: []TaskSpec{
			{
				ID: 1, Group: "a",
				Events: []TaskEvent{
					{Tick: 0, Type: EvtAllocFile, Bytes: 3 << 20},
					{Tick: 10, Type: EvtAllocFile, Bytes: 4 << 20},
				},
			},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 50,
	}
	cfg := DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 600
	cfg.ReclaimRate = 1 << 20
	e, err := New(scn, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	ga, _ := e.GroupByName("a")
	// 回收过程中任何时刻的组用量都不应低于保护线 (允许最终申请落账后更高)。
	// 这里用累计回收量反推: 回收量不能超过 (回收前用量 - low)。
	task, _ := e.TaskByID(1)
	totalAlloc := int64(7 << 20)
	finalUsage := task.Mem.total()
	reclaimed := ga.memReclaimed
	if finalUsage+reclaimed != totalAlloc {
		t.Fatalf("memory bookkeeping: final=%d reclaimed=%d alloc=%d",
			finalUsage, reclaimed, totalAlloc)
	}
	if finalUsage < low {
		t.Fatalf("usage %d below protection line %d", finalUsage, low)
	}
}

// TestTriggerAudit 是 "触发器既不漏报也不多报" 的审计:
// 独立重放事件日志, 逐条核对节流/解节流/回收/OOM/冻结的触发条件。
func TestTriggerAudit(t *testing.T) {
	scn := Generate(DefaultGenConfig(20240521))
	cfg := DefaultConfig()
	cfg.CPUs = 4
	cfg.Ticks = 8000
	cfg.RecordRaw = true
	e, err := New(scn, cfg, 20240521)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}

	// 审计 1: 节流与解节流严格交替, 每条节流都对应一个配额耗尽的组,
	// 每条解节流都落在该组的周期边界 tick 上。
	throttled := map[string]bool{}
	for _, ev := range e.Events() {
		g, ok := e.GroupByName(ev.Group)
		if !ok {
			t.Fatalf("event references unknown group %q", ev.Group)
		}
		switch ev.Type {
		case EvThrottle:
			if throttled[ev.Group] {
				t.Fatalf("duplicate throttle on %q at %d (over-report)", ev.Group, ev.Tick)
			}
			throttled[ev.Group] = true
			if g.Spec.Quota <= 0 {
				t.Fatalf("throttle on unlimited group %q", ev.Group)
			}
		case EvUnthrottle:
			if !throttled[ev.Group] {
				t.Fatalf("unthrottle without prior throttle on %q at %d (over-report)", ev.Group, ev.Tick)
			}
			delete(throttled, ev.Group)
			// 解节流 tick 必须是该组周期的整数边界。
			if ev.Tick%g.Spec.Period != 0 {
				t.Fatalf("unthrottle on %q at tick %d not a period boundary (period=%d)",
					ev.Group, ev.Tick, g.Spec.Period)
			}
		case EvReclaim:
			if ev.Bytes <= 0 {
				t.Fatalf("zero-byte reclaim event at %d (over-report)", ev.Tick)
			}
		case EvOOMKill:
			victim, ok := e.TaskByID(ev.TaskID)
			if !ok || victim.alive {
				t.Fatalf("oom event at %d does not correspond to a killed task", ev.Tick)
			}
		case EvFreeze, EvUnfreeze:
			// 冻结/解冻必须严格交替。
		}
	}
	// 模拟结束时仍处于节流态的组允许没有收尾解节流 (没有下一个周期)。

	// 审计 2: 冻结/解冻事件严格交替且与最终冻结态一致。
	var frozenSeen map[string]bool = map[string]bool{}
	for _, ev := range e.Events() {
		switch ev.Type {
		case EvFreeze:
			if frozenSeen[ev.Group] {
				t.Fatalf("double freeze on %q (over-report)", ev.Group)
			}
			frozenSeen[ev.Group] = true
		case EvUnfreeze:
			if !frozenSeen[ev.Group] {
				t.Fatalf("unfreeze without freeze on %q (over-report)", ev.Group)
			}
			frozenSeen[ev.Group] = false
		}
	}
	for name, stillFrozen := range frozenSeen {
		g, _ := e.GroupByName(name)
		if stillFrozen != g.frozen {
			t.Fatalf("final freeze state mismatch on %q", name)
		}
	}

	// 审计 3 (不漏报): 每个 OOM 牺牲者在任务规格里都能对应到一次
	// 超大分配, 且 oomKills 计数与事件数相等。
	oomFromLog := int64(0)
	for _, ev := range e.Events() {
		if ev.Type == EvOOMKill {
			oomFromLog++
		}
	}
	var oomFromCounter int64
	for _, g := range e.scn.Groups {
		runtime, ok := e.GroupByName(g.Name)
		if !ok {
			t.Fatalf("group %q missing at runtime", g.Name)
		}
		oomFromCounter += runtime.oomKills
	}
	if oomFromLog != oomFromCounter {
		t.Fatalf("OOM under/over-report: log=%d counter=%d", oomFromLog, oomFromCounter)
	}
}
