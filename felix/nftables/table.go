// Copyright (c) 2016-2022 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nftables

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/felix/environment"
	"github.com/projectcalico/calico/felix/iptables/cmdshim"
	"github.com/projectcalico/calico/felix/logutils"
	logutilslc "github.com/projectcalico/calico/libcalico-go/lib/logutils"
	"github.com/projectcalico/calico/libcalico-go/lib/set"
	"sigs.k8s.io/knftables"
)

const (
	MaxChainNameLength   = 28
	minPostWriteInterval = 50 * time.Millisecond
)

var (
	// List of all the top-level chains by table.
	tableToChains = map[string][]string{
		"cali-filter": {"INPUT", "FORWARD", "OUTPUT"},
		"cali-nat":    {"PREROUTING", "INPUT", "OUTPUT", "POSTROUTING"},
		"cali-mangle": {"PREROUTING", "INPUT", "FORWARD", "OUTPUT", "POSTROUTING"},
		"cali-raw":    {"PREROUTING", "OUTPUT"},
	}

	// Prometheus metrics.
	countNumRestoreCalls = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "felix_nft_calls",
		Help: "Number of nft calls.",
	})
	countNumRestoreErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "felix_nft_errors",
		Help: "Number of nft errors.",
	})
	countNumSaveCalls = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "felix_nft_list_calls",
		Help: "Number of nft list calls.",
	})
	countNumSaveErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "felix_nft_list_errors",
		Help: "Number of nft list errors.",
	})
	gaugeNumChains = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "felix_nft_chains",
		Help: "Number of active nft chains.",
	}, []string{"ip_version", "table"})
	gaugeNumRules = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "felix_nft_rules",
		Help: "Number of active nftables rules.",
	}, []string{"ip_version", "table"})
	countNumLinesExecuted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "felix_nftables_lines_executed",
		Help: "Number of nftables rule updates executed.",
	}, []string{"ip_version", "table"})
)

func init() {
	prometheus.MustRegister(countNumRestoreCalls)
	prometheus.MustRegister(countNumRestoreErrors)
	prometheus.MustRegister(countNumSaveCalls)
	prometheus.MustRegister(countNumSaveErrors)
	prometheus.MustRegister(gaugeNumChains)
	prometheus.MustRegister(gaugeNumRules)
	prometheus.MustRegister(countNumLinesExecuted)
}

