// The interaction graph, drawn in WebGL.
//
// Loaded as a module the moment the graph tab is first opened, and never before:
// three.js is most of a megabyte, and most visits never ask for a picture. Import
// alone is what starts it — everything below the scene setup runs on load, against
// the markup the tab already holds.
//
// three.js is pinned rather than vendored. The version appears twice because a
// static import takes a literal and both must stay in step; esm.sh resolves the
// addon's bare `three` to this same build, which is what keeps one copy in the page.
import * as THREE from 'https://esm.sh/three@0.180.0';
import { OrbitControls } from 'https://esm.sh/three@0.180.0/examples/jsm/controls/OrbitControls.js';

const $ = id => document.getElementById(id);
const short = a => a.slice(0, 6) + '…' + a.slice(-4);

// Drawing budget. Beyond this the picture stops being one, and the browser stops
// being responsive, so the extra edges are dropped and the HUD says how many.
const MAX_EDGES = 40000;

// The host page owns which chain is selected; this asks it rather than keeping a
// second copy that could drift.
function onChain(path) {
  const id = window.evmscanChainID ? window.evmscanChainID() : 0;
  if (!id) return path;
  return path + (path.includes('?') ? '&' : '?') + 'chain_id=' + id;
}

async function api(path) {
  const r = await fetch(onChain(path), { headers: { accept: 'application/json' } });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || body.detail || `${r.status} ${r.statusText}`);
  return body;
}

// --------------------------------------------------------------------------
// Colour: derived from the address so a contract keeps its colour across
// redraws, when the slider changes what else is on screen.
// --------------------------------------------------------------------------
function hueOf(addr) {
  let h = 0;
  for (let i = 2; i < addr.length; i++) h = (h * 31 + addr.charCodeAt(i)) >>> 0;
  return (h % 360) / 360;
}
// The page commits to one palette — printed ink on paper — so the scene does too.
// Reading the OS preference here would drop a dark canvas into a white page.
const DARK = false;
function colorOf(addr) {
  return new THREE.Color().setHSL(hueOf(addr), DARK ? 0.62 : 0.68, DARK ? 0.62 : 0.46);
}
function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

// --------------------------------------------------------------------------
// Model: memberships in, nodes and edges out.
// --------------------------------------------------------------------------
function buildModel(assets) {
  const index = new Map();          // account -> node index
  const nodes = [];                 // { addr, assets:[{ai,events}], events }
  const edges = [];                 // { a, b, asset, weight }
  let dropped = 0;

  assets.forEach((asset, ai) => {
    asset.color = colorOf(asset.address);
    asset.events = 0;
    const ids = [], ev = [];
    for (const m of asset.members) {
      let i = index.get(m.account);
      if (i === undefined) {
        i = nodes.length;
        index.set(m.account, i);
        nodes.push({ addr: m.account, assets: [], events: 0, roles: new Set() });
      }
      const n = nodes[i];
      const count = m.event_count || 0;
      n.assets.push({ ai, events: count });
      n.events += count;
      asset.events += count;
      (m.roles || []).forEach(r => n.roles.add(r));
      ids.push(i);
      ev.push(count);
    }
    // A contract's members are a hyperedge; drawn as the clique over them, which
    // is the plain reading of "the contract connects these accounts".
    for (let x = 0; x < ids.length; x++) {
      for (let y = x + 1; y < ids.length; y++) {
        if (edges.length >= MAX_EDGES) { dropped++; continue; }
        // The weight the index can actually support. Not an amount — the decoder
        // never reads event data, so no token value exists anywhere in here — but
        // the smaller of the two endpoints' event counts, which is the most this
        // pair can have in common on this contract.
        edges.push({ a: ids[x], b: ids[y], asset: ai, weight: Math.min(ev[x], ev[y]) });
      }
    }
  });

  // Degree drives the physics, and it needs the unique pairs: two contracts shared
  // by the same two accounts are two edges to look at but one spring.
  const degree = new Uint32Array(nodes.length);
  const springKeys = new Set();
  const springs = [];
  let maxWeight = 0, maxEvents = 0;
  for (const e of edges) {
    degree[e.a]++; degree[e.b]++;
    if (e.weight > maxWeight) maxWeight = e.weight;
    const key = e.a < e.b ? e.a * nodes.length + e.b : e.b * nodes.length + e.a;
    if (springKeys.has(key)) continue;
    springKeys.add(key);
    springs.push(e.a, e.b);
  }
  for (const n of nodes) if (n.events > maxEvents) maxEvents = n.events;

  return { nodes, edges, springs: new Uint32Array(springs), degree, assets, dropped,
           maxWeight, maxEvents };
}

