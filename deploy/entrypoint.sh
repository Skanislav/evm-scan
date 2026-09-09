#!/usr/bin/env bash
# Runs Helios and evmscand as one unit.
#
# Helios listens on loopback only, so from the daemon's point of view this is a
# local node: require_local_node stays on and stays true. If either process dies the
# container exits, and the supervisor restarts both; there is no useful state in
# which one runs without the other.
set -euo pipefail

: "${HELIOS_NETWORK:=sepolia}"
: "${HELIOS_EXECUTION_RPC:?set HELIOS_EXECUTION_RPC to an upstream RPC that supports eth_getProof}"
: "${HELIOS_CONSENSUS_RPC:?set HELIOS_CONSENSUS_RPC to a beacon API serving light-client endpoints}"
: "${HELIOS_RPC_PORT:=8545}"
: "${HELIOS_DATA_DIR:=/data/helios}"
: "${EVMSCAN_CONFIG:=/app/config.yaml}"
: "${EVMSCAN_WEB_DIR:=/app/web}"
: "${EVMSCAN_LOG_LEVEL:=info}"

# Railway hands the port in $PORT; the daemon wants a literal listen address.
export EVMSCAN_API_LISTEN="${EVMSCAN_API_LISTEN:-0.0.0.0:${PORT:-8080}}"

helios_args=(
  ethereum
  --network "$HELIOS_NETWORK"
  --execution-rpc "$HELIOS_EXECUTION_RPC"
  --consensus-rpc "$HELIOS_CONSENSUS_RPC"
  --rpc-bind-ip 127.0.0.1
  --rpc-port "$HELIOS_RPC_PORT"
  --data-dir "$HELIOS_DATA_DIR"
  --load-external-fallback
)
if [[ -n "${HELIOS_CHECKPOINT:-}" ]]; then
  helios_args+=(--checkpoint "$HELIOS_CHECKPOINT")
fi

echo "entrypoint: starting helios (${HELIOS_NETWORK})"
/app/helios "${helios_args[@]}" &
helios_pid=$!

rpc() {
  curl -fsS -m 5 -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":[]}" \
    "http://127.0.0.1:${HELIOS_RPC_PORT}" 2>/dev/null
}

# Wait until Helios has a verified head. eth_chainId answers before sync completes,
# so ask for the block number and insist on a non-zero one.
head=""
for _ in $(seq 1 120); do
  if ! kill -0 "$helios_pid" 2>/dev/null; then
    echo "entrypoint: helios exited before becoming ready" >&2
    exit 1
  fi
  head="$(rpc eth_blockNumber | sed -n 's/.*"result":"\(0x[0-9a-fA-F]*\)".*/\1/p' || true)"
  if [[ -n "$head" && "$head" != "0x0" ]]; then
    echo "entrypoint: helios ready at block $head"
    break
  fi
  sleep 2
done
if [[ -z "$head" || "$head" == "0x0" ]]; then
  echo "entrypoint: helios did not become ready in time" >&2
  kill "$helios_pid" 2>/dev/null || true
  exit 1
fi

echo "entrypoint: starting evmscand on ${EVMSCAN_API_LISTEN}"
/app/evmscand -config "$EVMSCAN_CONFIG" -web "$EVMSCAN_WEB_DIR" -log-level "$EVMSCAN_LOG_LEVEL" &
scan_pid=$!

shutdown() {
  echo "entrypoint: shutting down"
  kill -TERM "$scan_pid" 2>/dev/null || true
  kill -TERM "$helios_pid" 2>/dev/null || true
}
trap shutdown TERM INT

# Whichever exits first decides the container's fate.
set +e
wait -n "$helios_pid" "$scan_pid"
code=$?
shutdown
wait "$scan_pid" 2>/dev/null
wait "$helios_pid" 2>/dev/null
exit "$code"