// Table represents a single nftable table i.e. "raw", "nat", "filter", etc.  It
// caches the desired state of that table, then attempts to bring it into sync when Apply() is
// called.
//
// # API Model
//
// Table supports two classes of operation:  "rule insertions" and "full chain updates".
//
// As the name suggests, rule insertions allow for inserting one or more rules into a preexisting
// chain.  Rule insertions are intended to be used to hook kernel chains (such as "FORWARD") in
// order to direct them to a Felix-owned chain.  It is important to minimise the use of rule
// insertions because the top-level chains are shared resources, which can be modified by other
// applications.  In addition, rule insertions are harder to clean up after an upgrade to a new
// version of Felix (because we need a way to recognise our rules in a crowded chain).
//
// Full chain updates replace the entire contents of a Felix-owned chain with a new set of rules.
// Limiting the operation to "replace whole chain" in this way significantly simplifies the API.
// Although the API operates on full chains, the dataplane write logic tries to avoid rewriting
// a whole chain if only part of it has changed (this was not the case in Felix 1.4).  This
// prevents counters from being reset unnecessarily.
//
// In either case, the actual dataplane updates are deferred until the next call to Apply() so
// chain updates and insertions may occur in any order as long as they are consistent (i.e. there
// are no references to nonexistent chains) by the time Apply() is called.
//
// # Design
//
// We had several goals in designing the iptables machinery in 2.0.0:
//
// (1) High performance. Felix needs to handle high churn of endpoints and rules.
//
// (2) Ability to restore rules, even if other applications accidentally break them: we found that
// other applications sometimes misuse iptables-save and iptables-restore to do a read, modify,
// write cycle. That behaviour is not safe under concurrent modification.
//
// (3) Avoid rewriting rules that haven't changed so that we don't reset iptables counters.
//
// (4) Avoid parsing iptables commands (for example, the output from iptables/iptables-save).
// This is very hard to do robustly because iptables rules do not necessarily round-trip through
// the kernel in the same form.  In addition, the format could easily change due to changes or
// fixes in the iptables/iptables-save command.
//
// (5) Support for graceful restart.  I.e. deferring potentially incorrect updates until we're
// in-sync with the datastore.  For example, if we have 100 endpoints on a host, after a restart
// we don't want to write a "dispatch" chain when we learn about the first endpoint (possibly
// replacing an existing one that had all 100 endpoints in place and causing traffic to glitch);
// instead, we want to defer until we've seen all 100 and then do the write.
//
// (6) Improved handling of rule inserts vs Felix 1.4.x.  Previous versions of Felix sometimes
// inserted special-case rules that were not marked as Calico rules in any sensible way making
// cleanup of those rules after an upgrade difficult.
//
// # Implementation
//
// For high performance (goal 1), we use iptables-restore to do bulk updates to iptables.  This is
// much faster than individual iptables calls.
//
// To allow us to restore rules after they are clobbered by another process (goal 2), we cache
// them at this layer.  This means that we don't need a mechanism to ask the other layers of Felix
// to do a resync.  Note: Table doesn't start a thread of its own so it relies on the main event
// loop to trigger any dataplane resync polls.
//
// There is tension between goals 3 and 4.  In order to avoid full rewrites (goal 3), we need to
// know what rules are in place, but we also don't want to parse them to find out (goal 4)!  As
// a compromise, we deterministically calculate an ID for each rule and store it in an iptables
// comment.  Then, when we want to know what rules are in place, we _do_ parse the output from
// iptables-save, but only to read back the rule IDs.  That limits the amount of parsing we need
// to do and keeps it manageable/robust.
//
// To support graceful restart (goal 5), we defer updates to the dataplane until Apply() is called,
// then we do an atomic update using iptables-restore.  As long as the first Apply() call is
// after we're in sync, the dataplane won't be touched until the right time.  Felix 1.4.x had a
// more complex mechanism to support partial updates during the graceful restart period but
// Felix 2.0.0 resyncs so quickly that the added complexity is not justified.
//
// To make it easier to manage rule insertions (goal 6), we add rule IDs to those too.  With
// rule IDs in place, we can easily distinguish Calico rules from non-Calico rules without needing
// to know exactly which rules to expect.  To deal with cleanup after upgrade from older versions
// that did not write rule IDs, we support special-case regexes to detect our old rules.
//
// # Thread safety
//
// Table doesn't do any internal synchronization, its methods should only be called from one
// thread.  To avoid conflicts in the dataplane itself, there should only be one instance of
// Table for each table in an application.
type Table struct {
	Name      string
	IPVersion uint8
	nft       knftables.Interface

	// featureDetector detects the features of the dataplane.
	featureDetector environment.FeatureDetectorIface

	// chainToInsertedRules maps from chain name to a list of rules to be inserted at the start
	// of that chain.  Rules are written with rule hash comments.  The Table cleans up inserted
	// rules with unknown hashes.
	chainToInsertedRules map[string][]Rule
	// chainToAppendRules maps from chain name to a list of rules to be appended at the end
	// of that chain.
	chainToAppendedRules map[string][]Rule
	dirtyInsertAppend    set.Set[string]

	// chainNameToChain contains the desired state of our chains, indexed by
	// chain name.
	chainNameToChain map[string]*Chain

	// chainRefCounts counts the number of chains that refer to a given chain.  Transitive
	// reachability isn't tracked but testing whether a chain is referenced does allow us to
	// avoid programming unreferenced leaf chains (for example, policies that aren't used in
	// this table).
	chainRefCounts map[string]int
	dirtyChains    set.Set[string]

	inSyncWithDataPlane bool

	// chainToDataplaneHashes contains the rule hashes that we think are in the dataplane.
	// it is updated when we write to the dataplane but it can also be read back and compared
	// to what we calculate from chainToContents.
	chainToDataplaneHashes map[string][]string

	// chainToFullRules contains the full rules for any chains that we may be hooking into, mapped from chain name
	// to slices of rules in that chain.
	chainToFullRules map[string][]*knftables.Rule

	// hashCommentPrefix holds the prefix that we prepend to our rule-tracking hashes.
	hashCommentPrefix string
	// hashCommentRegexp matches the rule-tracking comment, capturing the rule hash.
	hashCommentRegexp *regexp.Regexp
	// ourChainsRegexp matches the names of chains that are "ours", i.e. start with one of our
	// prefixes.
	ourChainsRegexp *regexp.Regexp

	// insertMode is either "insert" or "append"; whether we insert our rules or append them
	// to top-level chains.
	insertMode string

	// Record when we did our most recent reads and writes of the table.  We use these to
	// calculate the next time we should force a refresh.
	lastReadTime             time.Time
	lastWriteTime            time.Time
	initialPostWriteInterval time.Duration
	postWriteInterval        time.Duration
	refreshInterval          time.Duration

	// Estimates for the time taken to do an nftables read / write.
	// When an nft command exceeds the one of these we update them immediately.
	// When a  nft command takes less time we decay them exponentially.
	peakNftablesReadTime  time.Duration
	peakNftablesWriteTime time.Duration

	logCxt               *log.Entry
	updateRateLimitedLog *logutilslc.RateLimitedLogger

	gaugeNumChains        prometheus.Gauge
	gaugeNumRules         prometheus.Gauge
	countNumLinesExecuted prometheus.Counter

	// Factory for making commands, used by UTs to shim exec.Command().
	newCmd cmdshim.CmdFactory

	// Shims for time.XXX functions:
	timeSleep func(d time.Duration)
	timeNow   func() time.Time

	// lookPath is a shim for exec.LookPath.
	lookPath func(file string) (string, error)

	onStillAlive func()
	opReporter   logutils.OpRecorder
	reason       string
}

type TableOptions struct {
	HistoricChainPrefixes    []string
	ExtraCleanupRegexPattern string
	InsertMode               string
	RefreshInterval          time.Duration
	PostWriteInterval        time.Duration

	// NewCmdOverride for tests, if non-nil, factory to use instead of the real exec.Command()
	NewCmdOverride cmdshim.CmdFactory
	// SleepOverride for tests, if non-nil, replacement for time.Sleep()
	SleepOverride func(d time.Duration)
	// NowOverride for tests, if non-nil, replacement for time.Now()
	NowOverride func() time.Time
	// LookPathOverride for tests, if non-nil, replacement for exec.LookPath()
	LookPathOverride func(file string) (string, error)
	// Thunk to call periodically when doing a long-running operation.
	OnStillAlive func()
	// OpRecorder to tell when we do resyncs etc.
	OpRecorder logutils.OpRecorder
}

