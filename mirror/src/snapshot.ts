/**
 * Reading and checking an evmscan snapshot document.
 *
 * The format is the one `internal/snapshot` writes: NDJSON, one header line, then
 * the coverage rows, then the leaves, in tree order. This is the port of
 * `snapshot.Verify` — it recomputes every leaf from the account and its assets
 * rather than reading any hash out of the document, so a document that disagrees
 * with itself cannot pass. A snapshot is therefore only as good as the roots the
 * caller checks it against, which come from the chain.
 */

import {
  assetKey,
  assetsHash,
  buildRoot,
  coverageLeaf,
  leafHash,
  normalizeHex,
  ZERO_HASH,
  type Hex,
} from "./merkle.js";

export const FORMAT = "evmscan-snapshot/1";

export interface SnapshotHeader {
  format: string;
  /**
   * The chain the index is *about*. `registryChainId` is the chain the registry
   * sits on, which is not necessarily the same one — confusing the two is the
   * standing bug in this codebase, so a snapshot states both and so do we.
   */
  chainId: bigint;
  fromBlock: bigint;
  toBlock: bigint;
  root: Hex;
  coverageRoot: Hex;
  leafCount: number;
  assetCount: number;
  onchainEpochId?: bigint;
  registry?: Hex;
  registryChainId?: bigint;
}

export interface Coverage {
  asset: Hex;
  fromBlock: bigint;
  toBlock: bigint;
}

export interface Leaf {
  account: Hex;
  assets: Hex[];
}

export interface Snapshot {
  header: SnapshotHeader;
  coverage: Coverage[];
  leaves: Leaf[];
  /** Rebuilt from the rows, never taken from the header. */
  root: Hex;
  coverageRoot: Hex;
}

/**
 * `JSON.parse`, with integer literals preserved as strings.
 *
 * This exists because `JSON.parse` is lossy for the values this format carries:
 * `chain_id`, `from_block` and `to_block` are uint64 on the wire, and anything above
 * 2^53 comes back from `JSON.parse` already rounded — by the time a guard could
 * look at it, the true value is gone. A rounded chain id hashes to a wrong leaf, so
 * the loss would show up as a root mismatch blamed on the publisher.
 *
 * So the text is rewritten before it is parsed: every bare integer token becomes a
 * quoted string, which `toBigInt` then reads exactly. The scan tracks string state
 * so that digits inside a JSON string — a hex address, a uri with a port — are left
 * alone.
 */
function parseJsonLossless(text: string): unknown {
  let out = "";
  let inString = false;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]!;
    if (inString) {
      out += ch;
      if (ch === "\\") {
        out += text[++i] ?? "";
      } else if (ch === '"') {
        inString = false;
      }
      continue;
    }
    if (ch === '"') {
      inString = true;
      out += ch;
      continue;
    }
    // A number token starts here only if we are between structural characters.
    if (ch === "-" || (ch >= "0" && ch <= "9")) {
      let j = i;
      if (text[j] === "-") j++;
      const digits = (): void => {
        while (j < text.length && text[j]! >= "0" && text[j]! <= "9") j++;
      };
      digits();
      const intEnd = j;
      // A fraction or an exponent has to be consumed with the token, not left for
      // the next iteration: emitting only the integer part and then re-quoting the
      // remaining digits produces invalid JSON, and the failure would read as a
      // corrupt document rather than an unexpected field.
      if (text[j] === ".") {
        j++;
        digits();
      }
      if (text[j] === "e" || text[j] === "E") {
        j++;
        if (text[j] === "+" || text[j] === "-") j++;
        digits();
      }
      if (j === intEnd) {
        out += `"${text.slice(i, j)}"`;
      } else {
        // A non-integer is left as-is; no field in this format is one, and
        // JSON.parse hands it back as a number that toBigInt refuses.
        out += text.slice(i, j);
      }
      i = j - 1;
      continue;
    }
    out += ch;
  }
  return JSON.parse(out);
}

function requireField(row: Record<string, unknown>, name: string, at: string): unknown {
  const v = row[name];
  if (v === undefined || v === null) throw new Error(`${at} has no ${name}`);
  return v;
}

function toBigInt(v: unknown, at: string): bigint {
  if (typeof v === "bigint") return v;
  if (typeof v === "number") {
    if (!Number.isSafeInteger(v)) throw new Error(`${at}: ${v} is not a safe integer`);
    return BigInt(v);
  }
  if (typeof v === "string" && v !== "") return BigInt(v);
  if (v === undefined || v === null) return 0n;
  throw new Error(`${at}: cannot read ${String(v)} as an integer`);
}

/**
 * Parses the header, which JSON.parse would otherwise hand back with `chain_id` as
 * a `number`. Chain ids and block numbers are uint64 on the wire; above 2^53 a
 * `number` silently rounds and the resulting leaf hash is wrong rather than absent.
 * So every integer field is read through `toBigInt`, and a value that arrived
 * already-rounded is rejected instead of hashed.
 */
