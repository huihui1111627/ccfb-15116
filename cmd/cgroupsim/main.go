// 容器资源控制组模拟器:直播推流 + 展陈插件 + 夜间转码混跑场景。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"cgroupsim/internal/sim"
	"cgroupsim/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	seed := flag.Int64("seed", 42, "负载随机种子")
	cpus := flag.Int("cpus", 4, "处理器个数")
	speed := flag.Int64("speed", 8, "模拟速度(相对真实时间的倍率)")
	flag.Parse()

	s := sim.GenerateDemo(*seed, *cpus)
	var mu sync.Mutex

	// 模拟循环:按真实流逝时间 × 倍率推进虚拟时钟
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		last := time.Now()
		for range ticker.C {
			now := time.Now()
			advance := now.Sub(last).Microseconds() * *speed
			last = now
			mu.Lock()
			for i := int64(0); i < advance; i += s.Cfg.TickUs {
				s.Step()
			}
			mu.Unlock()
		}
	}()

	srv := web.New(s, &mu)
	fmt.Printf("场景:直播推流 + 展陈插件 + 夜间转码混跑(%d 核,种子 %d,%dx 速度)\n", *cpus, *seed, *speed)
	fmt.Printf("看板: http://localhost%s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
