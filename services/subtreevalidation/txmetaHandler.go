// Package subtreevalidation provides functionality for validating subtrees in a blockchain context.
// It handles the validation of transaction subtrees, manages transaction metadata caching,
// and interfaces with blockchain and validation services.
package subtreevalidation

import (
	"context"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"google.golang.org/protobuf/proto"
)

// txmetaMessageHandler returns a Kafka message handler for transaction metadata operations.
//
// This wrapper provides the context to the actual handler function.
func (u *Server) txmetaMessageHandler(ctx context.Context) func(msg *kafka.KafkaMessage) error {
	return func(msg *kafka.KafkaMessage) error {
		return u.txmetaHandler(ctx, msg)
	}
}

// txmetaHandler processes Kafka messages for transaction metadata cache operations.
//
// Processing errors (ErrProcessing) are logged and the message is marked as completed
// to prevent infinite retry loops on malformed data.
func (u *Server) txmetaHandler(ctx context.Context, msg *kafka.KafkaMessage) error {
	if msg == nil || len(msg.ConsumerMessage.Value) <= chainhash.HashSize {
		return nil
	}

	// Process the message asynchronously to avoid blocking the Kafka consumer.
	// Errors are logged but do not prevent message acknowledgment.
	go func() {
		startTime := time.Now()

		var m kafkamessage.KafkaTxMetaTopicMessage

		if err := proto.Unmarshal(msg.ConsumerMessage.Value, &m); err != nil {
			return
		}

		hash, err := chainhash.NewHashFromStr(m.TxHash)
		if err != nil {
			return
		}

		if m.Action == kafkamessage.KafkaTxMetaActionType_DELETE {
			if err = u.DelTxMetaCache(ctx, hash); err != nil {
				prometheusSubtreeValidationSetTXMetaCacheKafkaErrors.Inc()
				u.logger.Errorf("[txmetaHandler][%s] failed to delete tx meta data: %v", hash, err)
			}

			prometheusSubtreeValidationDelTXMetaCacheKafka.Observe(float64(time.Since(startTime).Microseconds()) / 1_000_000)
		} else {
			if err = u.SetTxMetaCacheFromBytes(ctx, hash[:], m.Content); err != nil {
				prometheusSubtreeValidationSetTXMetaCacheKafkaErrors.Inc()
				u.logger.Debugf("[txmetaHandler][%s] failed to set tx meta data: %v", hash, err)
			}

			prometheusSubtreeValidationSetTXMetaCacheKafka.Observe(float64(time.Since(startTime).Microseconds()) / 1_000_000)
		}
	}()

	return nil
}