export function parseHeader(line: string): SnapshotHeader {
  const raw = parseJsonLossless(line) as Record<string, unknown>;
  const format = raw["format"];
  if (format !== FORMAT) {
    throw new Error(`unknown snapshot format ${JSON.stringify(format)}, want ${FORMAT}`);
  }
  const header: SnapshotHeader = {
    format: FORMAT,
    chainId: toBigInt(requireField(raw, "chain_id", "header"), "header.chain_id"),
    fromBlock: toBigInt(raw["from_block"], "header.from_block"),
    toBlock: toBigInt(raw["to_block"], "header.to_block"),
    root: normalizeHex(String(raw["root"] ?? ZERO_HASH), 32),
    coverageRoot: normalizeHex(String(raw["coverage_root"] ?? ZERO_HASH), 32),
    leafCount: Number(toBigInt(raw["leaf_count"], "header.leaf_count")),
    assetCount: Number(toBigInt(raw["asset_count"], "header.asset_count")),
  };
  if (raw["onchain_epoch_id"] !== undefined) {
    header.onchainEpochId = toBigInt(raw["onchain_epoch_id"], "header.onchain_epoch_id");
  }
  if (raw["registry"] !== undefined) {
    header.registry = normalizeHex(String(raw["registry"]), 20);
  }
  if (raw["registry_chain_id"] !== undefined) {
    header.registryChainId = toBigInt(raw["registry_chain_id"], "header.registry_chain_id");
  }
  return header;
}

/**
 * Reads a whole snapshot and rebuilds both trees from it.
 *
 * Row ordering is not cosmetic: a merkle tree is built from a sequence, so coverage
 * rows interleaved with leaves would land in the wrong tree. The Go writer emits
 * them in sections and this refuses a document that does not.
 */
export function parseSnapshot(text: string): Snapshot {
  const lines = text.split("\n").filter((l) => l.trim() !== "");
  const first = lines[0];
  if (first === undefined) throw new Error("empty snapshot: no header");
  const header = parseHeader(first);

  const coverage: Coverage[] = [];
  const leaves: Leaf[] = [];
  const coverageLeaves: Hex[] = [];
  const indexLeaves: Hex[] = [];
  let seenLeaf = false;

  for (let i = 1; i < lines.length; i++) {
    const row = parseJsonLossless(lines[i]!) as Record<string, unknown>;
    const at = `row ${i}`;
    switch (row["t"]) {
      case "coverage": {
        if (seenLeaf) throw new Error(`${at}: coverage row after a leaf row; sections are ordered`);
        const asset = normalizeHex(String(requireField(row, "asset", at)), 20);
        const c: Coverage = {
          asset,
          fromBlock: toBigInt(row["from_block"], `${at}.from_block`),
          toBlock: toBigInt(row["to_block"], `${at}.to_block`),
        };
        coverage.push(c);
        coverageLeaves.push(
          coverageLeaf(assetKey(header.chainId, c.asset), c.fromBlock, c.toBlock),
        );
        break;
      }
      case "leaf": {
        seenLeaf = true;
        const account = normalizeHex(String(requireField(row, "account", at)), 20);
        const assets = ((row["assets"] as string[] | undefined) ?? []).map((a) =>
          normalizeHex(a, 20),
        );
        leaves.push({ account, assets });
        indexLeaves.push(leafHash(account, header.chainId, assetsHash(assets)));
        break;
      }
      default:
        throw new Error(`${at}: unknown row type ${JSON.stringify(row["t"])}`);
    }
  }

  if (header.leafCount !== 0 && header.leafCount !== leaves.length) {
    throw new Error(`header says ${header.leafCount} leaves, document has ${leaves.length}`);
  }
  if (header.assetCount !== 0 && header.assetCount !== coverage.length) {
    throw new Error(`header says ${header.assetCount} assets, document has ${coverage.length}`);
  }

  return {
    header,
    coverage,
    leaves,
    root: buildRoot(indexLeaves),
    coverageRoot: buildRoot(coverageLeaves),
  };
}

/**
 * Rebuilds both roots from rows already in memory — the shape a mirror is in, since
 * its rows come out of SQLite rather than off the wire.
 *
 * `leaves` must be in the order the publisher built the tree from, which is
 * ascending by account: `store.SnapshotIndex` selects `ORDER BY account` over a
 * `BYTEA` column, and Postgres orders those bytewise. `sortLeaves` is that order.
 */
export function rootsOf(
  chainId: bigint,
  coverage: readonly Coverage[],
  leaves: readonly Leaf[],
): { root: Hex; coverageRoot: Hex } {
  return {
    root: buildRoot(leaves.map((l) => leafHash(l.account, chainId, assetsHash(l.assets)))),
    coverageRoot: buildRoot(
      coverage.map((c) => coverageLeaf(assetKey(chainId, c.asset), c.fromBlock, c.toBlock)),
    ),
  };
}

/**
 * Puts leaves in the order the publisher's tree was built from: ascending by
 * account, compared as bytes.
 *
 * A mirror must reproduce this exactly. Sorted-pair hashing makes a proof carry no
 * direction bits, but it does not make the tree order-independent — leaves in a
 * different order give a different root, with nothing to say which one was meant.
 */
export function sortLeaves(leaves: readonly Leaf[]): Leaf[] {
  return [...leaves].sort((a, b) => {
    const x = normalizeHex(a.account, 20);
    const y = normalizeHex(b.account, 20);
    return x < y ? -1 : x > y ? 1 : 0;
  });
}