// Counts here span several orders of magnitude — a router has millions of events
// where an ordinary account has three — so every magnitude is mapped through log,
// or the whole picture would be one bright edge and a field of black ones.
function intensity(v, max) {
  if (!max || v <= 0) return 0;
  return Math.log1p(v) / Math.log1p(max);
}

// 1234567 -> "1.2M". Counts belong in the UI at a glance, not in full.
function compact(n) {
  if (n < 1000) return String(n);
  const units = [['B', 1e9], ['M', 1e6], ['k', 1e3]];
  for (const [suffix, size] of units) {
    if (n >= size) {
      const v = n / size;
      return (v < 10 ? v.toFixed(1) : Math.round(v)) + suffix;
    }
  }
  return String(n);
}

// --------------------------------------------------------------------------
// Layout: Fruchterman–Reingold in three dimensions, with repulsion sampled
// rather than exhaustive so the cost stays linear in the node count. It runs to
// a fixed budget and stops; a graph that never settles is a screensaver.
// --------------------------------------------------------------------------
// seed, when given, maps an account address to where it already sits. Accounts in
// it keep their place and the layout only has to find room for the new ones, so
// loading another page reads as growth rather than as a different picture.
function makeLayout(model, seed) {
  const n = model.nodes.length;
  const pos = new Float32Array(n * 3);
  const disp = new Float32Array(n * 3);

  const spread = 40 * Math.cbrt(Math.max(n, 8));
  let reused = 0;
  for (let i = 0; i < n; i++) {
    const at = seed && seed.get(model.nodes[i].addr);
    if (at) {
      pos[i * 3] = at[0]; pos[i * 3 + 1] = at[1]; pos[i * 3 + 2] = at[2];
      reused++;
      continue;
    }
    // Rejection-free spherical sampling: uniform direction, cube-root radius.
    const u = Math.random() * 2 - 1, th = Math.random() * Math.PI * 2;
    const r = spread * Math.cbrt(Math.random()), s = Math.sqrt(1 - u * u);
    pos[i * 3] = r * s * Math.cos(th);
    pos[i * 3 + 1] = r * s * Math.sin(th);
    pos[i * 3 + 2] = r * u;
  }
  // How far a seeded layout is allowed to move: enough to absorb the new nodes,
  // not enough to rearrange what someone is already looking at.
  const warm = reused / Math.max(n, 1);

  // Hubs pull the whole layout into shape, so every node is pushed away from them
  // every step; everything else is sampled.
  const hubs = Array.from({ length: n }, (_, i) => i)
    .sort((a, b) => model.degree[b] - model.degree[a])
    .slice(0, Math.min(24, n));
  const SAMPLES = Math.min(20, Math.max(0, n - 1));

  const k = spread / Math.cbrt(Math.max(n, 8)) * 2.2;
  const k2 = k * k;
  let temp = spread * 0.14 * (1 - warm * 0.72);
  const TOTAL = 420;
  let done = 0;

  function repel(i, j) {
    if (i === j) return;
    let dx = pos[i * 3] - pos[j * 3], dy = pos[i * 3 + 1] - pos[j * 3 + 1], dz = pos[i * 3 + 2] - pos[j * 3 + 2];
    let d2 = dx * dx + dy * dy + dz * dz;
    if (d2 < 1e-4) { dx = Math.random() - .5; dy = Math.random() - .5; dz = Math.random() - .5; d2 = 0.25; }
    const d = Math.sqrt(d2);
    // Scaled by n/samples so a sampled sum estimates the exhaustive one.
    const f = (k2 / d) * (n / (SAMPLES + hubs.length)) * (1 + model.degree[j] * 0.05);
    disp[i * 3] += dx / d * f; disp[i * 3 + 1] += dy / d * f; disp[i * 3 + 2] += dz / d * f;
  }

  function step() {
    disp.fill(0);
    for (let i = 0; i < n; i++) {
      for (const h of hubs) repel(i, h);
      for (let s = 0; s < SAMPLES; s++) repel(i, (Math.random() * n) | 0);
    }
    const sp = model.springs;
    for (let e = 0; e < sp.length; e += 2) {
      const i = sp[e], j = sp[e + 1];
      let dx = pos[i * 3] - pos[j * 3], dy = pos[i * 3 + 1] - pos[j * 3 + 1], dz = pos[i * 3 + 2] - pos[j * 3 + 2];
      const d = Math.hypot(dx, dy, dz) || 1e-3;
      // Damped by the endpoints' degree. A contract's members form a clique, so
      // without this the busiest contracts out-pull everything and the whole graph
      // collapses into one ball — which is a picture of nothing in particular.
      const f = d * d / k / Math.sqrt(model.degree[i] * model.degree[j] || 1);
      const ux = dx / d * f, uy = dy / d * f, uz = dz / d * f;
      disp[i * 3] -= ux; disp[i * 3 + 1] -= uy; disp[i * 3 + 2] -= uz;
      disp[j * 3] += ux; disp[j * 3 + 1] += uy; disp[j * 3 + 2] += uz;
    }
    for (let i = 0; i < n; i++) {
      // Gravity, so components with no edge between them do not drift apart
      // forever. Weak: it only has to beat the sampling noise.
      disp[i * 3] -= pos[i * 3] * 0.12;
      disp[i * 3 + 1] -= pos[i * 3 + 1] * 0.12;
      disp[i * 3 + 2] -= pos[i * 3 + 2] * 0.12;
      const d = Math.hypot(disp[i * 3], disp[i * 3 + 1], disp[i * 3 + 2]) || 1e-6;
      const m = Math.min(d, temp) / d;
      pos[i * 3] += disp[i * 3] * m;
      pos[i * 3 + 1] += disp[i * 3 + 1] * m;
      pos[i * 3 + 2] += disp[i * 3 + 2] * m;
    }
    temp = Math.max(temp * 0.982, k * 0.004);
    done++;
  }

  return {
    pos,
    get settled() { return done >= TOTAL; },
    get progress() { return done / TOTAL; },
    // A frame budget rather than a step count: the same wall-clock feel whether
    // the graph has fifty nodes or five thousand.
    run(budgetMs) {
      const until = performance.now() + budgetMs;
      while (done < TOTAL && performance.now() < until) step();
    },
  };
}

