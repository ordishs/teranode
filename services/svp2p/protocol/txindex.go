package protocol

import (
	"context"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
)

// ErrTxUnknown is returned by TxIndex.Open when the requested transaction is
// not held.
var ErrTxUnknown = errors.New(errors.ERR_NOT_FOUND, "svp2p: transaction not held")

// TxIndex is the seam compact-block reconstruction (spec §6) reads through:
// protocol owns the state machine and the wire exchange, and TxIndex is what
// only Teranode knows — which transactions bridge holds, and their bytes.
type TxIndex interface {
	// Match returns, for each short ID, the transaction hash it identifies in
	// the index under (k0,k1), or nil; and reports a collision (two indexed
	// hashes map to one short ID), which BIP152 says must abort
	// reconstruction.
	Match(k0, k1 uint64, shortIDs []uint64) (hashes []*chainhash.Hash, collision bool)
	// Open returns the raw bytes of a transaction the node holds as a
	// reader, or ErrTxUnknown. The answer comes from the node's own
	// storage, not from whatever Match matched against: an implementation
	// may hold bytes for a transaction its index no longer names.
	Open(ctx context.Context, hash chainhash.Hash) (io.ReadCloser, uint64, error)
}
