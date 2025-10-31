package unlocktx

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/aerospike/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/settings"
	aerospikeStore "github.com/bsv-blockchain/teranode/stores/utxo/aerospike"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
)

// UnlockTransaction unlocks all records for a given transaction hash
func UnlockTransaction(logger ulogger.Logger, tSettings *settings.Settings, txHashStr string) {
	ctx := context.Background()

	// Validate transaction hash format
	if len(txHashStr) != 64 {
		logger.Errorf("Invalid transaction hash format: %s (expected 64 hex characters)", txHashStr)
		fmt.Printf("Error: Invalid transaction hash format. Expected 64 hex characters, got %d\n", len(txHashStr))
		return
	}

	// Decode transaction hash
	txHashBytes, err := hex.DecodeString(txHashStr)
	if err != nil {
		logger.Errorf("Failed to decode transaction hash: %v", err)
		fmt.Printf("Error: Failed to decode transaction hash: %v\n", err)
		return
	}

	// Create chainhash.Hash from bytes
	txHash, err := chainhash.NewHash(txHashBytes)
	if err != nil {
		logger.Errorf("Failed to create transaction hash: %v", err)
		fmt.Printf("Error: Failed to create transaction hash: %v\n", err)
		return
	}

	// Connect to Aerospike UTXO store
	logger.Infof("Connecting to UTXO store...")
	aeroStore, err := aerospikeStore.New(ctx, logger, tSettings, tSettings.UtxoStore.UtxoStore)
	if err != nil {
		logger.Errorf("Failed to connect to UTXO store: %v", err)
		fmt.Printf("Error: Failed to connect to UTXO store: %v\n", err)
		return
	}

	// Get the main record to check locked status and determine number of records
	logger.Infof("Fetching transaction record for %s...", txHashStr)
	mainKey, err := aerospike.NewKey(aeroStore.GetNamespace(), aeroStore.GetName(), txHash[:])
	if err != nil {
		logger.Errorf("Failed to create Aerospike key: %v", err)
		fmt.Printf("Error: Failed to create Aerospike key: %v\n", err)
		return
	}

	policy := util.GetAerospikeReadPolicy(tSettings)
	record, err := aeroStore.GetClient().Get(policy, mainKey, fields.Locked.String(), fields.TotalExtraRecs.String())
	if err != nil {
		if errors.Is(err, aerospike.ErrKeyNotFound) {
			logger.Errorf("Transaction not found: %s", txHashStr)
			fmt.Printf("Error: Transaction not found: %s\n", txHashStr)
			return
		}
		logger.Errorf("Failed to fetch transaction: %v", err)
		fmt.Printf("Error: Failed to fetch transaction: %v\n", err)
		return
	}

	// Check locked status
	locked, ok := record.Bins[fields.Locked.String()].(bool)
	if !ok {
		locked = false
	}

	// Get total extra records
	totalExtraRecs, ok := record.Bins[fields.TotalExtraRecs.String()].(int)
	if !ok {
		totalExtraRecs = 0
	}

	numRecords := 1 + totalExtraRecs

	logger.Infof("Transaction found: %d total records (1 main + %d extra)", numRecords, totalExtraRecs)
	fmt.Printf("Transaction: %s\n", txHashStr)
	fmt.Printf("Total records: %d (1 main + %d extra)\n", numRecords, totalExtraRecs)
	fmt.Printf("Current locked status: %v\n", locked)

	// Check if already unlocked
	if !locked {
		logger.Infof("Transaction is already unlocked")
		fmt.Println("✓ Transaction is already unlocked")
		return
	}

	// Attempt to unlock all records
	fmt.Printf("\nAttempting to unlock %d records...\n", numRecords)
	logger.Infof("Unlocking %d records for transaction %s", numRecords, txHashStr)

	// Perform the unlock operation
	err = aeroStore.UnlockTransaction(ctx, txHash, numRecords)
	if err != nil {
		logger.Errorf("Failed to unlock transaction: %v", err)
		fmt.Printf("\n❌ Failed to unlock transaction: %v\n", err)
		fmt.Println("\nSome records may have been unlocked successfully.")
		fmt.Println("Check the logs above for details on which records failed.")
		return
	}

	// Success!
	logger.Infof("Successfully unlocked all %d records for transaction %s", numRecords, txHashStr)
	fmt.Printf("\n✓ Successfully unlocked all %d records\n", numRecords)
	fmt.Printf("Transaction %s is now fully unlocked and UTXOs can be spent\n", txHashStr)
}