// --------------------------------------------------------------------------
// Scene
// --------------------------------------------------------------------------
const holder = $('canvas-holder');
const renderer = new THREE.WebGLRenderer({ antialias: true });
renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
holder.appendChild(renderer.domElement);

const scene = new THREE.Scene();
scene.background = new THREE.Color(cssVar('--scene') || '#101114');

const camera = new THREE.PerspectiveCamera(55, 1, 1, 20000);
camera.position.set(0, 0, 600);

const controls = new OrbitControls(camera, renderer.domElement);
controls.enableDamping = true;
controls.dampingFactor = 0.08;
controls.addEventListener('start', () => { userMovedCamera = true; });
controls.addEventListener('change', () => pick());

scene.add(new THREE.AmbientLight(0xffffff, DARK ? 1.5 : 2.1));
const key = new THREE.DirectionalLight(0xffffff, DARK ? 2.0 : 1.4);
key.position.set(1, 1, 1);
scene.add(key);

const NODE_GEO = new THREE.IcosahedronGeometry(1, 2);
const NODE_MAT = new THREE.MeshLambertMaterial();
let mesh = null, lines = null, model = null, layout = null;
let edgePos = null, edgeCol = null, baseEdgeCol = null;
let highlight = -1;   // asset index whose edges are isolated, or -1

const dummy = new THREE.Object3D();

