// Real Chromium integration with a mocked daemon, wallet and ENS RPC. No funds move.
import { chromium } from "@playwright/test";
import { build } from "esbuild";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import assert from "node:assert/strict";
import { privateKeyToAccount } from "viem/accounts";
import { codec } from "../userstate.mjs";
import * as v from "viem";
const root = fileURLToPath(new URL("../../", import.meta.url));
const fixture = JSON.parse(
  await readFile(root + "internal/userstate/testdata/state.json", "utf8"),
);
const signer = privateKeyToAccount("0x" + "0".repeat(63) + "1"),
  c = codec(v);
let snapshot = fixture.snapshot,
  generation = "1",
  posts = 0;
const server = createServer(async (req, res) => {
  try {
    const path = new URL(req.url, "http://localhost").pathname;
    res.setHeader("Content-Type", "application/json");
    if (path.endsWith("/state") && req.method === "POST") {
      let raw = "";
      for await (const chunk of req) raw += chunk;
      const input = JSON.parse(raw);
      await c.verify(input.snapshot);
      assert.equal(input.generation, generation);
      assert.equal(input.snapshot.previous, await c.verify(snapshot));
      snapshot = input.snapshot;
      generation = String(BigInt(generation) + 1n);
      posts++;
      res.end(
        JSON.stringify({
          id: await c.verify(snapshot),
          snapshot,
          generation,
          legacy_changed: false,
        }),
      );
      return;
    }
    if (path.endsWith("/state")) {
      res.end(
        JSON.stringify({
          state: {
            id: await c.verify(snapshot),
            snapshot,
            generation,
            legacy_changed: false,
          },
          projection: snapshot.entries,
        }),
      );
      return;
    }
    if (path.endsWith("/asset-commit")) {
      res.end(
        JSON.stringify({
          assets: snapshot.entries
            .filter((e) => e.kind === "asset")
            .map((e) => ({ chain_id: Number(e.chain_id), address: e.address })),
        }),
      );
      return;
    }
    if (path.startsWith("/v1/state/revisions/")) {
      res.end(JSON.stringify(snapshot));
      return;
    }
    if (path.startsWith("/v1/")) {
      res.end("{}");
      return;
    }
    if (!["/", "/userstate.mjs"].includes(path)) {
      res.statusCode = 404;
      res.end("{}");
      return;
    }
    res.setHeader(
      "Content-Type",
      path.endsWith(".mjs") ? "text/javascript" : "text/html",
    );
    res.end(
      await readFile(
        root + "web/" + (path === "/" ? "index.html" : "userstate.mjs"),
      ),
    );
  } catch (e) {
    res.statusCode = 500;
    res.end(JSON.stringify({ error: e.message }));
  }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
let browser;
try {
  const bundle = await build({
    stdin: {
      contents: "export * from 'viem'",
      resolveDir: fileURLToPath(new URL(".", import.meta.url)),
    },
    bundle: true,
    write: false,
    format: "esm",
    platform: "browser",
  });
  browser = await chromium.launch({
    headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE }
      : {}),
  });
  const page = await browser.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("https://esm.sh/**", (route) =>
    route.request().url().startsWith("https://esm.sh/viem@2.56.3?")
      ? route.fulfill({
          contentType: "text/javascript",
          body: bundle.outputFiles[0].text,
        })
      : route.abort(),
  );
  await page.exposeFunction("testSignState", (typed) =>
    signer.signTypedData(typed),
  );
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  await page.evaluate(async (f) => {
    WALLET_ADDR = f.snapshot.account;
    WALLET_CHAIN = 8453;
    HOLDINGS = [];
    $("account").value = WALLET_ADDR;
    window.ethereum = {
      request: async ({ method, params }) => {
        if (method === "eth_requestAccounts") return [WALLET_ADDR];
        if (method === "eth_signTypedData_v4")
          return window.testSignState(JSON.parse(params[1]));
        throw Error("Unexpected wallet request " + method);
      },
    };
    await loadPortableState(WALLET_ADDR);
  }, fixture);
  assert.equal(await page.evaluate(() => PORTABLE.revision), "1");
  assert.equal(
    await page.evaluate(
      () =>
        portableMemory(WALLET_ADDR, 8453).pairs["0x" + "00".repeat(19) + "22"],
    ),
    -1,
  );
  assert.equal(await page.evaluate(() => portableAssets(8453).length), 1);
  assert.equal(await page.locator("#portable-state-card").isVisible(), true);
  await page.locator("#portable-state-card summary").click();
  await page.locator("#state-review-button").click();
  await page.waitForFunction(() => PORTABLE_REVIEW !== null);
  assert.equal(await page.locator("#state-review-rows p").count(), 3);
  await page.locator("#state-review-rows select").nth(1).selectOption("1");
  await page.locator("#state-sign-reviewed").click();
  await page.waitForFunction(() => PORTABLE?.revision === "2");
  assert.equal(posts, 1);
  assert.equal(snapshot.revision, "2");
  // Corrupt imports cannot replace the signed cache.
  assert.equal(
    await page.evaluate(async () => {
      try {
        await importPortable({
          ...PORTABLE,
          state_root: "0x" + "00".repeat(32),
        });
        return false;
      } catch {
        return true;
      }
    }),
    true,
  );
  assert.equal(await page.evaluate(() => PORTABLE.revision), "2");
  // ENS calls use the reader's RPC; the transaction contains the signed revision ID.
  await page.evaluate(async () => {
    const c = await stateCode();
    c.v = {
      ...c.v,
      createPublicClient: () => ({
        waitForTransactionReceipt: async () => ({ status: "success" }),
      }),
    };
    window.testENSRecord = "";
    window.testSends = 0;
    window.testChain = "0xaa36a7";
    rpc = async (url, method, params) => {
      if (method === "eth_chainId") return window.testChain;
      if (method === "eth_estimateGas") return "0x186a0";
      if (method === "eth_call") {
        const text = c.v.encodeFunctionResult({
          abi: STATE_RESOLVER_ABI,
          functionName: "text",
          result: window.testENSRecord,
        });
        return params[0].to.toLowerCase() === UNIVERSAL_RESOLVER.toLowerCase()
          ? c.v.encodeFunctionResult({
              abi: UR_ABI,
              functionName: "resolve",
              result: [text, "0x0000000000000000000000000000000000001234"],
            })
          : text;
      }
      throw Error(method);
    };
    window.ethereum = {
      request: async ({ method, params }) => {
        if (method === "eth_requestAccounts") return [WALLET_ADDR];
        if (method === "wallet_switchEthereumChain") return null;
        if (method === "eth_sendTransaction") {
          const decoded = c.v.decodeFunctionData({
            abi: STATE_RESOLVER_ABI,
            data: params[0].data,
          });
          window.testENSRecord = decoded.args[2];
          window.testSends++;
          return "0x" + "11".repeat(32);
        }
        throw Error(method);
      },
    };
    $("state-rpc").value = "https://reader.example";
    $("state-ens-name").value = "reader.eth";
    await publishPortable();
  });
  assert.equal(
    await page.evaluate(() => window.testENSRecord),
    "evmscan:state:1:" + (await c.verify(snapshot)),
  );
  assert.match(
    await page.locator("#state-ens-status").textContent(),
    /verified/,
  );
  await page.evaluate(() => restoreENS());
  assert.equal(await page.evaluate(() => PORTABLE.revision), "2");
  assert.equal(
    await page.evaluate(async () => {
      window.testChain = "0x1";
      try {
        await publishPortable();
        return false;
      } catch {
        return true;
      }
    }),
    true,
  );
  assert.equal(await page.evaluate(() => window.testSends), 1);
  assert.deepEqual(errors, []);
  console.log(
    "Browser: restore, public review/sign, tamper rejection, ENS publish/readback/restore and wrong-network checks passed.",
  );
} finally {
  await browser?.close();
  await new Promise((resolve) => server.close(resolve));
}