func NewTable(
	name string,
	ipVersion uint8,
	hashPrefix string,
	featureDetector environment.FeatureDetectorIface,
	options TableOptions,
) *Table {
	// Calculate the regex used to match the hash comment.  The comment looks like this:
	// comment "cali:abcd1234_-".
	hashCommentRegexp := regexp.MustCompile(`comment "?` + hashPrefix + `([a-zA-Z0-9_-]+)"?`)
	ourChainsPattern := "^(" + strings.Join(options.HistoricChainPrefixes, "|") + ")"
	ourChainsRegexp := regexp.MustCompile(ourChainsPattern)

	oldInsertRegexpParts := []string{}
	for _, prefix := range options.HistoricChainPrefixes {
		part := fmt.Sprintf("(?:-j|--jump) %s", prefix)
		oldInsertRegexpParts = append(oldInsertRegexpParts, part)
	}
	if options.ExtraCleanupRegexPattern != "" {
		oldInsertRegexpParts = append(oldInsertRegexpParts,
			options.ExtraCleanupRegexPattern)
	}
	oldInsertPattern := strings.Join(oldInsertRegexpParts, "|")
	log.WithField("pattern", oldInsertPattern).Info("Calculated old-insert detection regex.")

	// Pre-populate the insert and append table with empty lists for each kernel chain.  Ensures that we
	// clean up any chains that we hooked on a previous run.
	inserts := map[string][]Rule{}
	appends := map[string][]Rule{}
	dirtyInsertAppend := set.New[string]()
	refcounts := map[string]int{}
	for _, kernelChain := range tableToChains[name] {
		inserts[kernelChain] = []Rule{}
		appends[kernelChain] = []Rule{}
		dirtyInsertAppend.Add(kernelChain)
		// Kernel chains are referred to by definition.
		refcounts[kernelChain] += 1
	}

	var insertMode string
	switch options.InsertMode {
	case "", "insert":
		insertMode = "insert"
	case "append":
		insertMode = "append"
	default:
		log.WithField("insertMode", options.InsertMode).Panic("Unknown insert mode")
	}

	if options.PostWriteInterval <= minPostWriteInterval {
		log.WithFields(log.Fields{
			"setValue": options.PostWriteInterval,
			"default":  minPostWriteInterval,
		}).Info("PostWriteInterval too small, defaulting.")
		options.PostWriteInterval = minPostWriteInterval
	}

	// Allow override of exec.Command() and time.Sleep() for test purposes.
	newCmd := cmdshim.NewRealCmd
	if options.NewCmdOverride != nil {
		newCmd = options.NewCmdOverride
	}
	sleep := time.Sleep
	if options.SleepOverride != nil {
		sleep = options.SleepOverride
	}
	now := time.Now
	if options.NowOverride != nil {
		now = options.NowOverride
	}
	lookPath := exec.LookPath
	if options.LookPathOverride != nil {
		lookPath = options.LookPathOverride
	}

	logFields := log.Fields{
		"ipVersion": ipVersion,
		"table":     name,
	}

	nft, err := knftables.New(knftables.IPv4Family, name)
	if err != nil {
		log.WithError(err).Panic("Failed to create nftables table")
	}

	table := &Table{
		Name:                   name,
		nft:                    nft,
		IPVersion:              ipVersion,
		featureDetector:        featureDetector,
		chainToInsertedRules:   inserts,
		chainToAppendedRules:   appends,
		dirtyInsertAppend:      dirtyInsertAppend,
		chainNameToChain:       map[string]*Chain{},
		chainRefCounts:         refcounts,
		dirtyChains:            set.New[string](),
		chainToDataplaneHashes: map[string][]string{},
		chainToFullRules:       map[string][]*knftables.Rule{},
		logCxt:                 log.WithFields(logFields),
		updateRateLimitedLog: logutilslc.NewRateLimitedLogger(
			logutilslc.OptInterval(30*time.Second),
			logutilslc.OptBurst(100),
		).WithFields(logFields),
		hashCommentPrefix: hashPrefix,
		hashCommentRegexp: hashCommentRegexp,
		ourChainsRegexp:   ourChainsRegexp,
		insertMode:        insertMode,

		// Initialise the write tracking as if we'd just done a write, this will trigger
		// us to recheck the dataplane at exponentially increasing intervals at startup.
		// Note: if we didn't do this, the calculation logic would need to be modified
		// to cope with zero values for these fields.
		lastWriteTime:            now(),
		initialPostWriteInterval: options.PostWriteInterval,
		postWriteInterval:        options.PostWriteInterval,

		refreshInterval: options.RefreshInterval,

		newCmd:    newCmd,
		timeSleep: sleep,
		timeNow:   now,
		lookPath:  lookPath,

		gaugeNumChains:        gaugeNumChains.WithLabelValues(fmt.Sprintf("%d", ipVersion), name),
		gaugeNumRules:         gaugeNumRules.WithLabelValues(fmt.Sprintf("%d", ipVersion), name),
		countNumLinesExecuted: countNumLinesExecuted.WithLabelValues(fmt.Sprintf("%d", ipVersion), name),
		opReporter:            options.OpRecorder,
	}

	if options.OnStillAlive != nil {
		table.onStillAlive = options.OnStillAlive
	} else {
		table.onStillAlive = func() {}
	}

	return table
}

// InsertOrAppendRules sets the rules that should be inserted into or appended
// to the given non-Calico chain (depending on the chain insert mode).  See
// also AppendRules, which can be used to record additional rules that are
// always appended.
func (t *Table) InsertOrAppendRules(chainName string, rules []Rule) {
	t.logCxt.WithField("chainName", chainName).Debug("Updating rule insertions")
	oldRules := t.chainToInsertedRules[chainName]
	t.chainToInsertedRules[chainName] = rules
	numRulesDelta := len(rules) - len(oldRules)
	t.gaugeNumRules.Add(float64(numRulesDelta))
	t.dirtyInsertAppend.Add(chainName)

	// Incref any newly-referenced chains, then decref the old ones.  By incrementing first we
	// avoid marking a still-referenced chain as dirty.
	t.maybeIncrefReferredChains(chainName, rules)
	t.maybeDecrefReferredChains(chainName, oldRules)

	// Defensive: updates to insert/append is very rare and the top-level
	// chains are contended with other apps.  Make sure we re-read the state
	// of the chains before updating them.
	t.InvalidateDataplaneCache("insertion")
}

// AppendRules sets the rules to be appended to a given non-Calico chain.
// These rules are always appended, even if chain insert mode is "insert".
// If chain insert mode is "append", these rules are appended after any
// rules added with InsertOrAppendRules.
func (t *Table) AppendRules(chainName string, rules []Rule) {
	t.logCxt.WithField("chainName", chainName).Debug("Updating rule appends")
	oldRules := t.chainToAppendedRules[chainName]
	t.chainToAppendedRules[chainName] = rules
	numRulesDelta := len(rules) - len(oldRules)
	t.gaugeNumRules.Add(float64(numRulesDelta))
	t.dirtyInsertAppend.Add(chainName)

	// Incref any newly-referenced chains, then decref the old ones.  By incrementing first we
	// avoid marking a still-referenced chain as dirty.
	t.maybeIncrefReferredChains(chainName, rules)
	t.maybeDecrefReferredChains(chainName, oldRules)

	// Defensive: updates to insert/append is very rare and the top-level
	// chains are contended with other apps.  Make sure we re-read the state
	// of the chains before updating them.
	t.InvalidateDataplaneCache("insertion")
}

