// Package farm provides a robust API for CRDTs on top of multiple clusters.
package farm

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/soundcloud/roshi/cluster"
	"github.com/soundcloud/roshi/common"
	"github.com/soundcloud/roshi/instrumentation"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Farm implements CRDT-semantic ZSET methods over many clusters.
type Farm struct {
	clusters        []cluster.Cluster
	writeQuorum     int
	readStrategy    coreReadStrategy
	repairStrategy  repairStrategy
	instrumentation instrumentation.Instrumentation
}

// New creates and returns a new Farm.
//
// Writes are always sent to all clusters, and writeQuorum determines how many
// individual successful responses need to be received before the client
// receives an overall success. Reads are sent to clusters according to the
// passed ReadStrategy.
//
// Instrumentation may be nil; all other parameters are required.
func New(
	clusters []cluster.Cluster,
	writeQuorum int,
	readStrategy ReadStrategy,
	repairStrategy repairStrategy,
	instr instrumentation.Instrumentation,
) *Farm {
	if instr == nil {
		instr = instrumentation.NopInstrumentation{}
	}
	farm := &Farm{
		clusters:        clusters,
		writeQuorum:     writeQuorum,
		repairStrategy:  repairStrategy,
		instrumentation: instr,
	}
	farm.readStrategy = readStrategy(farm)
	return farm
}

// Insert adds each tuple into each underlying cluster, if the scores are
// greater than the already-stored scores. As long as over half of the clusters
// succeed to write all tuples, the overall write succeeds.
func (f *Farm) Insert(tuples []common.KeyScoreMember) error {
	return f.write(
		tuples,
		func(c cluster.Cluster, a []common.KeyScoreMember) error { return c.Insert(a) },
		insertInstrumentation{f.instrumentation},
	)
}

// Selecter defines a synchronous Select API, implemented by Farm.
type Selecter interface {
	Select(keys []string, offset, limit int) (map[string][]common.KeyScoreMember, error)
}

// Select invokes the ReadStrategy of the farm.
func (f *Farm) Select(keys []string, offset, limit int) (map[string][]common.KeyScoreMember, error) {
	// High performance optimization.
	if len(keys) <= 0 {
		return map[string][]common.KeyScoreMember{}, nil
	}
	return f.readStrategy(keys, offset, limit)
}

// Delete removes each tuple from the underlying clusters, if the score is
// greater than the already-stored scores.
func (f *Farm) Delete(tuples []common.KeyScoreMember) error {
	return f.write(
		tuples,
		func(c cluster.Cluster, a []common.KeyScoreMember) error { return c.Delete(a) },
		deleteInstrumentation{f.instrumentation},
	)
}

// Repair queries all clusters for the most recent score for the given
// keyMember taking both, the deletes key and the inserts key, into
// account. It then propagates that score and if it was connected to
// the deletes or the inserts key to all clusters that are not up to
// date already.
func (f *Farm) Repair(km keyMember) {
	go func() {
		f.instrumentation.RepairCall()
		f.instrumentation.RepairRequestCount(1)
	}()

	began := time.Now()
	clustersUpToDate := map[int]bool{}
	highestScore := 0.
	var wasInserted bool // Whether the highest scoring keyMember was inserted or deleted.

	// Scatter.
	responsesChan := make(chan scoreResponseTuple, len(f.clusters))
	for i, c := range f.clusters {
		go func(i int, c cluster.Cluster) {
			score, wasInserted, err := c.Score(km.Key, km.Member)
			responsesChan <- scoreResponseTuple{i, score, wasInserted, err}
		}(i, c)
	}

	// Gather.
	for i := 0; i < cap(responsesChan); i++ {
		resp := <-responsesChan
		if resp.err != nil {
			f.instrumentation.RepairCheckPartialFailure()
			continue
		}
		if resp.score == highestScore && resp.wasInserted == wasInserted {
			clustersUpToDate[resp.cluster] = true
			continue
		}
		if resp.score > highestScore {
			highestScore = resp.score
			wasInserted = resp.wasInserted
			clustersUpToDate = map[int]bool{}
			clustersUpToDate[resp.cluster] = true
		}
		// Highly unlikely corner case: One cluster returns
		// wasInserted=true, another wasInserted=false, but both
		// return the same score. I that case, we will
		// propagate the first result encountered. (And yes,
		// this situation will screw us up elsewhere, too.)
	}
	go f.instrumentation.RepairCheckDuration(time.Now().Sub(began))

	if highestScore == 0. {
		// All errors (or keyMember not found). Do not proceed.
		f.instrumentation.RepairCheckCompleteFailure()
		return
	}
	if len(clustersUpToDate) == len(f.clusters) {
		// Cool. All clusters agree already. Done.
		f.instrumentation.RepairCheckRedundant()
		return
	}
	// We have a KeyScoreMember, and we have to propagate it to some clusters.
	f.instrumentation.RepairWriteCount()
	ksm := common.KeyScoreMember{Key: km.Key, Score: highestScore, Member: km.Member}
	for i, c := range f.clusters {
		if !clustersUpToDate[i] {
			go func(c cluster.Cluster) {
				defer func(began time.Time) {
					f.instrumentation.RepairWriteDuration(time.Now().Sub(began))
				}(time.Now())
				var err error
				if wasInserted {
					err = c.Insert([]common.KeyScoreMember{ksm})
				} else {
					err = c.Delete([]common.KeyScoreMember{ksm})
				}
				if err == nil {
					f.instrumentation.RepairWriteSuccess()
				} else {
					f.instrumentation.RepairWriteFailure()
				}
			}(c)
		}
	}
}

