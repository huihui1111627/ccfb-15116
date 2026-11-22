package sim

import "math/rand"

// 负载原型
const (
	ArchCPUHog     = iota // 纯计算
	ArchStreamer          // 计算+读写交替(直播推流)
	ArchTranscoder        // 重计算+重内存(转码)
	ArchIdler             // 大部分时间在睡
	ArchMixed             // 什么都有一点
)

// makeProgram 按原型生成一个循环程序。
func makeProgram(rng *rand.Rand, arch int) []Phase {
	us := func(lo, hi int64) int64 { return lo + rng.Int63n(hi-lo) }
	switch arch {
	case ArchCPUHog:
		return []Phase{
			{Kind: PhaseCPU, DurUs: us(20_000, 80_000)},
			{Kind: PhaseSleep, DurUs: us(0, 2_000)},
		}
	case ArchStreamer:
		return []Phase{
			{Kind: PhaseCPU, DurUs: us(4_000, 12_000), AnonBps: 2, CacheBps: 5},
			{Kind: PhaseIO, DurUs: us(2_000, 6_000)},
			{Kind: PhaseCPU, DurUs: us(3_000, 8_000), CacheBps: 5},
			{Kind: PhaseSleep, DurUs: us(1_000, 4_000)},
		}
	case ArchTranscoder:
		return []Phase{
			{Kind: PhaseCPU, DurUs: us(20_000, 60_000), AnonBps: 15, CacheBps: 40, FreeFrac: 0.4},
			{Kind: PhaseIO, DurUs: us(4_000, 12_000)},
			{Kind: PhaseCPU, DurUs: us(10_000, 30_000), AnonBps: 10, CacheBps: 30, FreeFrac: 0.5},
		}
	case ArchIdler:
		return []Phase{
			{Kind: PhaseSleep, DurUs: us(50_000, 200_000)},
			{Kind: PhaseCPU, DurUs: us(1_000, 3_000)},
		}
	default: // ArchMixed
		return []Phase{
			{Kind: PhaseCPU, DurUs: us(5_000, 25_000), AnonBps: 5, CacheBps: 10, FreeFrac: 0.3},
			{Kind: PhaseIO, DurUs: us(2_000, 10_000)},
			{Kind: PhaseSleep, DurUs: us(2_000, 20_000)},
		}
	}
}

// GenerateDemo 生成演示场景:直播推流 + 展陈插件 + 夜间转码混跑。
func GenerateDemo(seed int64, numCPUs int) *Sim {
	cfg := Config{NumCPUs: numCPUs, ReclaimBytesPerSec: 256 << 20}
	s := New(cfg)
	s.Respawn = true
	rng := rand.New(rand.NewSource(seed))

	live := s.NewCgroup(s.Root, "live", 300)
	push := s.NewCgroup(live, "live-push", 200)
	ctl := s.NewCgroup(live, "live-ctl", 100)

	exhibit := s.NewCgroup(s.Root, "exhibit", 200)
	ui := s.NewCgroup(exhibit, "exhibit-ui", 150)
	api := s.NewCgroup(exhibit, "exhibit-api", 100)

	batch := s.NewCgroup(s.Root, "batch", 400)
	trans := s.NewCgroup(batch, "transcode", 150)
	thumb := s.NewCgroup(batch, "thumbnail", 100)

	// 转码组限带宽:4 核机器上最多用约 0.8 核(权重应得约 1.1 核,会被周期性节流)
	s.SetQuota(trans, 80_000, 100_000)
	// 转码组内存三档:保护线 32MB,软上限 96MB,硬上限 160MB
	trans.MemLow = 32 << 20
	trans.MemHigh = 96 << 20
	trans.MemMax = 160 << 20
	// 展陈组内存受保护
	exhibit.MemLow = 48 << 20
	ui.MemHigh = 128 << 20
	// 整机内存 1GB
	s.Root.MemMax = 1 << 30

	add := func(g *Cgroup, arch, n int, prefix string) {
		for i := 0; i < n; i++ {
			s.AddTask(g, prefix, makeProgram(rng, arch))
		}
	}
	add(push, ArchStreamer, 3, "push")
	add(ctl, ArchIdler, 2, "ctl")
	add(ui, ArchMixed, 3, "ui")
	add(api, ArchStreamer, 2, "api")
	add(trans, ArchTranscoder, 4, "xcode")
	add(thumb, ArchCPUHog, 2, "thumb")

	s.Finalize()
	return s
}

// GenerateRandom 用固定种子生成几十个混合负载的随机场景,供压力/触发器对账测试。
func GenerateRandom(seed int64, numCPUs int) *Sim {
	cfg := Config{NumCPUs: numCPUs}
	s := New(cfg)
	rng := rand.New(rand.NewSource(seed))

	nMid := 2 + rng.Intn(2) // 2-3 个中间层
	var leaves []*Cgroup
	for i := 0; i < nMid; i++ {
		mid := s.NewCgroup(s.Root, midName(i), int64(50+rng.Intn(350)))
		nLeaf := 3 + rng.Intn(3) // 每个中间层 3-5 个叶子
		for j := 0; j < nLeaf; j++ {
			leaf := s.NewCgroup(mid, leafName(i, j), int64(50+rng.Intn(350)))
			leaves = append(leaves, leaf)
			// 约三分之一叶子带配额
			if rng.Intn(3) == 0 {
				quota := int64(50_000+rng.Intn(200_000)) * int64(numCPUs) / 4
				s.SetQuota(leaf, quota, 100_000)
			}
			// 约三分之一叶子带内存限制
			if rng.Intn(3) == 0 {
				leaf.MemHigh = int64(96+rng.Intn(128)) << 20
				leaf.MemMax = leaf.MemHigh * 2
				if rng.Intn(2) == 0 {
					leaf.MemLow = 32 << 20
				}
			}
			nTask := 2 + rng.Intn(4)
			for k := 0; k < nTask; k++ {
				arch := rng.Intn(5)
				s.AddTask(leaf, "task", makeProgram(rng, arch))
			}
		}
	}
	s.Root.MemMax = 2 << 30
	s.Finalize()
	return s
}

func midName(i int) string { return "mid-" + string(rune('a'+i)) }

func leafName(i, j int) string { return midName(i) + "/leaf-" + string(rune('a'+j)) }