func (t *Table) UpdateChains(chains []*Chain) {
	for _, chain := range chains {
		t.UpdateChain(chain)
	}
}

func (t *Table) UpdateChain(chain *Chain) {
	t.logCxt.WithField("chainName", chain.Name).Debug("Adding chain to available set.")
	oldNumRules := 0

	// Incref any newly-referenced chains, then decref the old ones.  By incrementing first we
	// avoid marking a still-referenced chain as dirty.
	t.maybeIncrefReferredChains(chain.Name, chain.Rules)
	if oldChain := t.chainNameToChain[chain.Name]; oldChain != nil {
		oldNumRules = len(oldChain.Rules)
		t.maybeDecrefReferredChains(chain.Name, oldChain.Rules)
	}
	t.chainNameToChain[chain.Name] = chain
	numRulesDelta := len(chain.Rules) - oldNumRules
	t.gaugeNumRules.Add(float64(numRulesDelta))
	if t.chainIsReferenced(chain.Name) {
		t.dirtyChains.Add(chain.Name)

		// Defensive: make sure we re-read the dataplane state before we make updates.  While the
		// code was originally designed not to need this, we found that other users of
		// nftables can still clobber our updates so it's safest to re-read the state before
		// each write.
		t.InvalidateDataplaneCache("chain update")
	}
}

func (t *Table) RemoveChains(chains []*Chain) {
	for _, chain := range chains {
		t.RemoveChainByName(chain.Name)
	}
}

func (t *Table) RemoveChainByName(name string) {
	t.logCxt.WithField("chainName", name).Debug("Removing chain from available set.")
	if oldChain, known := t.chainNameToChain[name]; known {
		t.gaugeNumRules.Sub(float64(len(oldChain.Rules)))
		t.maybeDecrefReferredChains(name, oldChain.Rules)
		delete(t.chainNameToChain, name)
		if t.chainIsReferenced(name) {
			t.dirtyChains.Add(name)

			// Defensive: make sure we re-read the dataplane state before we make updates.  While the
			// code was originally designed not to need this, we found that other users of
			// nftables can still clobber out updates so it's safest to re-read the state before
			// each write.
			t.InvalidateDataplaneCache("chain removal")
		}
	}
}

func (t *Table) chainIsReferenced(name string) bool {
	return t.chainRefCounts[name] > 0
}

// maybeIncrefReferredChains checks whether the named chain is referenced;
// if so, it increfs all child chains.  If a child chain becomes newly
// referenced, its children are increffed recursively.
func (t *Table) maybeIncrefReferredChains(chainName string, rules []Rule) {
	if !t.chainIsReferenced(chainName) {
		return
	}
	for _, r := range rules {
		if ref, ok := r.Action.(Referrer); ok {
			t.increfChain(ref.ReferencedChain())
		}
	}
}

// maybeDecrefReferredChains checks whether the named chain is referenced;
// if so, it decrefs all child chains.  If a child chain becomes newly
// unreferenced, its children are decreffed recursively.
func (t *Table) maybeDecrefReferredChains(chainName string, rules []Rule) {
	if !t.chainIsReferenced(chainName) {
		return
	}
	for _, r := range rules {
		if ref, ok := r.Action.(Referrer); ok {
			t.decrefChain(ref.ReferencedChain())
		}
	}
}

// increfChain increments the refcount of the given chain; if the refcount transitions from 0,
// marks the chain dirty so it will be programmed.
func (t *Table) increfChain(chainName string) {
	log.WithField("chainName", chainName).Debug("Incref chain")
	t.chainRefCounts[chainName] += 1
	if t.chainRefCounts[chainName] == 1 {
		t.updateRateLimitedLog.WithField("chainName", chainName).Info("Chain became referenced, marking it for programming")
		t.dirtyChains.Add(chainName)
		if chain := t.chainNameToChain[chainName]; chain != nil {
			// Recursively incref chains that this chain refers to.  If
			// chain == nil then the chain is likely about to be added, in
			// which case we'll handle this whe the chain is added.
			t.maybeIncrefReferredChains(chainName, chain.Rules)
		}
	}
}

// decrefChain decrements the refcount of the given chain; if the refcount transitions to 0,
// marks the chain dirty so it will be cleaned up.
func (t *Table) decrefChain(chainName string) {
	log.WithField("chainName", chainName).Debug("Decref chain")
	if t.chainRefCounts[chainName] == 1 {
		t.updateRateLimitedLog.WithField("chainName", chainName).Info("Chain no longer referenced, marking it for removal")
		if chain := t.chainNameToChain[chainName]; chain != nil {
			// Recursively decref chains that this chain refers to.  If
			// chain == nil then the chain has probably already been deleted
			// in which case we'll already have done the decrefs.
			t.maybeDecrefReferredChains(chainName, chain.Rules)
		}
		delete(t.chainRefCounts, chainName)
		t.dirtyChains.Add(chainName)
		return
	}

	// Chain still referenced, just decrement.
	t.chainRefCounts[chainName] -= 1
}

