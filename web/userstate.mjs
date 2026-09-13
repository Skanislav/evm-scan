// Portable state v1. Cryptographic primitives are injected from the page's viem.
export const ZERO = "0x" + "00".repeat(32);
const hashRE = /^0x[0-9a-fA-F]{64}$/;
const addressRE = /^0x[0-9a-fA-F]{40}$/;
export function decimal(s, max) {
  if (typeof s !== "string" || !/^(0|[1-9][0-9]*)$/.test(s) || BigInt(s) > max)
    throw Error("Invalid decimal string");
  return BigInt(s);
}
export function codec(v) {
  const hash = (...parts) => v.keccak256(v.concat(parts));
  const empty = Array(257);
  empty[256] = hash("0x02");
  const branch = (l, r) => hash("0x01", l, r);
  for (let d = 255; d >= 0; d--) empty[d] = branch(empty[d + 1], empty[d + 1]);
  const bit = (key, d) =>
    (parseInt(
      key.slice(2 + 2 * Math.floor(d / 8), 4 + 2 * Math.floor(d / 8)),
      16,
    ) >>
      (7 - (d % 8))) &
    1;
  function trie(pairs) {
    const p = pairs.slice().sort((a, b) => a.key.localeCompare(b.key));
    for (let i = 0; i < p.length; i++) {
      if (!hashRE.test(p[i].key) || !/^0x(?:[a-f0-9]{2})+$/i.test(p[i].value))
        throw Error("Invalid trie pair");
      if (i && p[i].key.toLowerCase() === p[i - 1].key.toLowerCase())
        throw Error("Duplicate state key");
    }
    const walk = (lo, hi, d) => {
      if (lo === hi) return empty[d];
      if (d === 256) return hash("0x00", p[lo].key, p[lo].value);
      let mid = lo;
      while (mid < hi && !bit(p[mid].key, d)) mid++;
      return branch(walk(lo, mid, d + 1), walk(mid, hi, d + 1));
    };
    return walk(0, p.length, 0);
  }
  function pairs(entries) {
    entries ??= [];
    if (!Array.isArray(entries) || entries.length > 2000)
      throw Error("At most 2,000 state entries");
    const counts = new Map();
    let assets = 0;
    return entries.map((e) => {
      const c = decimal(e.chain_id, (1n << 63n) - 1n);
      if (!c || !addressRE.test(e.address))
        throw Error("Invalid chain or address");
      let ns, value;
      if (e.kind === "verdict") {
        ns = "0x01";
        if (e.weight !== 1 && e.weight !== -1) throw Error("Invalid verdict");
        value = e.weight === 1 ? "0x01" : "0xff";
        const n = (counts.get(e.chain_id) || 0) + 1;
        counts.set(e.chain_id, n);
        if (n > 200) throw Error("At most 200 verdicts per chain");
      } else if (e.kind === "asset") {
        ns = "0x02";
        value = "0x01";
        if (e.weight !== undefined && e.weight !== 0)
          throw Error("Asset cannot have a weight");
        if (++assets > 200) throw Error("At most 200 remembered assets");
      } else throw Error("Unknown state entry kind");
      return {
        key: hash(
          ns,
          "0x" + c.toString(16).padStart(16, "0"),
          e.address.toLowerCase(),
        ),
        value,
      };
    });
  }
  const root = (entries) => trie(pairs(entries));
  function typed(s) {
    return {
      domain: { name: "evm-scan state", version: "1" },
      primaryType: "State",
      types: {
        State: [
          { name: "account", type: "address" },
          { name: "stateRoot", type: "bytes32" },
          { name: "revision", type: "uint64" },
          { name: "previous", type: "bytes32" },
          { name: "deadline", type: "uint256" },
        ],
      },
      message: {
        account: s.account,
        stateRoot: s.state_root,
        revision: BigInt(s.revision),
        previous: s.previous,
        deadline: BigInt(s.deadline),
      },
    };
  }
  async function verify(s) {
    if (
      s.version !== 1 ||
      !addressRE.test(s.account) ||
      !hashRE.test(s.state_root) ||
      !hashRE.test(s.previous)
    )
      throw Error("Invalid state envelope");
    const r = decimal(s.revision, (1n << 64n) - 1n);
    decimal(s.deadline, (1n << 256n) - 1n);
    if (!r || (r === 1n) !== (s.previous === ZERO))
      throw Error("Invalid revision linkage");
    if (!/^0x[0-9a-fA-F]{130}$/.test(s.signature))
      throw Error("Invalid state signature");
    const ss = BigInt("0x" + s.signature.slice(66, 130));
    if (
      ss === 0n ||
      ss > 0x7fffffffffffffffffffffffffffffff5d576e7357a4501ddfe92f46681b20a0n
    )
      throw Error("Noncanonical signature");
    if (root(s.entries) !== s.state_root.toLowerCase())
      throw Error("State root mismatch");
    const t = typed(s);
    const signer = await v.recoverTypedDataAddress({
      ...t,
      signature: s.signature,
    });
    if (signer.toLowerCase() !== s.account.toLowerCase())
      throw Error("State signer mismatch");
    return v.hashTypedData(t);
  }
  const accountKey = (a) => {
    if (!addressRE.test(a)) throw Error("Invalid account");
    return hash(v.stringToHex("evmscan/accounts/v1"), a.toLowerCase());
  };
  function proof(root, p) {
    if (
      !hashRE.test(p.key) ||
      !Array.isArray(p.siblings) ||
      p.siblings.length !== 256
    )
      return false;
    let h = empty[256];
    if (p.value) {
      const b = Uint8Array.from(atob(p.value), (x) => x.charCodeAt(0));
      if (b.length) h = hash("0x00", p.key, v.toHex(b));
    }
    for (let d = 255; d >= 0; d--) {
      if (!hashRE.test(p.siblings[d])) return false;
      h = bit(p.key, d) ? branch(p.siblings[d], h) : branch(h, p.siblings[d]);
    }
    return h === root.toLowerCase();
  }
  return { root, typed, verify, trie, accountKey, proof, empty };
}
export function chooseRevision(local, remote, localID, remoteID) {
  if (!local) return remote;
  if (!remote) return local;
  if (localID === remoteID) return local;
  if (BigInt(remote.revision) < BigInt(local.revision))
    throw Error("Server offered an older revision; local copy retained");
  if (
    BigInt(remote.revision) === BigInt(local.revision) ||
    remote.previous !== localID
  )
    throw Error(
      "Different signed history; import the chosen snapshot explicitly",
    );
  return remote;
}
export function seed(snapshot, chain) {
  const pairs = {},
    assets = [];
  for (const e of snapshot?.entries || []) {
    if (e.chain_id !== String(chain)) continue;
    if (e.kind === "verdict") pairs[e.address.toLowerCase()] = e.weight;
    else assets.push(e.address);
  }
  return { pairs, assets };
}
