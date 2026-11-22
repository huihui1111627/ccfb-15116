package web

// indexHTML 是单文件看板: 原生 JS + Canvas, 无外部依赖。
// 布局四块:
//  1. 控制组层级树 (含冻结/解冻按钮、节流/冻结状态徽标)
//  2. 每组处理器配额与实际用量、本地池、节流统计
//  3. 内存三档用量与回收/OOM 统计
//  4. 每组三类资源 (CPU/内存/IO) 的 some/full 压力曲线
const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>竹编工坊 · 控制组资源模拟器</title>
<style>
  :root {
    --bg:#0f1419; --panel:#171d24; --line:#24303c; --txt:#d7e0ea;
    --dim:#7d8da0; --accent:#4cc2ff; --warn:#ffb454; --bad:#ff6b6b;
    --ok:#7ee787; --purple:#c792ea;
  }
  * { box-sizing:border-box; }
  body { margin:0; background:var(--bg); color:var(--txt);
    font:13px/1.5 -apple-system,"PingFang SC","Microsoft YaHei",monospace; }
  header { padding:12px 20px; border-bottom:1px solid var(--line);
    display:flex; gap:18px; align-items:center; flex-wrap:wrap; }
  header h1 { font-size:15px; margin:0; font-weight:600; }
  .stat { color:var(--dim); }
  .stat b { color:var(--accent); font-weight:600; }
  .controls button { background:#1f2a36; color:var(--txt); border:1px solid var(--line);
    border-radius:5px; padding:5px 12px; cursor:pointer; margin-left:6px; }
  .controls button:hover { border-color:var(--accent); }
  main { display:grid; grid-template-columns:300px 1fr; gap:14px; padding:14px 20px; }
  .panel { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:12px; }
  .panel h2 { font-size:12px; color:var(--dim); margin:0 0 10px; font-weight:600;
    text-transform:uppercase; letter-spacing:.08em; }
  .tree { font-size:13px; }
  .tree .node { padding:3px 0; cursor:pointer; border-radius:4px; padding-left:4px; }
  .tree .node:hover { background:#1d2733; }
  .tree .node.active { background:#1d3344; color:var(--accent); }
  .tree .indent { display:inline-block; }
  .badge { display:inline-block; font-size:10px; padding:1px 6px; border-radius:8px;
    margin-left:6px; vertical-align:middle; }
  .badge.thr { background:#3d2424; color:var(--bad); }
  .badge.frz { background:#2d2a3d; color:var(--purple); }
  .right { display:flex; flex-direction:column; gap:14px; min-width:0; }
  table { width:100%; border-collapse:collapse; font-size:12px; }
  th,td { text-align:left; padding:5px 8px; border-bottom:1px solid var(--line);
    white-space:nowrap; }
  th { color:var(--dim); font-weight:500; }
  td.num { text-align:right; font-variant-numeric:tabular-nums; }
  .bar { height:7px; background:#10171e; border-radius:4px; overflow:hidden; min-width:90px; }
  .bar i { display:block; height:100%; background:var(--accent); }
  .bar i.warn { background:var(--warn); }
  .bar i.bad { background:var(--bad); }
  .grid2 { display:grid; grid-template-columns:1fr 1fr; gap:14px; }
  .chart-wrap { margin-bottom:10px; }
  .chart-wrap h3 { font-size:12px; margin:0 0 4px; color:var(--dim); font-weight:500; }
  canvas { width:100%; height:110px; background:#0c1117; border-radius:6px; }
  .legend { font-size:11px; color:var(--dim); margin-top:3px; }
  .legend span { margin-right:12px; }
  .dot { display:inline-block; width:8px; height:8px; border-radius:2px; margin-right:4px; }
  .memlines { display:flex; gap:14px; flex-wrap:wrap; font-size:11px; color:var(--dim); }
  .pool { font-size:11px; color:var(--dim); }
  button.fz { font-size:11px; padding:2px 8px; }
  .section-title { display:flex; justify-content:space-between; align-items:center; }
  .pill { font-size:11px; color:var(--dim); }
</style>
</head>
<body>
<header>
  <h1>竹编工坊 · 控制组资源模拟器</h1>
  <span class="stat">tick <b id="tick">0</b> / <b id="total">0</b></span>
  <span class="stat">CPU <b id="cpus">0</b></span>
  <span class="stat">事件 <b id="evcnt">0</b></span>
  <span class="controls" style="margin-left:auto">
    <button id="play">⏸ 暂停</button>
    <button id="step">单步</button>
    <button id="speed">速度 ×1</button>
  </span>
</header>

<main>
  <div class="panel">
    <h2>控制组层级树</h2>
    <div class="tree" id="tree"></div>
  </div>
  <div class="right">
    <div class="panel">
      <div class="section-title">
        <h2 style="margin:0">处理器配额与实际用量 — <span id="sel-group" class="pill"></span></h2>
        <button class="fz" id="freeze-btn">冻结整组</button>
      </div>
      <table id="cpu-table"><thead><tr>
        <th>组</th><th class="num">权重</th><th class="num">周期/配额(ms)</th>
        <th class="num">本周期已用</th><th>配额水位</th>
        <th class="num">累计CPU(ms)</th><th class="num">节流次数</th>
        <th class="num">节流时长(ms)</th><th>本地池</th>
      </tr></thead><tbody></tbody></table>
    </div>

    <div class="panel">
      <h2>内存三档用量与回收</h2>
      <table id="mem-table"><thead><tr>
        <th>组</th><th class="num">保护线 low</th><th class="num">软上限 soft</th>
        <th class="num">硬上限 hard</th><th class="num">当前用量</th>
        <th>硬上限水位</th><th class="num">累计回收</th><th class="num">OOM 杀进程</th>
      </tr></thead><tbody></tbody></table>
      <div class="memlines" id="memlines"></div>
    </div>

    <div class="panel">
      <h2>压力曲线 (PSI · some 实线 / full 虚线)</h2>
      <div id="charts"></div>
    </div>
  </div>
</main>

<script>
const RES = ["cpu","memory","io"];
const RES_CN = ["处理器","内存回收","读写"];
const COLORS = ["#4cc2ff","#ffb454","#c792ea"];
let snap = null, selected = null, playing = true, speedIdx = 0;
const SPEEDS = [1, 2, 4, 8];
const STEP_MS = 120;

const fmt = n => n == null ? "—" :
  n >= 1<<30 ? (n/(1<<30)).toFixed(2)+" GiB" :
  n >= 1<<20 ? (n/(1<<20)).toFixed(1)+" MiB" :
  n >= 1<<10 ? (n/(1<<10)).toFixed(1)+" KiB" : String(n);

async function load() {
  const r = await fetch("/api/snapshot");
  snap = await r.json();
  if (!selected) selected = snap.root;
  render();
}
function subscribe() {
  const es = new EventSource("/api/events");
  es.onmessage = ev => { snap = JSON.parse(ev.data); render(); };
  es.onerror = () => setTimeout(subscribe, 1000);
}

function groupMap() { const m = {}; snap.groups.forEach(g => m[g.name] = g); return m; }

function renderTree() {
  const m = groupMap();
  const root = m[snap.root];
  const el = document.getElementById("tree");
  el.innerHTML = "";
  const walk = (g, depth) => {
    const row = document.createElement("div");
    row.className = "node" + (g.name === selected ? " active" : "");
    row.innerHTML = '<span class="indent" style="width:'+depth*14+'px"></span>' +
      (g.children.length ? "▸ " : "· ") + g.name +
      (g.throttled ? '<span class="badge thr">节流</span>' : "") +
      (g.frozen ? '<span class="badge frz">冻结</span>' : "");
    row.onclick = () => { selected = g.name; render(); };
    el.appendChild(row);
    g.children.forEach(c => walk(m[c], depth+1));
  };
  walk(root, 0);
}

function cpuRow(g) {
  const pq = g.quota > 0;
  const periodUsed = pq ? g.period_usage : 0;
  const waterPct = pq ? Math.min(100, periodUsed / g.quota * 100) : 0;
  const cls = waterPct > 90 ? "bad" : waterPct > 70 ? "warn" : "";
  const pools = g.local_pools.map(p => "CPU"+p.cpu+":"+p.local+"ms").join(" ") || "—";
  return "<tr"+(g.name===selected?' style="background:#1d3344"':"")+"><td>"+g.name+"</td>"+
    '<td class="num">'+g.weight+"</td>"+
    '<td class="num">'+(pq ? g.period+" / "+g.quota : "不限")+"</td>"+
    '<td class="num">'+(pq ? periodUsed : "—")+"</td>"+
    '<td><div class="bar"><i class="'+cls+'" style="width:'+waterPct+'%"></i></div></td>'+
    '<td class="num">'+g.cpu_used.toFixed(0)+"</td>"+
    '<td class="num" style="color:'+(g.throttle_cnt?'var(--warn)':'inherit')+'">'+g.throttle_cnt+"</td>"+
    '<td class="num">'+g.throttle_ms+"</td>"+
    '<td class="pool">'+pools+"</td></tr>";
}

function memRow(g) {
  const pct = g.mem_hard > 0 ? Math.min(100, g.mem_cur / g.mem_hard * 100) : 0;
  const cls = pct > 90 ? "bad" : pct > 70 ? "warn" : "";
  return "<tr><td>"+g.name+"</td>"+
    '<td class="num">'+fmt(g.mem_low)+"</td>"+
    '<td class="num">'+fmt(g.mem_soft)+"</td>"+
    '<td class="num">'+(g.mem_hard?fmt(g.mem_hard):"不限")+"</td>"+
    '<td class="num">'+fmt(g.mem_cur)+"</td>"+
    '<td><div class="bar"><i class="'+cls+'" style="width:'+pct+'%"></i></div></td>'+
    '<td class="num" style="color:var(--ok)">'+fmt(g.mem_reclaimed)+"</td>"+
    '<td class="num" style="color:'+(g.oom_kills?'var(--bad)':'inherit')+'">'+g.oom_kills+"</td></tr>";
}

function drawChart(canvas, points, res, kind) {
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth, h = canvas.clientHeight;
  canvas.width = w*dpr; canvas.height = h*dpr;
  const ctx = canvas.getContext("2d"); ctx.scale(dpr,dpr);
  ctx.clearRect(0,0,w,h);
  ctx.strokeStyle = "#1c2733"; ctx.lineWidth = 1;
  for (let i=0;i<=4;i++){ const y=h/4*i; ctx.beginPath();
    ctx.moveTo(0,y); ctx.lineTo(w,y); ctx.stroke(); }
  if (!points.length) return;
  const plot = (key, color, dashed) => {
    ctx.beginPath(); ctx.strokeStyle = color; ctx.lineWidth = 1.6;
    ctx.setLineDash(dashed ? [4,3] : []);
    points.forEach((p,i) => {
      const x = points.length===1 ? 0 : i/(points.length-1)*w;
      const y = h - Math.min(100, p[key][res])/100*h;
      i ? ctx.lineTo(x,y) : ctx.moveTo(x,y);
    });
    ctx.stroke(); ctx.setLineDash([]);
  };
  plot("avg_some", COLORS[res], false);
  plot("avg_full", COLORS[res], true);
}

function renderCharts(g) {
  const wrap = document.getElementById("charts");
  wrap.innerHTML = "";
  RES.forEach((r, ri) => {
    const block = document.createElement("div");
    block.className = "chart-wrap";
    block.innerHTML = "<h3>"+RES_CN[ri]+" 压力 · 10s / 1m / 5m EWMA (%)</h3>" +
      '<canvas id="chart-'+r+'"></canvas>' +
      '<div class="legend"><span><i class="dot" style="background:'+COLORS[ri]+'"></i>some 部分停滞</span>'+
      '<span><i class="dot" style="background:'+COLORS[ri]+';opacity:.5"></i>full 全部停滞 (虚线)</span></div>';
    wrap.appendChild(block);
  });
  RES.forEach((r, ri) => {
    const cv = document.getElementById("chart-"+r);
    const points = g.pressure[ri].history.map(pt => ({
      avg_some: pt.avg_some, avg_full: pt.avg_full,
    }));
    drawChart(cv, points, ri, r);
  });
}

function render() {
  document.getElementById("tick").textContent = snap.tick;
  document.getElementById("total").textContent = snap.total_ticks;
  document.getElementById("cpus").textContent = snap.cpus;
  document.getElementById("evcnt").textContent = snap.events.length;
  renderTree();
  const m = groupMap();
  const sel = m[selected] || m[snap.root];
  document.getElementById("sel-group").textContent = sel.name;
  document.getElementById("freeze-btn").textContent = sel.frozen ? "解冻整组" : "冻结整组";

  // CPU 表: 选中组的整条祖先-子树链路, 便于对照层级配额。
  const chain = new Set();
  let cur = sel;
  while (cur) { chain.add(cur.name); cur = cur.parent ? m[cur.parent] : null; }
  sel.children.forEach(c => chain.add(c));
  document.querySelector("#cpu-table tbody").innerHTML =
    snap.groups.filter(g => chain.has(g.name)).map(cpuRow).join("");
  document.querySelector("#mem-table tbody").innerHTML =
    snap.groups.filter(g => chain.has(g.name)).map(memRow).join("");
  document.getElementById("memlines").innerHTML =
    "当前组用量 " + fmt(sel.mem_cur) + " · 保护线 " + fmt(sel.mem_low) +
    " · 软上限 " + fmt(sel.mem_soft) + " · 硬上限 " + (sel.mem_hard?fmt(sel.mem_hard):"不限") +
    " · 累计回收 " + fmt(sel.mem_reclaimed) + " · OOM " + sel.oom_kills +
    " · 冻结 " + sel.frozen_ticks + " ms";
  renderCharts(sel);
}

document.getElementById("freeze-btn").onclick = async () => {
  const g = groupMap()[selected];
  await fetch("/api/freeze", {method:"POST",
    headers:{"Content-Type":"application/json"},
    body: JSON.stringify({group:selected, freeze:!g.frozen})});
  load();
};
document.getElementById("play").onclick = () => {
  playing = !playing;
  document.getElementById("play").textContent = playing ? "⏸ 暂停" : "▶ 播放";
};
document.getElementById("step").onclick = () => { playing = false;
  document.getElementById("play").textContent = "▶ 播放"; fetch("/api/step",{method:"POST"}); };
document.getElementById("speed").onclick = () => {
  speedIdx = (speedIdx+1) % SPEEDS.length;
  document.getElementById("speed").textContent = "速度 ×"+SPEEDS[speedIdx];
};
// 模拟引擎在服务端自动推进, 前端定时拉快照兜底 (SSE 之外的冗余通道),
// 暂停时不发请求, 速度档缩短刷新间隔。
setInterval(() => { if (playing) load(); }, STEP_MS / SPEEDS[speedIdx]);
load(); subscribe();
</script>
</body>
</html>`
