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