func (t *Table) loadDataplaneState() {
	// Refresh the cache of feature data.
	t.featureDetector.RefreshFeatures()

	// Load the hashes from the dataplane.
	t.logCxt.Debug("Loading current nftables state and checking it is correct.")
	t.opReporter.RecordOperation(fmt.Sprintf("resync-%v-v%d", t.Name, t.IPVersion))

	t.lastReadTime = t.timeNow()

	dataplaneHashes, dataplaneRules := t.getHashesAndRulesFromDataplane()

	// Check that the rules we think we've programmed are still there and mark any inconsistent
	// chains for refresh.
	for chainName, expectedHashes := range t.chainToDataplaneHashes {
		logCxt := t.logCxt.WithField("chainName", chainName)
		if t.dirtyChains.Contains(chainName) || t.dirtyInsertAppend.Contains(chainName) {
			// Already an update pending for this chain; no point in flagging it as
			// out-of-sync.
			logCxt.Debug("Skipping known-dirty chain")
			continue
		}
		dpHashes := dataplaneHashes[chainName]
		if !t.ourChainsRegexp.MatchString(chainName) {
			// Not one of our chains so it may be one that we're inserting rules into.
			insertedRules := t.chainToInsertedRules[chainName]
			if len(insertedRules) == 0 {
				// This chain shouldn't have any inserts, make sure that's the
				// case.  This case also covers the case where a chain was removed,
				// making dpHashes nil.
				dataplaneHasInserts := false
				for _, hash := range dpHashes {
					if hash != "" {
						dataplaneHasInserts = true
						break
					}
				}
				if dataplaneHasInserts {
					logCxt.WithField("actualRuleIDs", dpHashes).Warn(
						"Chain had unexpected inserts, marking for resync")
					t.dirtyInsertAppend.Add(chainName)
				}
				continue
			}

			// Re-calculate the expected rule insertions based on the current length
			// of the chain (since other processes may have inserted/removed rules
			// from the chain, throwing off the numbers).
			expectedHashes, _, _ = t.expectedHashesForInsertAppendChain(
				chainName,
				numEmptyStrings(dpHashes),
			)
			if !reflect.DeepEqual(dpHashes, expectedHashes) {
				logCxt.WithFields(log.Fields{
					"expectedRuleIDs": expectedHashes,
					"actualRuleIDs":   dpHashes,
				}).Warn("Detected out-of-sync inserts, marking for resync")
				t.dirtyInsertAppend.Add(chainName)
			}
		} else {
			// One of our chains, should match exactly.
			if !reflect.DeepEqual(dpHashes, expectedHashes) {
				logCxt.Warn("Detected out-of-sync Calico chain, marking for resync")
				t.dirtyChains.Add(chainName)
			}
		}
	}

	// Now scan for chains that shouldn't be there and mark for deletion.
	t.logCxt.Debug("Scanning for unexpected nftables chains")
	for chainName, dataplaneHashes := range dataplaneHashes {
		logCxt := t.logCxt.WithField("chainName", chainName)
		if t.dirtyChains.Contains(chainName) || t.dirtyInsertAppend.Contains(chainName) {
			// Already an update pending for this chain.
			logCxt.Debug("Skipping known-dirty chain")
			continue
		}
		if _, ok := t.chainToDataplaneHashes[chainName]; ok {
			// Chain expected, we'll have checked its contents above.
			logCxt.Debug("Skipping expected chain")
			continue
		}
		if !t.ourChainsRegexp.MatchString(chainName) {
			// Non-calico chain that is not tracked in chainToDataplaneHashes. We
			// haven't seen the chain before and we haven't been asked to insert
			// anything into it.  Check that it doesn't have an rule insertions in it
			// from a previous run of Felix.
			for _, hash := range dataplaneHashes {
				if hash != "" {
					logCxt.Info("Found unexpected insert, marking for cleanup")
					t.dirtyInsertAppend.Add(chainName)
					break
				}
			}
			continue
		}
		// Chain exists in dataplane but not in memory, mark as dirty so we'll clean it up.
		logCxt.Info("Found unexpected chain, marking for cleanup")
		t.dirtyChains.Add(chainName)
	}

	t.logCxt.Debug("Finished loading nftables state")
	t.chainToDataplaneHashes = dataplaneHashes
	t.chainToFullRules = dataplaneRules
	t.inSyncWithDataPlane = true
}

// expectedHashesForInsertAppendChain calculates the expected hashes for a whole top-level chain
// given our inserts and appends.
// Hashes for inserted rules are calculated first. If we're in append mode, that consists of numNonCalicoRules empty strings
// followed by our inserted hashes; in insert mode, the opposite way round. Hashes for appended rules are calculated and
// appended at the end.
// To avoid recalculation, it returns the inserted rule hashes as a second output and appended rule hashes
// a third output.
func (t *Table) expectedHashesForInsertAppendChain(
	chainName string,
	numNonCalicoRules int,
) (allHashes, ourInsertedHashes, ourAppendedHashes []string) {
	insertedRules := t.chainToInsertedRules[chainName]
	appendedRules := t.chainToAppendedRules[chainName]
	allHashes = make([]string, len(insertedRules)+len(appendedRules)+numNonCalicoRules)
	features := t.featureDetector.GetFeatures()
	if len(insertedRules) > 0 {
		ourInsertedHashes = CalculateRuleHashes(chainName, insertedRules, features)
	}
	if len(appendedRules) > 0 {
		// Add *append* to chainName to produce a unique hash in case append chain/rules are same
		// as insert chain/rules above.
		ourAppendedHashes = CalculateRuleHashes(chainName+"*appends*", appendedRules, features)
	}
	offset := 0
	if t.insertMode == "append" {
		log.Debug("In append mode, returning our hashes at end.")
		offset = numNonCalicoRules
	}
	for i, hash := range ourInsertedHashes {
		allHashes[i+offset] = hash
	}

	offset = len(insertedRules) + numNonCalicoRules
	for i, hash := range ourAppendedHashes {
		allHashes[i+offset] = hash
	}
	return
}

// getHashesAndRulesFromDataplane loads the current state of our table. It parses out the hashes that we
// add to rules and, for chains that we insert into, the full rules. The 'hashes' map contains an entry for each chain
// in the table. Each entry is a slice containing the hashes for the rules in that table. Rules with no hashes are
// represented by an empty string. The 'rules' map contains an entry for each non-Calico chain in the table that
// contains inserts. It is used to generate deletes using the full rule, rather than deletes by line number, to avoid
// race conditions on chains we don't fully control.
func (t *Table) getHashesAndRulesFromDataplane() (hashes map[string][]string, rules map[string][]*knftables.Rule) {
	retries := 3
	retryDelay := 100 * time.Millisecond

	// Retry a few times before we panic.  This deals with any transient errors and it prevents
	// us from spamming a panic into the log when we're being gracefully shut down by a SIGTERM.
	for {
		t.onStillAlive()
		hashes, rules, err := t.attemptToGetHashesAndRulesFromDataplane()
		if err != nil {
			countNumSaveErrors.Inc()
			var stderr string
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			t.logCxt.WithError(err).WithField("stderr", stderr).Warn("nftables command failed")
			if retries > 0 {
				retries--
				t.timeSleep(retryDelay)
				retryDelay *= 2
			} else {
				t.logCxt.Panic("nftables command failed after retries")
			}
			continue
		}

		return hashes, rules
	}
}

