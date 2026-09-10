#!/usr/bin/env python3
"""Read a deployment's constructor arguments off its creation transaction.

The arguments are whatever follows the creation bytecode in the transaction's
input. Taking them from the chain rather than re-encoding the flags someone
believes they passed also checks the artifact: if the creation code in
contracts/out is not a prefix of what was deployed, this build is not the build
that produced that contract, and verification would fail for that reason rather
than because of the arguments.

    ctor-args.py <rpc-url> <tx-hash> <artifact.json>
"""

import json
import sys
import time
import urllib.request


def rpc(url, method, params, tries=5):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
    last = None
    for attempt in range(tries):
        try:
            req = urllib.request.Request(
                url, body.encode(), {"content-type": "application/json"}
            )
            return json.load(urllib.request.urlopen(req, timeout=60))
        except Exception as exc:  # a rate limit or a blip, not an answer
            last = exc
            if attempt < tries - 1:
                # Retrying a rate limit immediately just spends the next token on
                # another refusal; back off instead.
                time.sleep(2 ** attempt)
    raise SystemExit(f"rpc {method} failed after {tries} attempts: {last}")


def creation_code(artifact):
    with open(artifact) as fh:
        art = json.load(fh)
    code = art.get("bytecode") or art.get("creation")
    if isinstance(code, dict):
        code = code.get("object")
    if not code:
        raise SystemExit(f"no creation bytecode in {artifact}")
    return code if code.startswith("0x") else "0x" + code


def main():
    if len(sys.argv) != 4:
        raise SystemExit(__doc__)
    url, tx, artifact = sys.argv[1:4]

    result = rpc(url, "eth_getTransactionByHash", [tx]).get("result")
    if not result:
        raise SystemExit(f"transaction {tx} not found on this endpoint")
    data = result["input"]

    code = creation_code(artifact)
    if not data.startswith(code):
        raise SystemExit(
            "the creation code in contracts/out is not a prefix of the deployed "
            "input, so this build is not what produced that contract. Check out "
            "the commit it was deployed from and run: make contracts"
        )
    print(data[len(code):])


if __name__ == "__main__":
    main()
