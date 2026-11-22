package sim

// Recorder 记录原始事件流,供测试离线重算对账:
// 压力累计、节流次数/时长、内存事件、触发器,全部从原始记录独立重算后与模拟器对比。
type Recorder struct {
	On       bool
	Stall    []StallTransition
	Throttle []ThrottleTransition
	Allocs   []AllocEvent
	Kills    []KillEvent
}

// StallTransition 某组某资源某停滞类型的开/关沿。
type StallTransition struct {
	T         int64
	GroupID   int
	Res, Kind int
	On        bool
}

// ThrottleTransition 某组进入/退出节流状态。
type ThrottleTransition struct {
	T       int64
	GroupID int
	On      bool
}

// 分配尝试结果(供离线重算 memory.events)
const (
	AllocOK = iota
	AllocBlockedHigh
	AllocBlockedMax
)

// AllocEvent 一次被限流挡住的分配尝试(只记录被挡的,OK 的量太大不记)。
type AllocEvent struct {
	T       int64
	GroupID int // 挡住它的那个组
	Bytes   int64
	Result  int
}

// KillEvent 一次 OOM 杀。
type KillEvent struct {
	T       int64
	GroupID int
	TaskID  int
	Freed   int64
}

func (s *Sim) recStall(t int64, g *Cgroup, res, kind int, on bool) {
	if s.Rec == nil || !s.Rec.On {
		return
	}
	s.Rec.Stall = append(s.Rec.Stall, StallTransition{T: t, GroupID: g.ID, Res: res, Kind: kind, On: on})
}

func (s *Sim) recThrottle(t int64, g *Cgroup, on bool) {
	if s.Rec == nil || !s.Rec.On {
		return
	}
	s.Rec.Throttle = append(s.Rec.Throttle, ThrottleTransition{T: t, GroupID: g.ID, On: on})
}

func (s *Sim) recAlloc(t int64, g *Cgroup, bytes int64, result int) {
	if s.Rec == nil || !s.Rec.On {
		return
	}
	s.Rec.Allocs = append(s.Rec.Allocs, AllocEvent{T: t, GroupID: g.ID, Bytes: bytes, Result: result})
}

func (s *Sim) recKill(t int64, g *Cgroup, tk *Task, freed int64) {
	if s.Rec == nil || !s.Rec.On {
		return
	}
	s.Rec.Kills = append(s.Rec.Kills, KillEvent{T: t, GroupID: g.ID, TaskID: tk.ID, Freed: freed})
}