function disposeGraph() {
  if (mesh) { scene.remove(mesh); mesh.dispose(); mesh = null; }
  if (lines) { scene.remove(lines); lines.geometry.dispose(); lines.material.dispose(); lines = null; }
}

function buildScene(m) {
  disposeGraph();
  const n = m.nodes.length;

  mesh = new THREE.InstancedMesh(NODE_GEO, NODE_MAT, n);
  mesh.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
  paintNodes();      // the first setColorAt is what allocates instanceColor
  scene.add(mesh);

  edgePos = new Float32Array(m.edges.length * 6);
  edgeCol = new Float32Array(m.edges.length * 6);
  baseEdgeCol = new Float32Array(m.edges.length * 6);
  // A line's brightness is its weight. WebGL will not give LineSegments a real
  // per-segment width (linewidth is 1 on every desktop driver), so intensity is the
  // channel available — faded toward the scene's own ground, which makes a weak edge
  // recede in the light theme as well as the dark one.
  const ground = new THREE.Color(cssVar('--scene') || '#101114');
  for (let e = 0; e < m.edges.length; e++) {
    const c = scratch.copy(m.assets[m.edges[e].asset].color)
      .lerp(ground, 0.82 * (1 - intensity(m.edges[e].weight, m.maxWeight)));
    for (let v = 0; v < 2; v++) {
      baseEdgeCol[e * 6 + v * 3] = c.r;
      baseEdgeCol[e * 6 + v * 3 + 1] = c.g;
      baseEdgeCol[e * 6 + v * 3 + 2] = c.b;
    }
  }
  edgeCol.set(baseEdgeCol);

  const g = new THREE.BufferGeometry();
  g.setAttribute('position', new THREE.BufferAttribute(edgePos, 3).setUsage(THREE.DynamicDrawUsage));
  g.setAttribute('color', new THREE.BufferAttribute(edgeCol, 3).setUsage(THREE.DynamicDrawUsage));
  lines = new THREE.LineSegments(g, new THREE.LineBasicMaterial({
    vertexColors: true, transparent: true, opacity: DARK ? 0.5 : 0.42, depthWrite: false,
  }));
  scene.add(lines);
}

function syncPositions() {
  const p = layout.pos, m = model;
  for (let i = 0; i < m.nodes.length; i++) {
    dummy.position.set(p[i * 3], p[i * 3 + 1], p[i * 3 + 2]);
    // Sized by events, not degree: degree is an artefact of how many accounts of a
    // contract were drawn, while the event count is a fact about the account.
    const s = 2.2 + intensity(m.nodes[i].events, m.maxEvents) * 9;
    dummy.scale.setScalar(i === selected ? s * 1.9 : s);
    dummy.updateMatrix();
    mesh.setMatrixAt(i, dummy.matrix);
  }
  mesh.instanceMatrix.needsUpdate = true;
  mesh.computeBoundingSphere();

  for (let e = 0; e < m.edges.length; e++) {
    const { a, b } = m.edges[e];
    edgePos[e * 6] = p[a * 3]; edgePos[e * 6 + 1] = p[a * 3 + 1]; edgePos[e * 6 + 2] = p[a * 3 + 2];
    edgePos[e * 6 + 3] = p[b * 3]; edgePos[e * 6 + 4] = p[b * 3 + 1]; edgePos[e * 6 + 5] = p[b * 3 + 2];
  }
  lines.geometry.attributes.position.needsUpdate = true;
  lines.geometry.computeBoundingSphere();
}

// An account's colour is the contract it is most active in, so a cluster reads as
// "these accounts are here because of that token". Accounts spread across many
// contracts drift toward neutral, because no single colour is true of them.
const INK_NODE = new THREE.Color(DARK ? 0x8d8d86 : 0x55554f);
const DIM_NODE = new THREE.Color(DARK ? 0x2a2c31 : 0xd4d4cf);
const scratch = new THREE.Color();