// attemptToGetHashesAndRulesFromDataplane reads nftables state and loads it into memory.
func (t *Table) attemptToGetHashesAndRulesFromDataplane() (hashes map[string][]string, rules map[string][]*knftables.Rule, err error) {
	startTime := t.timeNow()
	defer func() {
		saveDuration := t.timeNow().Sub(startTime)
		t.peakNftablesReadTime = t.peakNftablesReadTime * 99 / 100
		if saveDuration > t.peakNftablesReadTime {
			log.WithField("duration", saveDuration).Debug("Updating nftables peak duration.")
			t.peakNftablesReadTime = saveDuration
		}
	}()

	t.logCxt.Debug("Attmempting to get hashes and rules from nftables")
	chains, err := t.nft.List(context.TODO(), "chain")
	if err != nil {
		return nil, nil, err
	}
	countNumSaveCalls.Inc()

	hashes = make(map[string][]string)
	rules = make(map[string][]*knftables.Rule)

	for _, chain := range chains {
		hashes[chain] = []string{}
		rulesInChain, err := t.nft.ListRules(context.TODO(), chain)
		if err != nil {
			return nil, nil, err
		}
		rules[chain] = rulesInChain
		for _, rule := range rulesInChain {
			hash := ""
			if rule.Comment != nil {
				hash = strings.TrimPrefix(strings.Split(*rule.Comment, ":")[0], t.hashCommentPrefix)
			}
			log.WithField("rule", rule).
				WithField("hash", hash).
				WithField("handle", rule.Handle).
				Info("Found rule")
			hashes[chain] = append(hashes[chain], hash)
		}
	}
	return
}

func (t *Table) InvalidateDataplaneCache(reason string) {
	logCxt := t.logCxt.WithField("reason", reason)
	if !t.inSyncWithDataPlane {
		logCxt.Debug("Would invalidate dataplane cache but it was already invalid.")
		return
	}
	logCxt.Debug("Invalidating dataplane cache")
	t.inSyncWithDataPlane = false
	t.reason = reason
}

func (t *Table) Apply() (rescheduleAfter time.Duration) {
	now := t.timeNow()
	defer func() {
		if time.Since(now) > time.Second {
			log.WithFields(log.Fields{
				"applyTime":      time.Since(now),
				"reasonForApply": t.reason,
			}).Info("Updating nftables took >1s")
		}
	}()
	// We _think_ we're in sync, check if there are any reasons to think we might
	// not be in sync.
	lastReadToNow := now.Sub(t.lastReadTime)
	invalidated := false
	if t.refreshInterval > 0 && lastReadToNow > t.refreshInterval {
		// Too long since we've forced a refresh.
		t.InvalidateDataplaneCache("refresh timer")
		invalidated = true
	}
	// To workaround the possibility of another process clobbering our updates, we refresh the
	// dataplane after we do a write at exponentially increasing intervals.  We do a refresh
	// if the delta from the last write to now is twice the delta from the last read.
	for t.postWriteInterval != 0 &&
		t.postWriteInterval < time.Hour &&
		!now.Before(t.lastWriteTime.Add(t.postWriteInterval)) {

		t.postWriteInterval *= 2
		t.logCxt.WithField("newPostWriteInterval", t.postWriteInterval).Debug("Updating post-write interval")
		if !invalidated {
			t.InvalidateDataplaneCache("post update")
			invalidated = true
		}
	}

	// Retry until we succeed.  There are several reasons that updating nftables may fail:
	//
	// - A concurrent write may invalidate compare-and-swap; this manifests
	//   as a failure on the COMMIT line.
	// - Another process may have clobbered some of our state, resulting in inconsistencies
	//   in what we try to program.  This could manifest in a number of ways depending on what
	//   the other process did.
	// - Random transient failure.
	//
	// It's also possible that we're bugged and trying to write bad data so we give up
	// eventually.
	retries := 10
	backoffTime := 1 * time.Millisecond
	failedAtLeastOnce := false
	for {
		if !t.inSyncWithDataPlane {
			// We have reason to believe that our picture of the dataplane is out of
			// sync.  Refresh it.  This may mark more chains as dirty.
			t.loadDataplaneState()
		}
		t.onStillAlive()

		if err := t.applyUpdates(); err != nil {
			if retries > 0 {
				retries--
				t.logCxt.WithError(err).Warn("Failed to program nftables, will retry")
				t.timeSleep(backoffTime)
				backoffTime *= 2
				t.logCxt.WithError(err).Warn("Retrying...")
				failedAtLeastOnce = true
				continue
			} else {
				t.logCxt.WithError(err).Error("Failed to program nftables, loading diags before panic.")
				cmd := t.newCmd("nft", "list", "table", t.Name)
				output, err2 := cmd.Output()
				if err2 != nil {
					t.logCxt.WithError(err2).Error("Failed to load nftables state")
				} else {
					t.logCxt.WithField("state", string(output)).Error("Current state of nftables")
				}
				t.logCxt.WithError(err).Panic("Failed to program nftables, giving up after retries")
			}
		}
		if failedAtLeastOnce {
			t.logCxt.Warn("Succeeded after retry.")
		}
		break
	}

	t.gaugeNumChains.Set(float64(len(t.chainRefCounts)))

	// Check whether we need to be rescheduled and how soon.
	if t.refreshInterval > 0 {
		// Refresh interval is set, start with that.
		lastReadToNow = now.Sub(t.lastReadTime)
		rescheduleAfter = t.refreshInterval - lastReadToNow
	}
	if t.postWriteInterval < time.Hour {
		postWriteReached := t.lastWriteTime.Add(t.postWriteInterval).Sub(now)
		if postWriteReached <= 0 {
			rescheduleAfter = 1 * time.Millisecond
		} else if t.refreshInterval <= 0 || postWriteReached < rescheduleAfter {
			rescheduleAfter = postWriteReached
		}
	}

	return
}

