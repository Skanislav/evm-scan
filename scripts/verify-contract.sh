#!/usr/bin/env bash
# Verify a deployed contract on an Etherscan-family explorer.
#
# Verification is a recompile: the explorer rebuilds from the sources and settings
# it is given and compares the result with the deployed bytecode. It therefore has
# to be the same input this repo compiled, which is why `make contracts` writes
# contracts/out/standard-input.json instead of leaving anyone to reconstruct the
# solc version, optimizer runs and evmVersion by hand. Any of those differing gives
# different bytecode and a failed verification that looks like a mystery.
#
# Constructor arguments are per-deployment rather than per-build, so they are read
# off the creation transaction: whatever follows the creation bytecode in its input
# is by definition the arguments that deployment used. That is more trustworthy than
# re-encoding the flags someone believes they passed.
#
#   ETHERSCAN_API_KEY=... scripts/verify-contract.sh \
#     -chain 8453 -address 0x… -tx 0x… -rpc https://… [-name HintRegistry]
set -euo pipefail

CHAIN="" ADDRESS="" TX="" RPC="" NAME="HintRegistry"
while [[ $# -gt 0 ]]; do
  case "$1" in
    -chain)   CHAIN="$2";   shift 2 ;;
    -address) ADDRESS="$2"; shift 2 ;;
    -tx)      TX="$2";      shift 2 ;;
    -rpc)     RPC="$2";     shift 2 ;;
    -name)    NAME="$2";    shift 2 ;;
    *) echo "unknown flag $1" >&2; exit 2 ;;
  esac
done

: "${ETHERSCAN_API_KEY:?set ETHERSCAN_API_KEY (one key covers every chain on the v2 API)}"
[[ -n "$CHAIN"   ]] || { echo "-chain is required (1 mainnet, 8453 base, 11155111 sepolia)" >&2; exit 2; }
[[ -n "$ADDRESS" ]] || { echo "-address is required" >&2; exit 2; }
[[ -n "$TX"      ]] || { echo "-tx is required: the creation transaction" >&2; exit 2; }
[[ -n "$RPC"     ]] || { echo "-rpc is required, to read that transaction" >&2; exit 2; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INPUT="$ROOT/contracts/out/standard-input.json"
ART="$ROOT/contracts/out/$NAME.json"
for f in "$INPUT" "$ART"; do
  [[ -f "$f" ]] || { echo "$f missing; run: make contracts" >&2; exit 1; }
done

ARGS="$(python3 "$ROOT/scripts/ctor-args.py" "$RPC" "$TX" "$ART")"
SOLC="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dependencies"]["solc"].lstrip("^~="))' "$ROOT/scripts/package.json")"

echo "verifying $NAME at $ADDRESS on chain $CHAIN"
echo "  compiler   v$SOLC"
echo "  ctor args  ${#ARGS} hex chars"

RESP="$(curl -s -X POST "https://api.etherscan.io/v2/api?chainid=$CHAIN" \
  --data-urlencode "apikey=$ETHERSCAN_API_KEY" \
  --data-urlencode "module=contract" \
  --data-urlencode "action=verifysourcecode" \
  --data-urlencode "codeformat=solidity-standard-json-input" \
  --data-urlencode "contractaddress=$ADDRESS" \
  --data-urlencode "contractname=$NAME.sol:$NAME" \
  --data-urlencode "compilerversion=v$SOLC" \
  --data-urlencode "constructorArguements=$ARGS" \
  --data-urlencode "sourceCode@$INPUT")"

echo "$RESP"
GUID="$(printf '%s' "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("result",""))' 2>/dev/null || true)"
if [[ -n "$GUID" && "$GUID" != "null" ]]; then
  echo
  echo "check status with:"
  echo "  curl -s \"https://api.etherscan.io/v2/api?chainid=$CHAIN&module=contract&action=checkverifystatus&guid=$GUID&apikey=\$ETHERSCAN_API_KEY\""
fi
