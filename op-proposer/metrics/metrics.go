package metrics

import (
	"io"

	pmetrics "github.com/ethereum-optimism/optimism/op-proposer/metrics"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	opmetrics "github.com/ethereum-optimism/optimism/op-service/metrics"
	txmetrics "github.com/ethereum-optimism/optimism/op-service/txmgr/metrics"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/prometheus/client_golang/prometheus"
)

// implements the Registry getter, for metrics HTTP server to hook into
var _ opmetrics.RegistryMetricer = (*Metrics)(nil)

type Metrics struct {
	ns       string
	registry *prometheus.Registry
	factory  opmetrics.Factory

	opmetrics.RefMetrics
	txmetrics.TxMetrics
	opmetrics.RPCMetrics

	info prometheus.GaugeVec
	up   prometheus.Gauge

	L1Cache      *opmetrics.CacheMetrics
	L2Cache      *opmetrics.CacheMetrics
	WitnessCache *opmetrics.CacheMetrics

	// L2OutputProposals tracks the number of L2 output proposals
	L2OutputProposals prometheus.Counter
}

var _ pmetrics.Metricer = (*Metrics)(nil)

func NewMetrics(procName string) *Metrics {
	if procName == "" {
		procName = "default"
	}
	ns := pmetrics.Namespace + "_" + procName

	registry := opmetrics.NewRegistry()
	factory := opmetrics.With(registry)

	return &Metrics{
		ns:       ns,
		registry: registry,
		factory:  factory,

		RefMetrics: opmetrics.MakeRefMetrics(ns, factory),
		TxMetrics:  txmetrics.MakeTxMetrics(ns, factory),
		RPCMetrics: opmetrics.MakeRPCMetrics(ns, factory),

		info: *factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "info",
			Help:      "Pseudo-metric tracking version and config info",
		}, []string{
			"version",
		}),
		up: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "up",
			Help:      "1 if the op-proposer has finished starting up",
		}),

		L1Cache:      opmetrics.NewCacheMetrics(factory, ns, "l1_cache", "L1 cache"),
		L2Cache:      opmetrics.NewCacheMetrics(factory, ns, "l2_cache", "L2 cache"),
		WitnessCache: opmetrics.NewCacheMetrics(factory, ns, "witness_cache", "Witness cache"),

		L2OutputProposals: factory.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "l2_output_proposals",
			Help:      "Number of L2 output proposals",
		}),
	}
}

func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

func (m *Metrics) StartBalanceMetrics(l log.Logger, client ethereum.ChainStateReader, account common.Address) io.Closer {
	return opmetrics.LaunchBalanceMetrics(l, m.registry, m.ns, client, account)
}

// RecordInfo sets a pseudo-metric that contains versioning and
// config info for the op-proposer.
func (m *Metrics) RecordInfo(version string) {
	m.info.WithLabelValues(version).Set(1)
}

// RecordUp sets the up metric to 1.
func (m *Metrics) RecordUp() {
	prometheus.MustRegister()
	m.up.Set(1)
}

// RecordL2BlocksProposed should be called when new L2 block is proposed
func (m *Metrics) RecordL2BlocksProposed(l2ref eth.L2BlockRef) {
	m.RecordL2Ref(pmetrics.BlockProposed, l2ref)
}

// RecordL2Proposal records metrics related to L2 output proposals with the given L2 block number
func (m *Metrics) RecordL2Proposal(l2BlockNumber uint64) {
	m.L2OutputProposals.Inc()
}

func (m *Metrics) Document() []opmetrics.DocumentedMetric {
	return m.factory.Document()
}