function paintNodes() {
  if (!mesh) return;
  for (let i = 0; i < model.nodes.length; i++) {
    const own = model.nodes[i].assets;
    if (highlight >= 0) {
      // Isolating a contract has to move the nodes too. Forty-five lit edges out of
      // several thousand is not a visible difference; forty-five lit *accounts* is.
      scratch.copy(own.some(o => o.ai === highlight) ? model.assets[highlight].color : DIM_NODE);
    } else if (own.length) {
      scratch.copy(model.assets[own[0].ai].color);
      if (own.length > 1) scratch.lerp(INK_NODE, Math.min(0.35, (own.length - 1) * 0.09));
    } else {
      scratch.copy(INK_NODE);
    }
    mesh.setColorAt(i, scratch);
  }
  if (mesh.instanceColor) mesh.instanceColor.needsUpdate = true;
}

function applyHighlight() {
  if (!lines) return;
  if (highlight < 0) {
    edgeCol.set(baseEdgeCol);
  } else {
    for (let e = 0; e < model.edges.length; e++) {
      const on = model.edges[e].asset === highlight;
      for (let v = 0; v < 6; v++) {
        edgeCol[e * 6 + v] = on ? baseEdgeCol[e * 6 + v] : baseEdgeCol[e * 6 + v] * (DARK ? 0.06 : 0.10);
      }
    }
  }
  lines.geometry.attributes.color.needsUpdate = true;
  paintNodes();
}

function frameAll() {
  if (!model || !model.nodes.length) return;
  const box = new THREE.Box3();
  const v = new THREE.Vector3();
  for (let i = 0; i < model.nodes.length; i++) {
    box.expandByPoint(v.set(layout.pos[i * 3], layout.pos[i * 3 + 1], layout.pos[i * 3 + 2]));
  }
  const c = box.getCenter(new THREE.Vector3());
  const size = box.getSize(new THREE.Vector3());
  const r = Math.max(size.length() / 2, 40);

  // Fit the box, not its bounding sphere: a cloud that is deeper than it is wide
  // has a sphere radius far larger than what the camera actually has to cover, and
  // backing off by that much leaves the graph a smudge in the middle of the canvas.
  // The near face sits at hz in front of the centre, so the standoff is added to it.
  const tanV = Math.tan(camera.fov * Math.PI / 360);
  const tanH = tanV * Math.max(camera.aspect, 0.2);
  const dist = size.z / 2 + Math.max(size.y / 2 / tanV, size.x / 2 / tanH) * 1.12;

  controls.target.copy(c);
  camera.position.copy(c).add(new THREE.Vector3(0, 0, Math.max(dist, 30)));
  camera.near = Math.max(r / 500, 0.5);
  camera.far = r * 60;
  camera.updateProjectionMatrix();
  scene.fog = new THREE.Fog(cssVar('--scene-fog') || '#101114', dist * 0.9, dist + r * 2.6);
}

function resize() {
  const w = holder.clientWidth, h = holder.clientHeight;
  if (!w || !h) return;
  renderer.setSize(w, h, false);
  camera.aspect = w / h;
  camera.updateProjectionMatrix();
}
new ResizeObserver(resize).observe(holder);

// --------------------------------------------------------------------------
// Picking
// --------------------------------------------------------------------------
const raycaster = new THREE.Raycaster();
const pointer = new THREE.Vector2();
let hovered = -1, selected = -1, pointerInside = false;

renderer.domElement.addEventListener('pointermove', ev => {
  const r = holder.getBoundingClientRect();
  pointer.x = ((ev.clientX - r.left) / r.width) * 2 - 1;
  pointer.y = -((ev.clientY - r.top) / r.height) * 2 + 1;
  $('tip').style.left = (ev.clientX - r.left) + 'px';
  $('tip').style.top = (ev.clientY - r.top) + 'px';
  pointerInside = true;
  // Picked here rather than in the render loop: the answer only changes when the
  // pointer or the camera moves, so raycasting every frame is work for nothing.
  pick();
});
renderer.domElement.addEventListener('pointerleave', () => { pointerInside = false; hovered = -1; $('tip').style.display = 'none'; });
renderer.domElement.addEventListener('click', () => { if (hovered >= 0) select(hovered); });

