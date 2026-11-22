package sim

// applyControls 处理本 tick 的冻结/解冻控制事件。
// 冻结是整组语义: 组节点打 frozen 标记, 子树所有任务在调度与压力
// 统计中视为不可见; 解冻前累积的冻结 tick 计入 frozenTicks。
func (e *Engine) applyControls() {
	for _, c := range e.controlEvents[e.now] {
		g, ok := e.groupByName[c.Group]
		if !ok {
			continue
		}
		if g.frozen == c.Freeze {
			continue // 幂等: 重复冻结/解冻不产生事件
		}
		g.frozen = c.Freeze
		evType := EvFreeze
		if !c.Freeze {
			evType = EvUnfreeze
		}
		e.events = append(e.events, LogEvent{
			Tick: e.now, Type: evType, Group: g.Spec.Name,
		})
	}
}

// applyTaskEvents 处理本 tick 到点的任务脚本事件。
func (e *Engine) applyTaskEvents() {
	for _, se := range e.taskEvents[e.now] {
		t := e.taskByID[se.taskID]
		if t == nil || !t.alive {
			continue
		}
		switch se.ev.Type {
		case EvtRun:
			// 进入可运行态; 若正在等 IO/内存, run 事件不抢占阻塞。
			if t.state == stSleeping || t.state == stRunning {
				t.state = stRunning
			}
		case EvtIO:
			if t.state == stDead {
				continue
			}
			t.ioWait = int(se.ev.Bytes)
			if t.ioWait < 1 {
				t.ioWait = 1
			}
			t.state = stIOWait
		case EvtAllocAnon, EvtAllocFile:
			if t.state == stDead {
				continue
			}
			k := memAnon
			if se.ev.Type == EvtAllocFile {
				k = memFile
			}
			e.requestAlloc(t, se.ev.Bytes, k)
		case EvtFree:
			e.freeMem(t, se.ev.Bytes)
		case EvtExit:
			e.killTask(t, false)
		}
	}
}

// killTask 把任务从系统里摘除。oom=true 时记 OOM 事件, 释放其全部内存。
func (e *Engine) killTask(t *Task, oom bool) {
	if t.state == stDead {
		return
	}
	t.state = stDead
	t.alive = false
	t.Mem = taskMem{}
	t.memWait = 0
	t.ioWait = 0
	if oom {
		t.Group.oomKills++
	}
}

func (e *Engine) freeMem(t *Task, bytes int64) {
	if bytes <= 0 || bytes >= t.Mem.total() {
		t.Mem = taskMem{}
		return
	}
	// 先减匿名, 再减文件 (确定性顺序, 测试可手算)。
	if bytes <= t.Mem.anon {
		t.Mem.anon -= bytes
		return
	}
	bytes -= t.Mem.anon
	t.Mem.anon = 0
	t.Mem.file -= bytes
}