func (t *Table) applyUpdates() error {
	// If needed, detect the dataplane features.
	features := t.featureDetector.GetFeatures()

	// Start a new nftables transaction.
	tx := t.nft.NewTransaction()

	// Add the table, as it must always exist and isn't created by default.
	tx.Add(&knftables.Table{})

	// Also make sure our base chains exist.
	for _, kernelChain := range tableToChains[t.Name] {
		// TODO: These need hooks / priority / etc.
		tx.Add(&knftables.Chain{Name: kernelChain})
	}

	// Make a pass over the dirty chains and generate a forward reference for any that we're about to update.
	// Writing a forward reference ensures that the chain exists and that it is empty.
	t.dirtyChains.Iter(func(chainName string) error {
		if _, present := t.desiredStateOfChain(chainName); !present {
			// About to delete this chain, flush it first to sever dependencies.
			tx.Flush(&knftables.Chain{Name: chainName})
		} else if _, ok := t.chainToDataplaneHashes[chainName]; !ok {
			// Chain doesn't exist in dataplane, mark it for creation.
			tx.Add(&knftables.Chain{Name: chainName})
		}
		return nil
	})

	// Make a second pass over the dirty chains.  This time, we write out the rule changes.
	newHashes := map[string][]string{}
	t.dirtyChains.Iter(func(chainName string) error {
		if chain, ok := t.desiredStateOfChain(chainName); ok {
			// Chain update or creation.  Scan the chain against its previous hashes
			// and replace/append/delete as appropriate.
			var previousHashes []string
			previousHashes = t.chainToDataplaneHashes[chainName]
			currentHashes := chain.RuleHashes(features)
			newHashes[chainName] = currentHashes

			// Make sure maps are created for the chain, as nft will faill the transaction
			// if there are unreferenced maps.
			for _, mapName := range chain.IPSetNames() {
				tx.Add(&knftables.Set{Name: mapName, Type: "ipv4_addr"})
			}

			for i := 0; i < len(previousHashes) || i < len(currentHashes); i++ {
				if i < len(previousHashes) && i < len(currentHashes) {
					if previousHashes[i] == currentHashes[i] {
						continue
					}
					// Hash doesn't match, replace the rule.
					prefixFrag := t.commentFrag(currentHashes[i])
					rendered := chain.Rules[i].Render(chainName, prefixFrag, features)
					rendered.Handle = t.chainToFullRules[chainName][i].Handle
					tx.Replace(rendered)
				} else if i < len(previousHashes) {
					// previousHashes was longer, remove the old rules from the end.
					prefixFrag := t.commentFrag(currentHashes[i])
					rendered := chain.Rules[i].Render(chainName, prefixFrag, features)
					rendered.Handle = t.chainToFullRules[chainName][i].Handle
					tx.Delete(rendered)
				} else {
					// currentHashes was longer.  Append.
					prefixFrag := t.commentFrag(currentHashes[i])
					tx.Add(chain.Rules[i].Render(chainName, prefixFrag, features))
				}
			}
		}
		return nil // Delay clearing the set until we've programmed nftables.
	})

	// Make a copy of our full rules map and keep track of all changes made while processing dirtyInsertAppend.
	// When we've successfully updated nftables, we'll update our cache of chainToFullRules with this map.
	newChainToFullRules := map[string][]*knftables.Rule{}
	for chain, rules := range t.chainToFullRules {
		newChainToFullRules[chain] = make([]*knftables.Rule, len(rules))
		copy(newChainToFullRules[chain], rules)
	}

	// Now calculate nftables updates for our inserted and appended rules, which are used to hook top-level chains.
	t.dirtyInsertAppend.Iter(func(chainName string) error {
		previousHashes := t.chainToDataplaneHashes[chainName]
		newRules := newChainToFullRules[chainName]

		// Calculate the hashes for our inserted and appended rules.
		newChainHashes, newInsertedRuleHashes, newAppendedRuleHashes := t.expectedHashesForInsertAppendChain(chainName, numEmptyStrings(previousHashes))

		if reflect.DeepEqual(newChainHashes, previousHashes) {
			// Chain is in sync, skip to next one.
			return nil
		}

		// For simplicity, if we've discovered that we're out-of-sync, remove all our
		// rules from this chain, then re-insert/re-append them below.
		tx.Flush(&knftables.Chain{Name: chainName})

		// Go over our slice of "new" rules and create a copy of the slice with just the rules we didn't empty out.
		copyOfNewRules := []*knftables.Rule{}
		for _, rule := range newRules {
			if rule != nil {
				copyOfNewRules = append(copyOfNewRules, rule)
			}
		}
		newRules = copyOfNewRules
		rules := t.chainToInsertedRules[chainName]

		// Add inserted rules if there is any
		if len(rules) > 0 {
			if t.insertMode == "insert" {
				t.logCxt.Debug("Rendering insert rules.")
				// Since each insert is pushed onto the top of the chain, do the inserts in
				// reverse order so that they end up in the correct order in the final
				// state of the chain.
				for i := len(rules) - 1; i >= 0; i-- {
					prefixFrag := t.commentFrag(newInsertedRuleHashes[i])
					tx.Insert(rules[i].Render(chainName, prefixFrag, features))
				}
			} else {
				t.logCxt.Debug("Rendering append rules.")
				for i := 0; i < len(rules); i++ {
					prefixFrag := t.commentFrag(newInsertedRuleHashes[i])
					tx.Add(rules[i].Render(chainName, prefixFrag, features))
				}
			}
		}

		// Add appended rules if there is any
		rules = t.chainToAppendedRules[chainName]
		if len(rules) > 0 {
			t.logCxt.Debug("Rendering specific append rules.")
			for i := 0; i < len(rules); i++ {
				prefixFrag := t.commentFrag(newAppendedRuleHashes[i])
				tx.Add(rules[i].Render(chainName, prefixFrag, features))
			}
		}

		newHashes[chainName] = newChainHashes
		newChainToFullRules[chainName] = newRules

		return nil // Delay clearing the set until we've programmed nftables.
	})

	// Do deletions at the end.  This ensures that we don't try to delete any chains that
	// are still referenced (because we'll have removed the references in the modify pass
	// above).  Note: if a chain is being deleted at the same time as a chain that it refers to
	// then we'll issue a create+flush instruction in the very first pass, which will sever the
	// references.
	t.dirtyChains.Iter(func(chainName string) error {
		if _, ok := t.desiredStateOfChain(chainName); !ok {
			// Chain deletion
			tx.Delete(&knftables.Chain{Name: chainName})
			newHashes[chainName] = nil
		}
		return nil // Delay clearing the set until we've programmed nftables.
	})

	if len(tx.String()) == 0 {
		t.logCxt.Debug("Update ended up being no-op, skipping call to nftables.")
	} else {
		// Run the transaction.
		t.opReporter.RecordOperation(fmt.Sprintf("update-%v-v%d", t.Name, t.IPVersion))

		if err := t.nft.Run(context.TODO(), tx); err != nil {
			log.WithField("tx", tx.String()).Error("Failed to run nft transaction")
			return fmt.Errorf("error performing nft transaction: %s", err)
		}

		t.lastWriteTime = t.timeNow()
		t.postWriteInterval = t.initialPostWriteInterval
	}

	if t.postWriteInterval != 0 {
		// If nft is taking a long time (as measured by
		// the peakNftables fields), make sure that we don't try to
		// recheck the nftables state too soon.
		dynamicMinPostWriteInterval := (t.peakNftablesReadTime + t.peakNftablesWriteTime) * 2
		if t.postWriteInterval < dynamicMinPostWriteInterval {
			log.WithFields(log.Fields{
				"dynamicMin":  dynamicMinPostWriteInterval,
				"peakSave":    t.peakNftablesReadTime,
				"peakRestore": t.peakNftablesWriteTime,
			}).Debug(
				"Post write interval shorter than time to read/write nftables, applying dynamic minimum.")
			t.postWriteInterval = dynamicMinPostWriteInterval
		}
	}

	// Now we've successfully updated nftables, clear the dirty sets.  We do this even if we
	// found there was nothing to do above, since we may have found out that a dirty chain
	// was actually a no-op update.
	t.dirtyChains = set.New[string]()
	t.dirtyInsertAppend = set.New[string]()

	// Store off the updates.
	for chainName, hashes := range newHashes {
		if hashes == nil {
			delete(t.chainToDataplaneHashes, chainName)
		} else {
			t.chainToDataplaneHashes[chainName] = hashes
		}
	}
	t.chainToFullRules = newChainToFullRules

	// CASEY: TODO: Hack to load data plane state after every write. This is temporary to make sure
	// we load rule handles for use in replace / deletes.
	log.Info("Reloading data plane state after successful write.")
	t.loadDataplaneState()
	log.Info("Done reloading data plane state after write.")

	return nil
}

