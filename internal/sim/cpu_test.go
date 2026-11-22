package sim

import (
	"fmt"
	"math"
	"testing"
)

func hogProg() []Phase {
	return []Phase{{Kind: PhaseCPU, DurUs: 50_000}}
}

func addHogs(s *Sim, g *Cgroup, n int) {
	for i := 0; i < n; i++ {
		s.AddTask(g, fmt.Sprintf("hog%d", i), hogProg())
	}
}

func expectClose(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if want == 0 {
		if got != 0 {
			t.Errorf("%s: got %v, want 0", name, got)
		}
		return
	}
	if math.Abs(got-want)/math.Abs(want) > tol {
		t.Errorf("%s: got %v, want %v (tol %.2f%%)", name, got, want, tol*100)
	}
}

// 需求 1:各组实际处理器时间与按权重算出的理论份额一致(扁平树)。
func TestCPUSharesMatchWeights(t *testing.T) {
	s := New(Config{NumCPUs: 4})
	weights := []int64{100, 200, 300, 400}
	var groups []*Cgroup
	var totW int64
	for i, w := range weights {
		g := s.NewCgroup(s.Root, fmt.Sprintf("g%d", i), w)
		addHogs(s, g, 4)
		groups = append(groups, g)
		totW += w
	}
	s.Finalize()
	s.Run(20_000_000)

	total := float64(s.Root.SubtreeCpuUs)
	if want := float64(4) * 20_000_000; total != want {
		t.Fatalf("total cpu = %v, want %v (机器应一直满载)", total, want)
	}
	for i, g := range groups {
		want := total * float64(weights[i]) / float64(totW)
		expectClose(t, g.Name, float64(g.SubtreeCpuUs), want, 0.02)
	}
}

// 需求 1:分层树——子组按权重分父组拿到的份额。
func TestCPUSharesHierarchical(t *testing.T) {
	s := New(Config{NumCPUs: 4})
	a := s.NewCgroup(s.Root, "A", 100)
	a1 := s.NewCgroup(a, "A1", 100)
	a2 := s.NewCgroup(a, "A2", 300)
	b := s.NewCgroup(s.Root, "B", 300)
	addHogs(s, a1, 4)
	addHogs(s, a2, 4)
	addHogs(s, b, 4)
	s.Finalize()
	s.Run(20_000_000)

	total := float64(s.Root.SubtreeCpuUs) // 80s
	// A 应得 1/4,B 应得 3/4;A 内部 A1:A2 = 1:3
	expectClose(t, "A", float64(a.SubtreeCpuUs), total*0.25, 0.02)
	expectClose(t, "B", float64(b.SubtreeCpuUs), total*0.75, 0.02)
	expectClose(t, "A1", float64(a1.SubtreeCpuUs), total*0.25*0.25, 0.03)
	expectClose(t, "A2", float64(a2.SubtreeCpuUs), total*0.25*0.75, 0.03)
}

// 需求 2:配额跑完即整组摘下,节流次数与时长与逐周期手算一致。
// 1 个 CPU、配额 40ms/100ms 周期、持续满载需求:
// 每周期第 40ms 节流,60ms 后放回;1 秒 = 10 个周期。
func TestQuotaThrottleExact(t *testing.T) {
	for _, slice := range []int64{5_000, 40_000} {
		t.Run(fmt.Sprintf("slice=%d", slice), func(t *testing.T) {
			s := New(Config{NumCPUs: 1, SliceUs: slice})
			s.Rec = &Recorder{On: true}
			g := s.NewCgroup(s.Root, "g", 100)
			s.SetQuota(g, 40_000, 100_000)
			addHogs(s, g, 2)
			s.Finalize()
			s.Run(1_000_000)

			// 手算:10 个周期,每周期节流 1 次、60ms
			if g.NrThrottled != 10 {
				t.Errorf("NrThrottled = %d, want 10", g.NrThrottled)
			}
			// 最后一个周期的节流区间 [940ms,1000ms) 尚未到周期边界,未入账
			pending := int64(0)
			if g.bw.throttled {
				pending = s.Now - g.bw.throttleStart
			}
			if got := g.ThrottledUs + pending; got != 600_000 {
				t.Errorf("ThrottledUs(含未结账) = %d, want 600000", got)
			}
			if got := g.SubtreeCpuUs; got != 400_000 {
				t.Errorf("SubtreeCpuUs = %d, want 400000", got)
			}

			// 用原始节流变迁记录离线重算,应与计数器完全一致
			var nr, throttled int64
			var onAt int64 = -1
			for _, tr := range s.Rec.Throttle {
				if tr.GroupID != g.ID {
					continue
				}
				if tr.On {
					nr++
					onAt = tr.T
				} else {
					throttled += tr.T - onAt
					onAt = -1
				}
			}
			if onAt >= 0 {
				throttled += s.Now - onAt
			}
			if nr != g.NrThrottled {
				t.Errorf("离线重算节流次数 %d != 计数器 %d", nr, g.NrThrottled)
			}
			if throttled != g.ThrottledUs+pending {
				t.Errorf("离线重算节流时长 %d != 计数器 %d", throttled, g.ThrottledUs+pending)
			}
		})
	}
}

// 需求 1+2:配额封顶后,剩余算力按权重分给其他组。
func TestQuotaCapRedistribution(t *testing.T) {
	s := New(Config{NumCPUs: 2, SliceUs: 25_000})
	a := s.NewCgroup(s.Root, "A", 100)
	s.SetQuota(a, 50_000, 100_000) // 每周期 50ms,即 0.5 个 CPU
	b := s.NewCgroup(s.Root, "B", 100)
	addHogs(s, a, 4)
	addHogs(s, b, 4)
	s.Finalize()
	s.Run(5_000_000)

	// A 被压到 0.5 核,B 拿走剩下的 1.5 核
	expectClose(t, "A capped", float64(a.SubtreeCpuUs), 0.5*5_000_000, 0.03)
	expectClose(t, "B gets rest", float64(b.SubtreeCpuUs), 1.5*5_000_000, 0.03)
}

// 需求 3:本地池用不完的小片定时还回全局池。
func TestSliceReturn(t *testing.T) {
	s := New(Config{NumCPUs: 2}) // slice 5ms,回收延迟 5ms
	g := s.NewCgroup(s.Root, "g", 100)
	s.SetQuota(g, 30_000, 100_000)
	// 任务只跑 3ms 就睡 200ms:本地池拿走 5ms 小片,用 3ms,剩 2ms 空闲
	s.AddTask(g, "t", []Phase{
		{Kind: PhaseCPU, DurUs: 3_000},
		{Kind: PhaseSleep, DurUs: 200_000},
	})
	s.Finalize()
	s.Run(20_000)

	// 全局池:30000 - 5000(下发) + 2000(空闲归还) = 27000
	if g.bw.global != 27_000 {
		t.Errorf("global pool = %d, want 27000(空闲小片应被定时还回)", g.bw.global)
	}
	if g.bw.local[0] != 0 {
		t.Errorf("local pool = %d, want 0", g.bw.local[0])
	}
}
