package bridge

import (
	"sync"

	"github.com/bsv-blockchain/teranode/util"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics relocated from the legacy netsync package's metrics.go, trimmed to the
// subset handle_block.go and subtree_partition.go actually observe (the
// tx-message and orphan-pool metrics have no relocated consumer in Phase 2 —
// see Decision 1 in the svp2p Phase 2 plan) and renamed/re-subsystemed so a
// coexisting legacy netsync process cannot collide with this package's
// registrations under the same metric name.
var (
	prometheusSvp2pBridgeBlockHeight                    prometheus.Gauge
	prometheusSvp2pBridgeHandleBlockDirect              prometheus.Histogram
	prometheusSvp2pBridgeProcessBlock                   prometheus.Histogram
	prometheusSvp2pBridgePrepareSubtrees                prometheus.Histogram
	prometheusSvp2pBridgeValidateTransactionsLegacyMode prometheus.Histogram
	prometheusSvp2pBridgePreValidateTransactions        prometheus.Histogram
	prometheusSvp2pBridgeValidateTransactions           prometheus.Histogram
	prometheusSvp2pBridgeExtendTransactions             prometheus.Histogram
	prometheusSvp2pBridgeCreateUtxos                    prometheus.Histogram
	prometheusSvp2pBridgeBlockTxSize                    prometheus.Histogram
	prometheusSvp2pBridgeBlockTxNrInputs                prometheus.Histogram
	prometheusSvp2pBridgeBlockTxNrOutputs               prometheus.Histogram
	prometheusSvp2pBridgeBlockTxValidate                prometheus.Histogram

	// prometheusSvp2pBridgePrewarmErrors counts validator errors observed during the
	// pre-warm path in validateTransactions, labelled by error class. The pre-warm runs
	// before full subtree validation and intentionally drops errors (real validation
	// catches consensus violations on its own), but ops still need a signal so they
	// can detect bursts of service/processing failures that would otherwise be silent.
	// Class labels: tx_invalid, service, processing, policy, other.
	prometheusSvp2pBridgePrewarmErrors *prometheus.CounterVec

	prometheusMetricsInitOnce sync.Once
)

func initPrometheusMetrics() {
	prometheusMetricsInitOnce.Do(_initPrometheusMetrics)
}

func _initPrometheusMetrics() {
	prometheusSvp2pBridgeBlockHeight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "block_height",
		Help:      "The height of the block being processed",
	})
	prometheus.MustRegister(prometheusSvp2pBridgeBlockHeight)

	prometheusSvp2pBridgeHandleBlockDirect = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "handle_block_direct",
		Help:      "The time taken to handle a block directly",
		Buckets:   util.MetricsBucketsSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgeHandleBlockDirect)

	prometheusSvp2pBridgeProcessBlock = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "process_block",
		Help:      "The time taken to process a block",
		Buckets:   util.MetricsBucketsSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgeProcessBlock)

	prometheusSvp2pBridgePrepareSubtrees = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "prepare_subtrees",
		Help:      "The time taken to prepare the subtrees",
		Buckets:   util.MetricsBucketsMilliLongSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgePrepareSubtrees)

	prometheusSvp2pBridgeValidateTransactionsLegacyMode = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "validate_transactions_legacy_mode",
		Help:      "The time taken to validate transactions in legacy mode",
		Buckets:   util.MetricsBucketsMilliLongSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgeValidateTransactionsLegacyMode)

	prometheusSvp2pBridgeExtendTransactions = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "extend_transactions",
		Help:      "The time taken to extend transactions",
		Buckets:   util.MetricsBucketsMilliLongSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgeExtendTransactions)

	prometheusSvp2pBridgePreValidateTransactions = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "pre_validate_transactions",
		Help:      "The time taken to pre-validate transactions",
		Buckets:   util.MetricsBucketsMilliLongSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgePreValidateTransactions)

	prometheusSvp2pBridgeValidateTransactions = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "validate_transactions",
		Help:      "The time taken to validate transactions",
		Buckets:   util.MetricsBucketsMilliLongSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgeValidateTransactions)

	prometheusSvp2pBridgeCreateUtxos = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "create_utxos",
		Help:      "The time taken to create UTXOs",
		Buckets:   util.MetricsBucketsMilliLongSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgeCreateUtxos)

	prometheusSvp2pBridgeBlockTxSize = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "block_tx_size",
		Help:      "The size of the transactions in the block being processed",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 20),
	})
	prometheus.MustRegister(prometheusSvp2pBridgeBlockTxSize)

	prometheusSvp2pBridgeBlockTxNrInputs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "block_tx_nr_inputs",
		Help:      "The number of inputs in the block being processed",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 20),
	})
	prometheus.MustRegister(prometheusSvp2pBridgeBlockTxNrInputs)

	prometheusSvp2pBridgeBlockTxNrOutputs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "block_tx_nr_outputs",
		Help:      "The number of outputs in the block being processed",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 20),
	})
	prometheus.MustRegister(prometheusSvp2pBridgeBlockTxNrOutputs)

	prometheusSvp2pBridgeBlockTxValidate = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "block_tx_validate",
		Help:      "The time taken to validate a transaction",
		Buckets:   util.MetricsBucketsMilliSeconds,
	})
	prometheus.MustRegister(prometheusSvp2pBridgeBlockTxValidate)

	prometheusSvp2pBridgePrewarmErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "teranode",
		Subsystem: "svp2p_bridge",
		Name:      "prewarm_validation_errors_total",
		Help:      "Number of validator errors observed during the pre-warm path in validateTransactions, by class",
	}, []string{"class"})
	prometheus.MustRegister(prometheusSvp2pBridgePrewarmErrors)
}