function pick() {
  if (!mesh || !pointerInside) return;
  raycaster.setFromCamera(pointer, camera);
  const hit = raycaster.intersectObject(mesh, false)[0];
  const id = hit ? hit.instanceId : -1;
  if (id === hovered) return;
  hovered = id;
  const tip = $('tip');
  if (id < 0) { tip.style.display = 'none'; renderer.domElement.style.cursor = 'grab'; return; }
  const n = model.nodes[id];
  tip.textContent = `${short(n.addr)} · ${compact(n.events)} events · ${compact(model.degree[id])} edges`;
  tip.style.display = 'block';
  renderer.domElement.style.cursor = 'pointer';
}

// Selecting an account shows two different kinds of fact, and they are labelled as
// two different kinds. The counts come from the index — folded events, no value in
// them. The balances are read live from the node with one deployless eth_call at
// head (the same lens behind /v1/accounts/{addr}), so they are real token amounts,
// current rather than historical, and never derived from the indexed events.
let selectToken = 0;

function select(i) {
  selected = i;
  const n = model.nodes[i];
  const own = [...n.assets].sort((x, y) => y.events - x.events);
  const row = o => {
    const t = model.assets[o.ai];
    return `<div class="kv" data-asset="${t.address.toLowerCase()}">
      <span class="k"><span class="swatch" style="display:inline-block;background:#${t.color.getHexString()}"></span>
        ${t.symbol || short(t.address)}</span>
      <span class="v"><span class="bal">—</span>
        <span class="ct" style="margin-left:6px">${compact(o.events)} ev</span></span></div>`;
  };
  $('sel').innerHTML = `
    <div class="addr">${n.addr}</div>
    <div class="kv"><span class="k">edges</span><span class="v">${model.degree[i].toLocaleString()}</span></div>
    <div class="kv"><span class="k">contracts here</span><span class="v">${own.length}</span></div>
    <div class="kv"><span class="k">events indexed</span><span class="v">${n.events.toLocaleString()}</span></div>
    <div style="margin:8px 0 4px">${[...n.roles].map(r => `<span class="tag">${r}</span>`).join('')}</div>
    <div id="bal-head" class="hint" style="margin:8px 0 2px">reading balances at head…</div>
    ${own.map(row).join('')}
    <div id="bal-total"></div>
    <a href="#" data-open-wallet="${n.addr}" style="display:inline-block;margin-top:8px">Open in the wallet view →</a>`;
  const open = $('sel').querySelector('[data-open-wallet]');
  if (open) {
    open.addEventListener('click', ev => {
      ev.preventDefault();
      window.evmscanOpenWallet(open.dataset.openWallet);
    });
  }

  loadBalances(n.addr, ++selectToken);
}

// Raw base units to something a person can read. Small balances keep their
// significant digits instead of rounding to a flat zero.
function formatUnits(raw, decimals) {
  const d = decimals || 0;
  const neg = raw.startsWith('-');
  const digits = (neg ? raw.slice(1) : raw).padStart(d + 1, '0');
  const whole = digits.slice(0, digits.length - d) || '0';
  let frac = d ? digits.slice(digits.length - d).replace(/0+$/, '') : '';
  if (whole === '0' && frac) {
    const lead = frac.length - frac.replace(/^0+/, '').length;
    frac = frac.slice(0, lead + 4);
  } else if (frac.length > 4) {
    frac = frac.slice(0, 4);
  }
  return (neg ? '-' : '') + BigInt(whole).toLocaleString() + (frac ? '.' + frac : '');
}

