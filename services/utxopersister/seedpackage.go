package utxopersister

import (
	"context"
	"crypto/sha256"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/pkg/seedpack"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
)

// BuildSeedPackage streams the UTXO-set body from r, splits it into
// content-defined chunks, writes each not-yet-present chunk to the store
// (content-addressed, deduplicated), and writes a manifest keyed by blockHash.
func BuildSeedPackage(ctx context.Context, store blob.Store, r io.Reader, height uint32, blockHash chainhash.Hash, setHash [32]byte, cfg seedpack.Config) error {
	manifest := seedpack.Manifest{
		FormatVersion: seedpack.FormatVersion,
		Height:        height,
		BlockHash:     blockHash,
		SetHash:       setHash,
	}

	err := seedpack.SplitStream(r, cfg, func(chunk []byte) error {
		hash := sha256.Sum256(chunk)

		exists, err := store.Exists(ctx, hash[:], fileformat.FileTypeSeedChunk)
		if err != nil {
			return errors.NewStorageError("error checking chunk %x", hash, err)
		}

		if !exists {
			if err := store.Set(ctx, hash[:], fileformat.FileTypeSeedChunk, chunk); err != nil {
				return errors.NewStorageError("error writing chunk %x", hash, err)
			}
		}

		manifest.Chunks = append(manifest.Chunks, seedpack.ChunkRef{Hash: hash, Size: uint32(len(chunk))})

		return nil
	})
	if err != nil {
		return err
	}

	if err := store.Set(ctx, blockHash[:], fileformat.FileTypeSeedManifest, manifest.Serialize(), options.WithAllowOverwrite(true)); err != nil {
		return errors.NewStorageError("error writing seed manifest for %s", blockHash.String(), err)
	}

	return nil
}

// ReadSeedPackage reassembles the UTXO-set body from a seed package, verifying
// each chunk's content hash and length against the manifest.
func ReadSeedPackage(ctx context.Context, store blob.Store, blockHash chainhash.Hash) ([]byte, error) {
	m, err := readManifest(ctx, store, blockHash)
	if err != nil {
		return nil, err
	}

	var out []byte

	for i, ref := range m.Chunks {
		chunk, err := getChunk(ctx, store, ref.Hash)
		if err != nil {
			return nil, err
		}

		if uint32(len(chunk)) != ref.Size {
			return nil, errors.NewProcessingError("chunk %d size %d, manifest says %d", i, len(chunk), ref.Size)
		}

		if got := sha256.Sum256(chunk); got != ref.Hash {
			return nil, errors.NewProcessingError("chunk %d content hash mismatch", i)
		}

		out = append(out, chunk...)
	}

	return out, nil
}

func readManifest(ctx context.Context, store blob.Store, blockHash chainhash.Hash) (seedpack.Manifest, error) {
	b, err := store.Get(ctx, blockHash[:], fileformat.FileTypeSeedManifest)
	if err != nil {
		return seedpack.Manifest{}, err
	}

	return seedpack.ParseManifest(b)
}

func getChunk(ctx context.Context, store blob.Store, hash [32]byte) ([]byte, error) {
	return store.Get(ctx, hash[:], fileformat.FileTypeSeedChunk)
}

func overwriteChunk(ctx context.Context, store blob.Store, hash [32]byte, data []byte) error {
	return store.Set(ctx, hash[:], fileformat.FileTypeSeedChunk, data, options.WithAllowOverwrite(true))
}