// CheckRulesPresent returns list of rules with the hashes that are already
// programmed. Return value of nil means that none of the rules are present.
func (t *Table) CheckRulesPresent(chain string, rules []Rule) []Rule {
	features := t.featureDetector.GetFeatures()
	hashes := CalculateRuleHashes(chain, rules, features)

	dpHashes, _ := t.getHashesAndRulesFromDataplane()
	dpHashesSet := set.New[string]()
	for _, h := range dpHashes[chain] {
		dpHashesSet.Add(h)
	}

	var present []Rule
	for i, r := range rules {
		if dpHashesSet.Contains(hashes[i]) {
			present = append(present, r)
		}
	}

	return present
}

// InsertRulesNow insets the given rules immediately without removing or syncing
// other rules. This is primarily useful when bootstrapping and we cannot wait
// until we have the full state.
func (t *Table) InsertRulesNow(chain string, rules []Rule) error {
	features := t.featureDetector.GetFeatures()
	hashes := CalculateRuleHashes(chain, rules, features)

	tx := t.nft.NewTransaction()
	tx.Add(&knftables.Table{})
	for i, r := range rules {
		prefixFrag := t.commentFrag(hashes[i])
		// buf.WriteLine(r.RenderInsertAtRuleNumber(t.Name, chain, i+1, prefixFrag, features))
		tx.Insert(r.Render(chain, prefixFrag, features))
	}

	// Run the transaction.
	if err := t.nft.Run(context.TODO(), tx); err != nil {
		log.WithField("tx", tx.String()).Error("Failed to run InsertRulesNow nft transaction")
		return fmt.Errorf("error performing InsertRulesNow nft transaction: %s", err)
	}

	return nil
}

// desiredStateOfChain returns the given chain, if and only if it exists in the cache and it is referenced by some
// other chain.  If the chain doesn't exist or it is not referenced, returns nil and false.
func (t *Table) desiredStateOfChain(chainName string) (chain *Chain, present bool) {
	if !t.chainIsReferenced(chainName) {
		return
	}
	chain, present = t.chainNameToChain[chainName]
	return
}

func (t *Table) commentFrag(hash string) string {
	return fmt.Sprintf(`%s%s`, t.hashCommentPrefix, hash)
}

// renderDeleteByIndexLine produces a delete line by rule number. This function is used for cali chains.
func (t *Table) renderDeleteByIndexLine(chainName string, ruleNum int) string {
	return fmt.Sprintf("-D %s %d", chainName, ruleNum)
}

func CalculateRuleHashes(chainName string, rules []Rule, features *environment.Features) []string {
	chain := Chain{
		Name:  chainName,
		Rules: rules,
	}
	return (&chain).RuleHashes(features)
}

func numEmptyStrings(strs []string) int {
	count := 0
	for _, s := range strs {
		if s == "" {
			count++
		}
	}
	return count
}

// NoopTable fulfils the Table interface but does nothing.
type NoopTable struct{}

func NewNoopTable() *NoopTable {
	return new(NoopTable)
}

func (t *NoopTable) Name() string                                       { return "" }
func (t *NoopTable) IPVersion() uint8                                   { return 0 }
func (t *NoopTable) InsertOrAppendRules(chainName string, rules []Rule) {}
func (t *NoopTable) AppendRules(chainName string, rules []Rule)         {}
func (t *NoopTable) UpdateChain(chain *Chain)                           {}
func (t *NoopTable) UpdateChains([]*Chain)                              {}
func (t *NoopTable) RemoveChains([]*Chain)                              {}
func (t *NoopTable) RemoveChainByName(name string)                      {}
func (t *NoopTable) InvalidateDataplaneCache(reason string)             {}
func (t *NoopTable) Apply() time.Duration                               { return 0 }