async function loadBalances(addr, token) {
  try {
    const d = await api(`/v1/accounts/${addr}?prices=true`);
    if (token !== selectToken) return;      // a different account is selected now
    const byAsset = new Map((d.assets || []).map(a => [a.address.toLowerCase(), a]));
    for (const el of $('sel').querySelectorAll('[data-asset]')) {
      const a = byAsset.get(el.dataset.asset);
      const slot = el.querySelector('.bal');
      if (!a) { slot.textContent = '—'; continue; }
      if (a.balance == null) {
        slot.textContent = a.balance_error ? 'unreadable' : '—';
        slot.className = 'bal empty';
        continue;
      }
      slot.textContent = formatUnits(a.balance, a.decimals);
      if (a.value_usd) slot.title = `$${a.value_usd}`;
    }
    const head = $('bal-head');
    if (head) {
      head.innerHTML = `balances read live at block ${(d.as_of_block || 0).toLocaleString()}`;
    }
    const v = d.valuation;
    if (v && v.total_usd && $('bal-total')) {
      $('bal-total').innerHTML =
        `<div class="kv" style="border-top:1px solid var(--line);margin-top:6px;padding-top:6px">
           <span class="k">valued at head</span><span class="v">$${v.total_usd}</span></div>`;
    }
  } catch (e) {
    if (token !== selectToken) return;
    const head = $('bal-head');
    // The counts above are still true; only the live read failed. Say which.
    if (head) head.textContent = `balances unavailable (${e.message || e})`;
  }
}

// --------------------------------------------------------------------------
// Legend
// --------------------------------------------------------------------------
function drawLegend(m) {
  $('legend-count').textContent = m.assets.length ? `(${m.assets.length})` : '';
  $('legend').innerHTML = m.assets.map((a, i) => `
    <li data-i="${i}" title="${a.address}">
      <span class="swatch" style="background:#${a.color.getHexString()}"></span>
      <span class="nm">${a.symbol || short(a.address)}${a.name ? ` <span class="ct">${a.name}</span>` : ''}</span>
      <span class="ct">${a.members.length}${a.holders > a.members.length ? `/${a.holders}` : ''}
        · ${compact(a.events)}</span>
    </li>`).join('') || '<li class="empty">No contract has an indexed account yet.</li>';

  for (const li of $('legend').querySelectorAll('li[data-i]')) {
    const i = +li.dataset.i;
    li.addEventListener('pointerenter', () => { highlight = i; applyHighlight(); li.classList.add('on'); });
    li.addEventListener('pointerleave', () => { highlight = -1; applyHighlight(); li.classList.remove('on'); });
    li.addEventListener('click', () => {
      // Frame the contract's own members, which is the only way to find a small
      // token inside a graph a big one dominates.
      const box = new THREE.Box3(), v = new THREE.Vector3();
      for (let x = 0; x < m.nodes.length; x++) {
        if (!m.nodes[x].assets.some(o => o.ai === i)) continue;
        box.expandByPoint(v.set(layout.pos[x * 3], layout.pos[x * 3 + 1], layout.pos[x * 3 + 2]));
      }
      if (box.isEmpty()) return;
      const c = box.getCenter(new THREE.Vector3());
      const r = Math.max(box.getSize(new THREE.Vector3()).length() / 2, 30);
      controls.target.copy(c);
      camera.position.copy(c).add(new THREE.Vector3(0, 0, r / Math.tan(camera.fov * Math.PI / 360) * 1.6));
    });
  }
}

// --------------------------------------------------------------------------
// Load
// --------------------------------------------------------------------------
// hudText is the standing description of what is drawn; the settling percentage is
// appended to it each frame rather than overwriting it.
let hudText = '';
function hud(text) { hudText = text; $('hud').textContent = text; }

// A layout that re-frames itself is helpful until someone has aimed the camera
// themselves, at which point it is rude.
let userMovedCamera = false;

// Everything drawn so far, and where to resume. Redraw resets both; Load more
// appends the next page to them.
let loaded = [];
let nextOffset = 0;
let total = 0;

