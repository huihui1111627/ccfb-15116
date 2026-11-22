package sim

import "testing"

// 需求 4:超过软上限时分配任务被拖慢并强制回收;事件计数与原始记录一致。
func TestMemoryHighThrottleAndReclaim(t *testing.T) {
	s := New(Config{NumCPUs: 1})
	s.Rec = &Recorder{On: true}
	g := s.NewCgroup(s.Root, "g", 100)
	g.MemHigh = 64 << 20
	// 纯缓存生产者:1000 字节/微秒 = 1GB/s,持续顶软上限
	s.AddTask(g, "t", []Phase{{Kind: PhaseCPU, DurUs: 1_000_000, CacheBps: 1000}})
	s.Finalize()

	for s.Now < 5_000_000 {
		s.Step()
		if g.Usage() > g.MemHigh {
			t.Fatalf("t=%d usage=%d 超过软上限 %d", s.Now, g.Usage(), g.MemHigh)
		}
	}
	if g.ReclaimedBytes <= 0 {
		t.Error("应有回收发生")
	}
	if g.Events[EvHigh] == 0 {
		t.Error("应记录 high 事件")
	}
	if got := g.PSITotal(ResMem, Some); got <= 0 {
		t.Error("应有内存停滞时间")
	}
	// 用量应被压在软上限附近
	if g.Usage() < g.MemHigh-8<<20 {
		t.Errorf("usage=%d 应贴近软上限(回收与分配平衡)", g.Usage())
	}
	// 离线重算:high 事件数 == 记录里被 high 挡住的分配次数,不多报不漏报
	var want int64
	for _, a := range s.Rec.Allocs {
		if a.GroupID == g.ID && a.Result == AllocBlockedHigh {
			want++
		}
	}
	if g.Events[EvHigh] != want {
		t.Errorf("Events[high]=%d, 离线重算=%d", g.Events[EvHigh], want)
	}
}

// 需求 4:保护线以下的组在回收时被动跳过。
func TestMemoryLowProtection(t *testing.T) {
	s := New(Config{NumCPUs: 1})
	p := s.NewCgroup(s.Root, "p", 100)
	p.MemHigh = 96 << 20
	a := s.NewCgroup(p, "protected", 100)
	a.MemLow = 48 << 20
	b := s.NewCgroup(p, "unprotected", 100)
	// A 先攒 40MB 缓存(在保护线以下)后纯计算
	s.AddTask(a, "ta", []Phase{
		{Kind: PhaseCPU, DurUs: 40_000, CacheBps: 1000},
		{Kind: PhaseCPU, DurUs: 10_000_000},
	})
	// B 持续产生缓存,把父组顶过软上限触发回收
	s.AddTask(b, "tb", []Phase{{Kind: PhaseCPU, DurUs: 10_000_000, CacheBps: 500}})
	s.Finalize()
	s.Run(500_000)

	if b.ReclaimedBytes <= 0 {
		t.Error("未受保护的 B 应被回收")
	}
	if a.ReclaimedBytes != 0 {
		t.Errorf("受保护的 A 不应被回收,实际被回收 %d 字节", a.ReclaimedBytes)
	}
	if a.Cache < 39_000_000 {
		t.Errorf("A 的缓存应基本完好(≥39e6 字节),实际 %d", a.Cache)
	}
}

// 需求 4:超过硬上限时在组内挑任务杀掉;杀的次数与原始记录一致。
func TestMemoryOOMKill(t *testing.T) {
	s := New(Config{NumCPUs: 1})
	s.Rec = &Recorder{On: true}
	g := s.NewCgroup(s.Root, "g", 100)
	g.MemMax = 32 << 20
	prog := []Phase{{Kind: PhaseCPU, DurUs: 10_000_000, AnonBps: 50}} // 50KB/tick
	s.AddTask(g, "t1", prog)
	s.AddTask(g, "t2", prog)
	s.Finalize()

	for s.Now < 2_000_000 {
		s.Step()
		if g.Usage() > g.MemMax {
			t.Fatalf("t=%d usage=%d 超过硬上限 %d", s.Now, g.Usage(), g.MemMax)
		}
	}
	// 两个任务持续增长,先后成为最大者被杀
	if got := len(s.Rec.Kills); got != 2 {
		t.Errorf("OOM 杀次数 = %d, want 2", got)
	}
	if g.Events[EvOOMKill] != int64(len(s.Rec.Kills)) {
		t.Errorf("oom_kill 计数 %d != 记录 %d", g.Events[EvOOMKill], len(s.Rec.Kills))
	}
	// max 事件与记录一致
	var wantMax int64
	for _, a := range s.Rec.Allocs {
		if a.GroupID == g.ID && a.Result == AllocBlockedMax {
			wantMax++
		}
	}
	if g.Events[EvMax] != wantMax {
		t.Errorf("Events[max]=%d, 离线重算=%d", g.Events[EvMax], wantMax)
	}
	// 每次杀都释放了内存
	for i, k := range s.Rec.Kills {
		if k.Freed <= 0 {
			t.Errorf("第 %d 次杀未释放内存", i)
		}
	}
	// 任务全部被杀死后不再有分配
	dead := 0
	for _, tk := range s.Tasks {
		if tk.Dead {
			dead++
		}
	}
	if dead != 2 {
		t.Errorf("死亡任务数 = %d, want 2", dead)
	}
}
