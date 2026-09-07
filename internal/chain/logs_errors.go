package chain

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/rpc"
)

// LogsErrorKind says what an eth_getLogs failure means for a history probe.
//
// The distinction matters because the probe uses errors as evidence: a block that
// fails is taken to be pruned. Only a definite "history unavailable" answer is such
// evidence; a timeout says nothing about the block and must be retried, not recorded.
type LogsErrorKind int

const (
	// LogsErrUnknown is an error the classifier does not recognise. The probe treats
	// it as unavailable — that is the conservative reading for a backfill floor — but
	// reports it separately so an operator can tell a guess from a known answer.
	LogsErrUnknown LogsErrorKind = iota
	// LogsErrHistoryUnavailable is a definite statement from the node that it no
	// longer holds the receipts (or headers) for the requested block.
	LogsErrHistoryUnavailable
	// LogsErrTransient is a failure of the transport or of the moment — timeouts,
	// resets, rate limits, a busy server — that says nothing about the block.
	LogsErrTransient
)

func (k LogsErrorKind) String() string {
	switch k {
	case LogsErrHistoryUnavailable:
		return "history_unavailable"
	case LogsErrTransient:
		return "transient"
	default:
		return "unknown"
	}
}

// gethPrunedHistoryCode is the JSON-RPC error code go-ethereum attaches to
// *history.PrunedHistoryError ("pruned history unavailable"), which is what
// eth_getLogs returns for any range below the node's history-pruning cutoff
// (go-ethereum v1.16: core/history/historymode.go, eth/filters/filter_system.go,
// eth/api_backend.go). Matching the code rather than the text survives rewording.
const gethPrunedHistoryCode = 4444

// Message fragments, lower-cased, that identify a definite history-unavailable
// answer. Each names the thing that is missing: a bare "not found" is not on the
// list, because a proxy's "404 Not Found" and a JSON-RPC "method not found" both
// contain it and neither is evidence that a block was pruned. Only the go-ethereum ones have been observed against a live node; the
// rest are the wordings other clients are documented or reported to use, kept
// here so a non-geth node degrades to "unknown" less often. See README, "Why a
// snap-synced node is enough".
var historyUnavailableNeedles = []string{
	// go-ethereum
	"pruned history unavailable",              // core/history.PrunedHistoryError, code 4444
	"failed to get logs for block",            // eth/filters/filter_system.go: receipts missing
	"header not found",                        // eth/filters/filter.go: unindexed search
	"unknown block",                           // eth/filters/filter.go: single-block filter
	"block body not found",                    // eth/api_backend.go
	"header found, but block body is missing", // eth/api_backend.go
	"receipt not found",                       // older geth receipt accessors
	"receipts not found",
	"missing trie node", // state below the pruning point; not logs, but definite
	// Other clients (untested against a live node; see README).
	"history not available",
	"history unavailable",
	"historical data",
	"pruned",
	"block not found",
	"beyond the last available block",
	"distance to target block exceeds maximum",
}

// Message fragments, lower-cased, that identify a transient failure.
var transientNeedles = []string{
	"timeout", "timed out", "deadline exceeded",
	"connection reset", "connection refused", "broken pipe", "connection closed",
	"use of closed network connection", "unexpected eof", "eof",
	"too many requests", "rate limit", "rate-limit", "request limit", "429",
	"busy", "overloaded", "temporarily unavailable", "service unavailable",
	"bad gateway", "gateway timeout", "try again", "backoff",
	"client is closed", // rpc.ErrClientQuit: the dial died; retrying reports it honestly
	"no result",        // rpc.ErrNoResult: a malformed reply, not an answer about the block
	"bad result in json-rpc response",
	"context canceled",
}

// ClassifyLogsError decides what an eth_getLogs error means for the history probe.
//
// It checks structured signals first — the context, net.Error, syscall errnos,
// rpc.HTTPError status codes and the JSON-RPC error code — and falls back to the
// message text only because clients do not agree on codes. Definite history
// answers win over transient wording so a message like "pruned history
// unavailable (request timed out fetching)" is still read as pruned.
func ClassifyLogsError(err error) LogsErrorKind {
	if err == nil {
		return LogsErrUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return LogsErrTransient
	}

	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		switch code := rpcErr.ErrorCode(); code {
		case gethPrunedHistoryCode:
			return LogsErrHistoryUnavailable
		case -32002, // go-ethereum server-side "request timed out"
			-32005: // "limit exceeded" / rate limit at several providers
			return LogsErrTransient
		case -32601, // "method not found": the node does not serve eth_getLogs at all
			-32600: // malformed request; our bug, never the block's age
			return LogsErrUnknown
		}
	}

	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
			http.StatusRequestTimeout:
			return LogsErrTransient
		case http.StatusNotFound:
			// A proxy or load balancer answering the RPC path with 404. The body is
			// "404 Not Found", which the message needles must not read as an answer
			// about the block.
			return LogsErrTransient
		}
	}

	msg := strings.ToLower(err.Error())
	for _, needle := range historyUnavailableNeedles {
		if strings.Contains(msg, needle) {
			return LogsErrHistoryUnavailable
		}
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, rpc.ErrClientQuit) || errors.Is(err, rpc.ErrNoResult) ||
		errors.Is(err, rpc.ErrBadResult) {
		return LogsErrTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		// Every net.Error we can get from a JSON-RPC round trip — dial, read, write —
		// is about the connection, not the block.
		return LogsErrTransient
	}
	for _, needle := range transientNeedles {
		if strings.Contains(msg, needle) {
			return LogsErrTransient
		}
	}
	return LogsErrUnknown
}
