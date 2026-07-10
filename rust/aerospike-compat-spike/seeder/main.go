// Command spikeseeder writes Aerospike records exactly as Teranode's UTXO store
// would: using the production BSV aerospike client and the production key-source
// logic (uaerospike.CalculateKeySourceInternal). The Rust client then reads /
// mutates these records to prove cross-client wire compatibility.
//
// This is part of the throwaway Gate 0 spike. It is an isolated nested Go module
// and never modifies the Teranode implementation.
package main

import (
	"flag"
	"log"

	aero "github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
)

const (
	namespace = "test"
	set       = "utxos"
)

func main() {
	host := flag.String("host", "127.0.0.1", "aerospike host")
	port := flag.Int("port", 3500, "aerospike port")
	hashHex := flag.String("hash", "", "64-char tx hash hex")
	num := flag.Uint("num", 0, "record index (vout/batchSize)")
	schema := flag.Bool("schema", false, "write a representative UTXO bin-schema record at num=0")
	flag.Parse()

	h, err := chainhash.NewHashFromStr(*hashHex)
	if err != nil {
		log.Fatalf("bad hash %q: %v", *hashHex, err)
	}

	client, err := aero.NewClient(*host, *port)
	if err != nil {
		log.Fatalf("connect %s:%d: %v", *host, *port, err)
	}
	defer client.Close()

	if *schema {
		writeSchema(client, h)
		return
	}
	writeMarker(client, h, uint32(*num))
}

// writeMarker writes a tiny record keyed by the production key source so the Rust
// client must compute the identical digest to find it.
func writeMarker(client *aero.Client, h *chainhash.Hash, num uint32) {
	keySource := uaerospike.CalculateKeySourceInternal(h, num)
	key, err := aero.NewKey(namespace, set, keySource)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	if err := client.Put(nil, key, aero.BinMap{"marker": "from-go", "num": int(num)}); err != nil {
		log.Fatalf("put marker: %v", err)
	}
	log.Printf("seeded marker num=%d keysource_len=%d", num, len(keySource))
}

// writeSchema writes a record shaped like a real master UTXO record: one unspent
// 32-byte utxo entry plus the counter bins the spend UDF reads.
func writeSchema(client *aero.Client, h *chainhash.Hash) {
	utxo0 := make([]byte, 32)
	for i := range utxo0 {
		utxo0[i] = 0x22
	}
	bins := aero.BinMap{
		"utxos":          []interface{}{utxo0},
		"totalUtxos":     1,
		"recordUtxos":    1,
		"spentUtxos":     0,
		"totalExtraRecs": 0,
		"spentExtraRecs": 0,
		"blockIDs":       []interface{}{int(123), int(456)},
		"fee":            1000,
		"sizeInBytes":    256,
	}
	key, err := aero.NewKey(namespace, set, uaerospike.CalculateKeySourceInternal(h, 0))
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	if err := client.Put(nil, key, bins); err != nil {
		log.Fatalf("put schema: %v", err)
	}
	log.Printf("seeded schema record")
}