async function load({ append = false } = {}) {
  const btn = append ? $('more') : $('reload');
  $('reload').disabled = $('more').disabled = true;
  $('err').textContent = '';
  if (!append) {
    $('overlay').classList.remove('gone');
    $('overlay').textContent = 'reading the index…';
  }
  const label = btn.textContent;
  btn.textContent = 'loading…';
  try {
    const page = +$('assets-range').value, nHolders = +$('holders').value;
    if (!append) { loaded = []; nextOffset = 0; }
    const data = await api(`/v1/graph?assets=${page}&offset=${nextOffset}&holders=${nHolders}`);
    const got = data.assets || [];
    // Contracts only ever arrive once: the server order is stable, but a redraw
    // after the index has grown could still overlap a page boundary.
    const seen = new Set(loaded.map(a => a.address));
    loaded = loaded.concat(got.filter(a => !seen.has(a.address)));
    nextOffset += got.length;
    total = data.total || loaded.length;

    // Keep where every account already sits, so appending a page grows the picture
    // rather than throwing it in the air and starting again.
    const seed = append && model && layout ? positionsByAddress() : null;

    model = buildModel(loaded);
    selected = -1; hovered = -1; highlight = -1;
    $('sel').innerHTML = '<span class="empty">Click an account to inspect it.</span>';
    drawLegend(model);
    updatePaging(data);

    if (!model.nodes.length) {
      disposeGraph();
      layout = null;
      $('overlay').textContent =
        'Nothing to draw yet: no promoted contract has an indexed account on this chain. ' +
        'Register or promote a contract, let the follower run, then redraw.';
      hud('');
      return;
    }

    buildScene(model);
    layout = makeLayout(model, seed);
    layout.run(120);              // a first shape before the camera is placed
    syncPositions();
    if (!append) {
      userMovedCamera = false;
      resize();        // before framing: the fit depends on the aspect ratio
      frameAll();
    }
    $('overlay').classList.add('gone');
    hud(`${model.nodes.length.toLocaleString()} accounts · ${model.edges.length.toLocaleString()} edges · `
      + `${model.assets.length} of ${total.toLocaleString()} contracts · chain ${data.chain_id}`
      + (model.dropped ? ` · ${model.dropped.toLocaleString()} edges over the drawing budget were dropped` : ''));
  } catch (e) {
    $('err').textContent = String(e.message || e);
    if (!append) $('overlay').textContent = 'Could not read /v1/graph.';
  } finally {
    btn.textContent = label;
    $('reload').disabled = false;
    updatePaging();
  }
}

function positionsByAddress() {
  const m = new Map();
  for (let i = 0; i < model.nodes.length; i++) {
    m.set(model.nodes[i].addr, [layout.pos[i * 3], layout.pos[i * 3 + 1], layout.pos[i * 3 + 2]]);
  }
  return m;
}

function updatePaging(data) {
  const drawn = model ? model.assets.length : 0;
  const more = data ? data.has_more : drawn < total;
  $('more').disabled = !more;
  $('more').textContent = more
    ? `Load ${Math.min(+$('assets-range').value, total - drawn)} more`
    : (drawn ? 'All contracts loaded' : 'Load more');
  $('paging').textContent = total
    ? `${drawn.toLocaleString()} of ${total.toLocaleString()} contracts drawn`
    : '';
}

renderer.setAnimationLoop(() => {
  if (layout && !layout.settled) {
    layout.run(11);
    syncPositions();
    // The first framing happens before the layout has found its size, so take the
    // camera back once it has stopped moving. Only if nobody has grabbed it since:
    // yanking the view out from under someone mid-drag is worse than a loose frame.
    if (layout.settled && !userMovedCamera) frameAll();
    // Written straight to the element, not through hud(): the progress suffix is
    // transient, and folding it back into hudText would make it accumulate.
    $('hud').textContent = hudText + (layout.settled ? '' : ` · settling ${Math.round(layout.progress * 100)}%`);
  } else if (mesh && selected >= 0) {
    syncPositions();
  }
  controls.update();
  renderer.render(scene, camera);
});

$('assets-range').addEventListener('input', e => {
  $('assets-val').textContent = e.target.value;
  updatePaging();   // the page size is also the size of the next Load more
});
$('holders').addEventListener('input', e => {
  const k = +e.target.value;
  $('holders-val').textContent = `${k} → ${k * (k - 1) / 2} edges each`;
});
$('reload').addEventListener('click', () => load());
$('more').addEventListener('click', () => load({ append: true }));
$('recenter').addEventListener('click', frameAll);
$('holders').dispatchEvent(new Event('input'));

resize();
load();

// The host calls this when the selected chain changes: the scene is about one
// chain's index, and keeping the old one on screen under a new chain's name would
// be the worst of both.
window.evmscanGraphReload = () => { loaded = []; nextOffset = 0; load(); };
