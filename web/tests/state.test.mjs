import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import * as viem from "viem";
import { codec, chooseRevision, seed, ZERO } from "../userstate.mjs";
const c = codec(viem);
const f = JSON.parse(
  await readFile(
    new URL("../../internal/userstate/testdata/state.json", import.meta.url),
    "utf8",
  ),
);
test("Go snapshot, signature, empty root and aggregate match independent JS", async () => {
  assert.equal(await c.verify(f.snapshot), f.id);
  assert.equal(c.root([]), f.empty_root);
  assert.equal(
    c.root([...f.snapshot.entries].reverse()),
    f.snapshot.state_root,
  );
  const aggregate = c.trie(
    f.checkpoint.accounts.map((a) => ({
      key: c.accountKey(a.account),
      value: a.id,
    })),
  );
  assert.equal(aggregate, f.checkpoint.root);
  assert.equal(c.proof(aggregate, f.proof), true);
  assert.equal(c.proof(aggregate, f.absent), true);
  assert.equal(c.proof(ZERO, f.proof), false);
  assert.equal(
    c.proof(aggregate, { ...f.proof, siblings: f.proof.siblings.slice(1) }),
    false,
  );
});
test("reject corruption and noncanonical or ambiguous entries", async () => {
  await assert.rejects(
    c.verify({ ...f.snapshot, account: "0x" + "00".repeat(20) }),
  );
  await assert.rejects(c.verify({ ...f.snapshot, state_root: ZERO }));
  await assert.rejects(c.verify({ ...f.snapshot, revision: "01" }));
  assert.throws(() => c.root([...f.snapshot.entries, f.snapshot.entries[0]]));
  assert.throws(() => c.root([{ ...f.snapshot.entries[0], chain_id: "01" }]));
  assert.throws(() => c.root([{ ...f.snapshot.entries[0], weight: 0 }]));
});
test("restored junk remains in the enumerable seed", () => {
  const s = seed(f.snapshot, 8453);
  assert.equal(s.pairs["0x" + "00".repeat(19) + "22"], -1);
  assert.equal(s.assets.length, 1);
  assert.deepEqual(seed(f.snapshot, 10), { pairs: {}, assets: [] });
});
test("never silently select an older or conflicting server snapshot", () => {
  const old = f.snapshot,
    next = { ...old, revision: "2", previous: f.id };
  assert.equal(chooseRevision(old, next, f.id, "next"), next);
  assert.throws(() => chooseRevision(next, old, "next", f.id));
  assert.throws(() => chooseRevision(old, old, f.id, "different"));
  assert.equal(chooseRevision(old, null, f.id, null), old);
});
