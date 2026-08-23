package bridge

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"google.golang.org/protobuf/proto"
)

// BlockFinalHandler is called once per successfully decoded blocks-final
// message: a block our own chain has already finalized, for the caller to
// relay to peers (protocol.PeerManager.RelayBlock). It is declared here
// rather than satisfied by a protocol type, because spec §4.4 forbids this
// package from importing protocol — the composition happens in the svp2p
// service package, which imports both.
type BlockFinalHandler func(hash chainhash.Hash, header *wire.BlockHeader)

// StartBlocksFinalConsumer wires the blocks-final Kafka topic
// (settings.Kafka.BlocksFinalConfig) to onFinal, decoding each message the
// way legacy netsync's kafkaBlocksFinalListener does
// (services/legacy/netsync/manager.go:3443-3475): the hash from the message
// key, the header from kafkamessage.KafkaBlocksFinalTopicMessage.Header.
//
// A nil BlocksFinalConfig means the topic is not configured, matching legacy
// (netsync/manager.go:3362-3366, "if blocksFinalConfigURL != nil"): the
// listener is not started at all, and no block is ever relayed by this path.
// It returns (nil, nil) rather than an error — an unconfigured topic is not a
// failure the caller should abort startup over.
//
// It is a PLAIN listener, not legacy's controlled one
// (kafka.StartKafkaControlledListener, netsync/manager.go:3366): legacy's own
// control channel for this specific listener is fed `blockEnabled = true`
// unconditionally by a poller goroutine (netsync/manager.go:3308, "Block
// listeners are always enabled. The only FSM state that previously disabled
// them ... was removed") — dead machinery that never actually pauses this
// topic today. Wiring an unused control channel here to replicate that would
// be exactly the premature complexity bridge.Bridge's own doctrine warns
// against (bridge.go: "not built early as unreachable no-ops"). The one real
// pause/resume story in this area, Task 16's LegacyInvConfig round trip
// (spec §7/§11), is a different topic entirely and owns its own control
// channel; nothing in the plan asks blocks-final to share it. If a future
// task needs to pause this listener, converting a plain
// kafka.NewKafkaConsumerGroupFromURL + Start into a controlled one is a
// small, local change — not a reason to build the control path now.
//
// The returned consumer's lifecycle is the caller's: Start already begins
// consuming (kafka.KafkaConsumerGroup.Start is non-blocking, spawning its own
// goroutine), tied to ctx, and the caller must call Close on it during Stop
// — ahead of tearing down the peer registry, not only cancelling ctx — so no
// new RelayBlock call can start against a registry that is already being
// torn down. (Fix round 1, review finding I3: this is NOT a claim that
// Close proves the underlying consumer goroutine has exited; against
// util/kafka/in_memory_kafka's own fake it provably does not — see
// Server.go's wiring for the full reasoning and kafka_test.go for what the
// lifecycle tests actually establish.)
func StartBlocksFinalConsumer(ctx context.Context, logger ulogger.Logger, tSettings *settings.Settings, onFinal BlockFinalHandler) (kafka.KafkaConsumerGroupI, error) {
	configURL := tSettings.Kafka.BlocksFinalConfig
	if configURL == nil {
		logger.Infof("[svp2p] block announcement relay disabled: kafka_blocksFinalConfig is not set")
		return nil, nil
	}

	groupID := "blocksfinal.legacy." + tSettings.ClientName

	consumer, err := kafka.NewKafkaConsumerGroupFromURL(logger, configURL, groupID, true, &tSettings.Kafka)
	if err != nil {
		return nil, err
	}

	consumer.Start(ctx, func(msg *kafka.KafkaMessage) error {
		return handleBlocksFinalMessage(logger, msg, onFinal)
	})

	return consumer, nil
}

// handleBlocksFinalMessage decodes one blocks-final message and calls
// onFinal. Every parse failure logs and returns nil rather than an error:
// legacy's own discipline here (netsync/manager.go:3443-3475, "not going to
// retry, if we cannot parse the message") — a message this node cannot parse
// will never become parseable, so returning an error and letting the
// consumer retry it would only spin.
func handleBlocksFinalMessage(logger ulogger.Logger, msg *kafka.KafkaMessage, onFinal BlockFinalHandler) error {
	if msg.Key == nil {
		logger.Errorf("[svp2p] blocks-final message has no Kafka key, skipping")
		return nil
	}

	hash, err := chainhash.NewHashFromStr(string(msg.Key))
	if err != nil {
		logger.Errorf("[svp2p] blocks-final message key %q is not a block hash, skipping: %v", string(msg.Key), err)
		return nil
	}

	var blockMsg kafkamessage.KafkaBlocksFinalTopicMessage
	if err := proto.Unmarshal(msg.Value, &blockMsg); err != nil {
		logger.Errorf("[svp2p][%s] failed to unmarshal blocks-final message, skipping: %v", hash, err)
		return nil
	}

	header, err := model.NewBlockHeaderFromBytes(blockMsg.Header)
	if err != nil {
		logger.Errorf("[svp2p][%s] failed to decode block header from blocks-final message, skipping: %v", hash, err)
		return nil
	}

	if *header.Hash() != *hash {
		// The producer always keys the message with block.Header.Hash()
		// (services/blockchain/Server.go sendKafkaBlockFinalNotification), so
		// a mismatch means the message is corrupt in a way proto.Unmarshal did
		// not catch. Trusting the decoded header instead of the key would
		// relay the wrong hash to every peer; skip it instead, for the same
		// never-parseable reason as the errors above. Compared by value
		// (chainhash.Hash is a fixed byte array) rather than by formatting
		// both sides to hex first — fix round 1, review Minor 10.
		logger.Errorf("[svp2p][%s] blocks-final message key does not match its decoded header hash %s, skipping", hash, header.Hash())
		return nil
	}

	// legacy's own log line at the equivalent point (netsync/manager.go:3475,
	// "received block final message from Kafka: %s, %s"), kept so an operator
	// can still see "we announced block X" — fix round 1, review Minor 7.
	logger.Infof("[svp2p] relaying blocks-final message from Kafka: %s", hash)

	onFinal(*hash, header.ToWireBlockHeader())

	return nil
}
