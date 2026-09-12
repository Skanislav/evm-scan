#!/usr/bin/env node
// Compiles contracts/src/*.sol with solc and writes {abi, bytecode} to contracts/out/.
// Kept dependency-light on purpose: the only requirement is the `solc` npm package.
const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const SRC = path.join(ROOT, 'contracts', 'src');
const OUT = path.join(ROOT, 'contracts', 'out');

function loadSolc() {
  const candidates = [
    () => require('solc'),
    () => require(path.join(process.env.SOLC_HOME || '', 'node_modules', 'solc')),
  ];
  for (const c of candidates) {
    try { return c(); } catch (_) { /* try next */ }
  }
  console.error('solc not found. Run: npm --prefix scripts install solc@0.8.28  (or set SOLC_HOME)');
  process.exit(1);
}

const solc = loadSolc();

const sources = {};
for (const f of fs.readdirSync(SRC)) {
  if (f.endsWith('.sol')) sources[f] = { content: fs.readFileSync(path.join(SRC, f), 'utf8') };
}

// Library imports (@openzeppelin/...) are pulled into `sources` under their import
// path rather than handed to solc through an import callback, so the standard input
// written below is complete: an explorer verifies by recompiling exactly this
// document, and a file solc fetched through a callback would be missing from it.
const NODE_MODULES = path.join(process.env.SOLC_HOME || __dirname, 'node_modules');
const importRe = /^\s*import\s+(?:[^'"]*from\s+)?["']([^"']+)["']/gm;
const queue = Object.keys(sources);
while (queue.length) {
  const key = queue.shift();
  for (const m of sources[key].content.matchAll(importRe)) {
    let dep = m[1];
    if (dep.startsWith('.')) {
      // Relative to the importing file's own path, as solc resolves it.
      dep = path.posix.normalize(path.posix.join(path.posix.dirname(key), dep));
    }
    if (sources[dep]) continue;
    const onDisk = dep.startsWith('@') ? path.join(NODE_MODULES, dep) : path.join(SRC, dep);
    if (!fs.existsSync(onDisk)) {
      console.error(`${key} imports ${dep}, which is not in contracts/src or ${NODE_MODULES}`);
      process.exit(1);
    }
    sources[dep] = { content: fs.readFileSync(onDisk, 'utf8') };
    queue.push(dep);
  }
}
if (Object.keys(sources).length === 0) {
  console.error(`no .sol files in ${SRC}`);
  process.exit(1);
}

const input = {
  language: 'Solidity',
  sources,
  settings: {
    optimizer: { enabled: true, runs: 200 },
    evmVersion: 'cancun',
    outputSelection: { '*': { '*': ['abi', 'evm.bytecode.object', 'evm.deployedBytecode.object'] } },
  },
};

const out = JSON.parse(solc.compile(JSON.stringify(input)));

// Emit the exact input this build used. A block explorer verifies by recompiling,
// so it needs the same sources and the same settings — solc version, optimizer runs
// and evmVersion all change the bytecode. Writing the input we actually compiled
// removes any chance of a verification attempt guessing them wrong.
fs.mkdirSync(OUT, { recursive: true });
fs.writeFileSync(
  path.join(OUT, 'standard-input.json'),
  JSON.stringify(input, null, 2) + '\n',
);

// The compiler's own full version, which is the one an explorer wants: a release
// is identified by its commit, and "0.8.28" alone is rejected as unsupported.
// solc reports it with a build suffix that the explorer does not use, so record
// both rather than leaving a script to guess which form is wanted.
const fullVersion = solc.version();
const commitMatch = /^(\d+\.\d+\.\d+\+commit\.[0-9a-f]+)/.exec(fullVersion);
fs.writeFileSync(
  path.join(OUT, 'compiler.json'),
  JSON.stringify({
    version: fullVersion,
    etherscan: commitMatch ? `v${commitMatch[1]}` : null,
    settings: input.settings,
  }, null, 2) + '\n',
);

let failed = false;
for (const err of out.errors || []) {
  const msg = err.formattedMessage || err.message;
  if (err.severity === 'error') { failed = true; console.error(msg); }
  else console.warn(msg);
}
if (failed) process.exit(1);

fs.mkdirSync(OUT, { recursive: true });
let n = 0;
for (const [file, contracts] of Object.entries(out.contracts || {})) {
  for (const [name, c] of Object.entries(contracts)) {
    fs.writeFileSync(
      path.join(OUT, `${name}.json`),
      JSON.stringify({
        contractName: name,
        sourceName: file,
        abi: c.abi,
        bytecode: '0x' + c.evm.bytecode.object,
        deployedBytecode: '0x' + c.evm.deployedBytecode.object,
      }, null, 2) + '\n'
    );
    n++;
    console.log(`compiled ${name}  (${c.evm.deployedBytecode.object.length / 2} bytes deployed)`);
  }
}
console.log(`wrote ${n} artifact(s) to contracts/out/`);
