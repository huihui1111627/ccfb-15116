// cgsim 是控制组资源模拟器的入口: 构建固定种子的混合负载场景,
// 在后台按固定节奏推进引擎, 并通过内嵌看板暴露实时状态。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"cgsim/internal/sim"
	"cgsim/internal/web"
)

func main() {
	var (
		addr     = flag.String("addr", ":8080", "看板监听地址")
		seed     = flag.Uint64("seed", 20240521, "负载生成随机种子 (固定种子可复现)")
		ticks    = flag.Int64("ticks", 30000, "总模拟 tick 数 (1 tick = 1ms)")
		cpus     = flag.Int("cpus", 4, "处理器数量")
		headless = flag.Bool("headless", false, "不开网页, 跑完整个模拟后打印汇总")
		stepMS   = flag.Int("step-ms", 50, "自动推进时每个 tick 的墙钟间隔毫秒")
	)
	flag.Parse()

	gc := sim.DefaultGenConfig(*seed)
	gc.CPUs = *cpus
	gc.Ticks = *ticks
	scn := sim.Generate(gc)

	cfg := sim.DefaultConfig()
	cfg.CPUs = *cpus
	cfg.Ticks = *ticks
	cfg.RecordRaw = true
	cfg.HistoryMax = 240

	engine, err := sim.New(scn, cfg, *seed)
	if err != nil {
		log.Fatalf("构建引擎失败: %v", err)
	}
	srv := web.NewServer(engine)

	if *headless {
		if err := srv.RunToEnd(); err != nil {
			log.Fatal(err)
		}
		printSummary(engine)
		return
	}

	// 后台按固定节奏推进引擎; 到达总 tick 后停在最终态供看板查看。
	go func() {
		ticker := time.NewTicker(time.Duration(*stepMS) * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if engine.Now() >= cfg.Ticks {
				continue
			}
			if err := srv.Step(); err != nil {
				log.Printf("step error: %v", err)
				return
			}
		}
	}()

	log.Printf("控制组模拟器已启动, 看板地址 http://localhost%s (种子 %d, %d tick, %d CPU)",
		*addr, *seed, *ticks, *cpus)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func printSummary(e *sim.Engine) {
	snap := e.Snapshot(0)
	fmt.Printf("模拟完成: %d tick, %d CPU, %d 个组, %d 个任务, %d 条事件\n",
		snap.Tick, snap.CPUs, len(snap.Groups), len(snap.Tasks), len(snap.Events))
	fmt.Printf("%-18s %10s %10s %10s %12s %10s %8s\n",
		"组", "CPU用量", "节流次数", "节流ms", "内存用量", "回收字节", "OOM")
	for _, g := range snap.Groups {
		fmt.Printf("%-18s %10.0f %10d %10d %12d %10d %8d\n",
			g.Name, g.CPUUsed, g.ThrottleCnt, g.ThrottleMs,
			g.MemCur, g.MemReclaimed, g.OOMKills)
	}
}