func (f *Farm) write(
	tuples []common.KeyScoreMember,
	action func(cluster.Cluster, []common.KeyScoreMember) error,
	instr writeInstrumentation,
) error {
	// High performance optimization.
	if len(tuples) <= 0 {
		return nil
	}
	instr.call()
	instr.recordCount(len(tuples))
	defer func(began time.Time) {
		d := time.Now().Sub(began)
		instr.callDuration(d)
		instr.recordDuration(d / time.Duration(len(tuples)))
	}(time.Now())

	// Scatter
	errChan := make(chan error, len(f.clusters))
	for _, c := range f.clusters {
		go func(c cluster.Cluster) {
			errChan <- action(c, tuples)
		}(c)
	}

	// Gather
	errors, got, need := []string{}, 0, f.writeQuorum
	haveQuorum := func() bool { return got-len(errors) >= need }
	for i := 0; i < cap(errChan); i++ {
		err := <-errChan
		if err != nil {
			errors = append(errors, err.Error())
		}
		got++
		if haveQuorum() {
			break
		}
	}

	// Report
	if !haveQuorum() {
		instr.quorumFailure()
		return fmt.Errorf("no quorum (%s)", strings.Join(errors, "; "))
	}
	return nil
}

// unionDifference computes two sets of keys from the input sets. Union is
// defined to be every key-member and its best (highest) score. Difference is
// defined to be those key-members with imperfect agreement across all input
// sets.
func unionDifference(tupleSets []tupleSet) (tupleSet, keyMemberSet) {
	expectedCount := len(tupleSets)
	counts := map[common.KeyScoreMember]int{}
	scores := map[keyMember]float64{}
	for _, tupleSet := range tupleSets {
		for tuple := range tupleSet {
			// For union
			km := keyMember{Key: tuple.Key, Member: tuple.Member}
			if score, ok := scores[km]; !ok || tuple.Score > score {
				scores[km] = tuple.Score
			}
			// For difference
			counts[tuple]++
		}
	}

	union, difference := tupleSet{}, keyMemberSet{}
	for km, bestScore := range scores {
		union.add(common.KeyScoreMember{
			Key:    km.Key,
			Score:  bestScore,
			Member: km.Member,
		})
	}
	for ksm, count := range counts {
		if count < expectedCount {
			difference.add(keyMember{
				Key:    ksm.Key,
				Member: ksm.Member,
			})
		}
	}
	return union, difference
}

type tupleSet map[common.KeyScoreMember]struct{}

func makeSet(a []common.KeyScoreMember) tupleSet {
	s := make(tupleSet, len(a))
	for _, tuple := range a {
		s.add(tuple)
	}
	return s
}

func (s tupleSet) add(tuple common.KeyScoreMember) {
	s[tuple] = struct{}{}
}

func (s tupleSet) has(tuple common.KeyScoreMember) bool {
	_, ok := s[tuple]
	return ok
}

func (s tupleSet) addMany(other tupleSet) {
	for tuple := range other {
		s.add(tuple)
	}
}

func (s tupleSet) slice() []common.KeyScoreMember {
	a := make([]common.KeyScoreMember, 0, len(s))
	for tuple := range s {
		a = append(a, tuple)
	}
	return a
}

func (s tupleSet) orderedLimitedSlice(limit int) []common.KeyScoreMember {
	a := s.slice()
	sort.Sort(common.KeyScoreMembers(a))
	if len(a) > limit {
		a = a[:limit]
	}
	return a
}

type keyMemberSet map[keyMember]struct{}

func (s keyMemberSet) add(km keyMember) {
	s[km] = struct{}{}
}

func (s keyMemberSet) addMany(other keyMemberSet) {
	for km := range other {
		s.add(km)
	}
}

func (s keyMemberSet) slice() []keyMember {
	a := make([]keyMember, 0, len(s))
	for km := range s {
		a = append(a, km)
	}
	return a
}

type writeInstrumentation interface {
	call()
	recordCount(int)
	callDuration(time.Duration)
	recordDuration(time.Duration)
	quorumFailure()
}

type insertInstrumentation struct {
	instrumentation.Instrumentation
}

func (i insertInstrumentation) call()                          { i.InsertCall() }
func (i insertInstrumentation) recordCount(n int)              { i.InsertRecordCount(n) }
func (i insertInstrumentation) callDuration(d time.Duration)   { i.InsertCallDuration(d) }
func (i insertInstrumentation) recordDuration(d time.Duration) { i.InsertRecordDuration(d) }
func (i insertInstrumentation) quorumFailure()                 { i.InsertQuorumFailure() }

type deleteInstrumentation struct {
	instrumentation.Instrumentation
}

func (i deleteInstrumentation) call()                          { i.DeleteCall() }
func (i deleteInstrumentation) recordCount(n int)              { i.DeleteRecordCount(n) }
func (i deleteInstrumentation) callDuration(d time.Duration)   { i.DeleteCallDuration(d) }
func (i deleteInstrumentation) recordDuration(d time.Duration) { i.DeleteRecordDuration(d) }
func (i deleteInstrumentation) quorumFailure()                 { i.DeleteQuorumFailure() }

type scoreResponseTuple struct {
	cluster     int
	score       float64
	wasInserted bool // false = this keyMember was deleted
	err         error
}
