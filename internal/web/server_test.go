package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cgsim/internal/sim"
)

func testEngine(t *testing.T) *sim.Engine {
	t.Helper()
	scn := sim.Scenario{
		Groups: []sim.GroupSpec{
			{Name: "root", Weight: 100, MemHard: 1 << 30},
			{Name: "a", Parent: "root", Weight: 100,
				Period: 10, Quota: 3, MemHard: 1 << 30},
		},
		Tasks: []sim.TaskSpec{
			{ID: 1, Group: "a", Events: []sim.TaskEvent{{Tick: 0, Type: sim.EvtRun}}},
		},
		PSIWindows: [3]int64{100, 400, 2000}, PSIFlush: 20,
	}
	cfg := sim.DefaultConfig()
	cfg.CPUs = 1
	cfg.Ticks = 500
	e, err := sim.New(scn, cfg, 7)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSnapshotAndIndex(t *testing.T) {
	srv := NewServer(testEngine(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 首页 HTML。
	r, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index status=%d type=%s", r.StatusCode, r.Header.Get("Content-Type"))
	}

	// 快照 JSON 结构。
	r2, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var snap sim.Snapshot
	if err := json.NewDecoder(r2.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Root != "root" || len(snap.Groups) != 2 {
		t.Fatalf("bad snapshot: root=%s groups=%d", snap.Root, len(snap.Groups))
	}
	for _, g := range snap.Groups {
		if g.Children == nil || g.LocalPools == nil {
			t.Fatalf("group %q exposes nil slices", g.Name)
		}
	}
}

func TestFreezeEndpoint(t *testing.T) {
	srv := NewServer(testEngine(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(body string, want int) {
		t.Helper()
		r, err := http.Post(ts.URL+"/api/freeze", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("body=%s status=%d want %d", body, r.StatusCode, want)
		}
	}
	post(`{"group":"a","freeze":true}`, http.StatusOK)
	g, _ := srv.engine.GroupByName("a")
	if !g.FrozenForTest() {
		t.Fatal("group a should be frozen")
	}
	post(`{"group":"a","freeze":true}`, http.StatusNotFound) // 幂等: 状态没变
	post(`{"group":"ghost","freeze":true}`, http.StatusNotFound)
	post(`{"group":"a","freeze":false}`, http.StatusOK)
	if g.FrozenForTest() {
		t.Fatal("group a should be unfrozen")
	}
	post(`{bad json`, http.StatusBadRequest)
}

func TestStepEndpoint(t *testing.T) {
	e := testEngine(t)
	srv := NewServer(e)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	before := e.Now()
	r, err := http.Post(ts.URL+"/api/step", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("step status=%d", r.StatusCode)
	}
	if e.Now() != before+1 {
		t.Fatalf("tick did not advance: %d -> %d", before, e.Now())
	}
}

func TestSSEPushesSnapshots(t *testing.T) {
	e := testEngine(t)
	srv := NewServer(e)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 打开 SSE 连接, 后台推几步, 确认能收到 event-stream 数据帧。
	client := ts.Client()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("not event-stream: %s", resp.Header.Get("Content-Type"))
	}

	go func() {
		for i := 0; i < 5; i++ {
			_ = srv.Step()
			time.Sleep(10 * time.Millisecond)
		}
	}()

	buf := make([]byte, 4096)
	deadline := time.After(2 * time.Second)
	got := false
	for !got {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for SSE frame")
		default:
		}
		n, err := resp.Body.Read(buf)
		if n > 0 && strings.HasPrefix(string(buf[:n]), "data:") {
			got = true
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
