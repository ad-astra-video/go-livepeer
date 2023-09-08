package server

import (
	"container/heap"
	"context"
	"math/rand"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/net"
)

const SELECTOR_LATENCY_SCORE_THRESHOLD = 1.0

var SelectRandFreq float64
var StakeWeight int
var PriceWeight int
var PerformanceWeight int
var StakeLimit SelectorLimit
var PriceLimit SelectorLimit
var PerformanceLimit SelectorLimit

// BroadcastSessionsSelector selects the next BroadcastSession to use
type BroadcastSessionsSelector interface {
	Add(sessions []*BroadcastSession)
	Complete(sess *BroadcastSession)
	Select(ctx context.Context) *BroadcastSession
	Size() int
	Clear()
}

type BroadcastSessionsSelectorFactory func() BroadcastSessionsSelector

type sessHeap []*BroadcastSession

func (h sessHeap) Len() int {
	return len(h)
}

func (h sessHeap) Less(i, j int) bool {
	return h[i].LatencyScore < h[j].LatencyScore
}

func (h sessHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *sessHeap) Push(x interface{}) {
	sess := x.(*BroadcastSession)
	*h = append(*h, sess)
}

func (h *sessHeap) Pop() interface{} {
	// Pop from the end because heap.Pop() swaps the 0th index element with the last element
	// before calling this method
	// See https://golang.org/src/container/heap/heap.go?s=2190:2223#L50
	old := *h
	n := len(old)
	sess := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]

	return sess
}

func (h *sessHeap) Peek() interface{} {
	if h.Len() == 0 {
		return nil
	}

	// The minimum element is at the 0th index as long as we always modify
	// sessHeap using the heap.Push() and heap.Pop() methods
	// See https://golang.org/pkg/container/heap/
	return (*h)[0]
}

type SelectorWeights struct {
	Scores     map[ethcommon.Address]int64
	TotalStake int64
}

type OrchSelectionData struct {
	Stake       int64
	Price       int64
	Performance int64
}

type SelectorLimit struct {
	Minimum int64
	Maximum int64
}

type orchSelectionDataReader interface {
	GetData(addrs []ethcommon.Address, prices map[ethcommon.Address]*net.PriceInfo) (map[ethcommon.Address]OrchSelectionData, error)
}

type storeOrchSelectionDataReader struct {
	store common.OrchestratorStore
}

func (r *storeOrchSelectionDataReader) GetData(addrs []ethcommon.Address, prices map[ethcommon.Address]*net.PriceInfo) (map[ethcommon.Address]OrchSelectionData, error) {
	orchs, err := r.store.SelectOrchs(&common.DBOrchFilter{Addresses: addrs})
	if err != nil {
		return nil, err
	}

	// The returned map may not have the stake weights for all addresses and the caller should handle this case
	orchData := make(map[ethcommon.Address]OrchSelectionData)

	for _, orch := range orchs {
		ethAddr := ethcommon.HexToAddress(orch.EthereumAddr)
		orchNetPrice, err := common.RatPriceInfo(prices[ethAddr])
		if err != nil {
			continue
		}
		orchPrice, err := common.PriceToFixed(orchNetPrice)
		if err != nil {
			continue
		}

		orchData[ethcommon.HexToAddress(orch.EthereumAddr)] = OrchSelectionData{Stake: orch.Stake, Performance: orch.Score, Price: orchPrice}
	}

	return orchData, nil
}

// MinLSSelector selects the next BroadcastSession with the lowest latency score if it is good enough.
// Otherwise, it selects a session that does not have a latency score yet
// MinLSSelector is not concurrency safe so the caller is responsible for ensuring safety for concurrent method calls
type WeightedSelector struct {
	unknownSessions []*BroadcastSession
	knownSessions   *sessHeap

	orchSelDataReader orchSelectionDataReader
	stakeWeight       int
	priceWeight       int
	performanceWeight int

	stakeLimit       SelectorLimit
	priceLimit       SelectorLimit
	performanceLimit SelectorLimit

	minLS float64
	// Frequency to randomly select unknown sessions
	randFreq float64
}

// NewMinLSSelector returns an instance of MinLSSelector configured with a good enough latency score
func NewWeightedSelector(orchselDataReader orchSelectionDataReader) *WeightedSelector {
	knownSessions := &sessHeap{}
	heap.Init(knownSessions)

	return &WeightedSelector{
		knownSessions:     knownSessions,
		orchSelDataReader: orchselDataReader,
		stakeWeight:       StakeWeight,
		priceWeight:       PriceWeight,
		performanceWeight: PerformanceWeight,
		stakeLimit:        StakeLimit,
		priceLimit:        PriceLimit,
		performanceLimit:  PerformanceLimit,
	}
}

func NewWeightedSelectorWithRandFreq(orchselDataReader orchSelectionDataReader, randFreq float64) *WeightedSelector {
	sel := NewWeightedSelector(orchselDataReader)
	sel.randFreq = randFreq

	return sel
}

func (s *WeightedSelector) SetSelectorWeights(stakeWeight *int, priceWeight *int, performanceWeight *int) {
	if stakeWeight != nil {
		s.stakeWeight = *stakeWeight
	}
	if priceWeight != nil {
		s.priceWeight = *priceWeight
	}
	if performanceWeight != nil {
		s.performanceWeight = *performanceWeight
	}
}

func (s *WeightedSelector) SetSelectorLimits(stakeLimit *SelectorLimit, priceLimit *SelectorLimit, performanceLimit *SelectorLimit) {
	if stakeLimit != nil {
		s.stakeLimit = *stakeLimit
	}

	if priceLimit != nil {
		s.priceLimit = *priceLimit
	}

	if performanceLimit != nil {
		s.performanceLimit = *performanceLimit
	}

}

// Add adds the sessions to the selector's list of sessions without a latency score
func (s *WeightedSelector) Add(sessions []*BroadcastSession) {
	s.unknownSessions = append(s.unknownSessions, sessions...)
}

// Complete adds the session to the selector's list sessions with a latency score
func (s *WeightedSelector) Complete(sess *BroadcastSession) {
	heap.Push(s.knownSessions, sess)
}

// Select returns the session with the lowest latency score if it is good enough.
// Otherwise, a session without a latency score yet is returned
func (s *WeightedSelector) Select(ctx context.Context) *BroadcastSession {
	sess := s.knownSessions.Peek()
	if sess == nil {
		return s.selectUnknownSession(ctx)
	}

	minSess := sess.(*BroadcastSession)
	if minSess.LatencyScore > s.minLS && len(s.unknownSessions) > 0 {
		return s.selectUnknownSession(ctx)
	}

	return heap.Pop(s.knownSessions).(*BroadcastSession)
}

// Size returns the number of sessions stored by the selector
func (s *WeightedSelector) Size() int {
	return len(s.unknownSessions) + s.knownSessions.Len()
}

// Clear resets the selector's state
func (s *WeightedSelector) Clear() {
	s.unknownSessions = nil
	s.knownSessions = &sessHeap{}
	s.orchSelDataReader = nil
}

func (s *WeightedSelector) totalWeights() int {
	return (s.stakeWeight + s.priceWeight + s.performanceWeight)
}

func (s *WeightedSelector) calculateWeights(ctx context.Context) *SelectorWeights {
	var addrs []ethcommon.Address
	addrCount := make(map[ethcommon.Address]int)
	prices := make(map[ethcommon.Address]*net.PriceInfo)

	for _, sess := range s.unknownSessions {
		if sess.OrchestratorInfo.GetTicketParams() == nil {
			continue
		}
		addr := ethcommon.BytesToAddress(sess.OrchestratorInfo.TicketParams.Recipient)
		if _, ok := addrCount[addr]; !ok {
			addrs = append(addrs, addr)
		}
		prices[addr] = sess.OrchestratorInfo.PriceInfo
		addrCount[addr]++
	}

	// Fetch stake weights for all addresses
	// We handle the possibility of missing stake weights for addresses when we run weighted random selection on unknownSessions
	orchSelData, err := s.orchSelDataReader.GetData(addrs, prices)
	// If we fail to read stake weights of unknownSessions we should not continue with selection
	if err != nil {
		clog.Errorf(ctx, "failed to read orchestrator selection data err=%q", err)
		return nil
	}

	totalStake := int64(0)
	sessionsStakeLimit := SelectorLimit{Minimum: 0, Maximum: 0}
	sessionsPriceLimit := SelectorLimit{Minimum: 0, Maximum: 0}
	sessionsPerfLimit := SelectorLimit{Minimum: 0, Maximum: 0}
	for _, orch := range orchSelData {
		//apply stake limits
		if s.stakeLimit.Minimum != 0 && orch.Stake < s.stakeLimit.Minimum {
			orch.Stake = int64(0)
		}
		if s.stakeLimit.Maximum != 0 && orch.Stake > s.stakeLimit.Maximum {
			orch.Stake = s.stakeLimit.Maximum
		}
		//apply price limits
		if s.priceLimit.Minimum != 0 && orch.Price < s.priceLimit.Minimum {
			orch.Price = int64(0)
		}
		if s.priceLimit.Maximum != 0 && orch.Price > s.priceLimit.Maximum {
			orch.Price = int64(0)
		}
		//apply performance limits
		if s.performanceLimit.Minimum != 0 && orch.Performance < s.performanceLimit.Minimum {
			orch.Performance = int64(0)
		}
		if s.performanceLimit.Maximum != 0 && orch.Performance > s.performanceLimit.Maximum {
			orch.Performance = int64(0)
		}

		if orch.Stake != 0 && orch.Price != 0 && orch.Performance != 0 {
			totalStake += orch.Stake
			//track working set bounds for weighted score
			if orch.Stake < s.stakeLimit.Minimum || s.stakeLimit.Minimum == 0 {
				sessionsStakeLimit.Minimum = orch.Stake
			}
			if sessionsStakeLimit.Maximum < orch.Stake {
				sessionsStakeLimit.Maximum = orch.Stake
			}
			if orch.Price < s.priceLimit.Minimum || s.priceLimit.Minimum == 0 {
				sessionsPriceLimit.Minimum = orch.Price
			}
			if sessionsPriceLimit.Maximum < orch.Price {
				sessionsPriceLimit.Maximum = orch.Price
			}
			if orch.Performance < s.performanceLimit.Minimum || s.performanceLimit.Minimum == 0 {
				sessionsPerfLimit.Minimum = orch.Performance
			}
			if sessionsPerfLimit.Maximum < orch.Performance {
				sessionsPerfLimit.Maximum = orch.Performance
			}
		}
	}

	//calculate scores
	sessionsWeights := SelectorWeights{TotalStake: totalStake, Scores: make(map[ethcommon.Address]int64)}
	sessionsTotalWeights := int64(s.totalWeights())
	sessionsStakeWeight := int64(s.stakeWeight)
	sessionsPriceWeight := int64(s.priceWeight)
	sessionsPerfWeight := int64(s.performanceWeight)
	for addr, orch := range orchSelData {
		addrStakeScore := ((orch.Stake - sessionsStakeLimit.Minimum) / (sessionsStakeLimit.Maximum - sessionsStakeLimit.Minimum) * (sessionsStakeWeight / sessionsTotalWeights))
		addrPerfScore := ((orch.Performance - sessionsPerfLimit.Minimum) / (sessionsPerfLimit.Maximum - sessionsPerfLimit.Minimum) * (sessionsPerfWeight / sessionsTotalWeights))
		addrPriceScore := (1 - ((orch.Price-sessionsPriceLimit.Minimum)/(sessionsPriceLimit.Maximum-sessionsPriceLimit.Minimum))*(sessionsPriceWeight/sessionsTotalWeights))
		sessionsWeights.Scores[addr] = addrStakeScore + addrPriceScore + addrPerfScore
	}

	return &sessionsWeights
}

// Use stake weighted random selection to select from unknownSessions
func (s *WeightedSelector) selectUnknownSession(ctx context.Context) *BroadcastSession {
	if len(s.unknownSessions) == 0 {
		return nil
	}

	if s.orchSelDataReader == nil {
		// Sessions are selected based on the order of unknownSessions in off-chain mode
		sess := s.unknownSessions[0]
		s.unknownSessions = s.unknownSessions[1:]
		return sess
	}

	// Select an unknown session randomly based on randFreq frequency
	if rand.Float64() < s.randFreq {
		i := rand.Intn(len(s.unknownSessions))
		sess := s.unknownSessions[i]
		s.removeUnknownSession(i)
		return sess
	}

	sessionsWeights := s.calculateWeights(ctx)
	if sessionsWeights == nil {
		// return first session, weighted scores did not calculate due to error pulling stakes
		sess := s.unknownSessions[0]
		s.unknownSessions = s.unknownSessions[1:]
		return sess

	}

	r := int64(0)
	// Generate a random stake weight between 1 and totalStake
	if sessionsWeights.TotalStake > 0 {
		r = 1 + rand.Int63n(sessionsWeights.TotalStake)
	}

	// Run a weighted random selection on unknownSessions
	// We iterate through each session and subtract the stake weight for the session's orchestrator from r (initialized to a random stake weight)
	// If subtracting the stake weight for the current session from r results in a value <= 0, we select the current session
	// The greater the stake weight of a session, the more likely that it will be selected because subtracting its stake weight from r
	// will result in a value <= 0
	for i, sess := range s.unknownSessions {
		if sess.OrchestratorInfo.GetTicketParams() == nil {
			continue
		}

		addr := ethcommon.BytesToAddress(sess.OrchestratorInfo.TicketParams.Recipient)

		r -= sessionsWeights.Scores[addr] * sessionsWeights.TotalStake

		if r <= 0 {
			s.removeUnknownSession(i)
			clog.Infof(ctx, "Selected Orchestrator 0x%v score=%v", addr.Hex(), sessionsWeights.Scores[addr])
			return sess
		}
	}

	return nil
}

func (s *WeightedSelector) removeUnknownSession(i int) {
	n := len(s.unknownSessions)
	s.unknownSessions[n-1], s.unknownSessions[i] = s.unknownSessions[i], s.unknownSessions[n-1]
	s.unknownSessions = s.unknownSessions[:n-1]
}

// LIFOSelector selects the next BroadcastSession in LIFO order
// now used only in tests
type LIFOSelector []*BroadcastSession

// Add adds the sessions to the front of the selector's list
func (s *LIFOSelector) Add(sessions []*BroadcastSession) {
	*s = append(sessions, *s...)
}

// Complete adds the session to the end of the selector's list
func (s *LIFOSelector) Complete(sess *BroadcastSession) {
	*s = append(*s, sess)
}

// Select returns the last session in the selector's list
func (s *LIFOSelector) Select(ctx context.Context) *BroadcastSession {
	sessList := *s
	last := len(sessList) - 1
	if last < 0 {
		return nil
	}
	sess, sessions := sessList[last], sessList[:last]
	*s = sessions
	return sess
}

// Size returns the number of sessions stored by the selector
func (s *LIFOSelector) Size() int {
	return len(*s)
}

// Clear resets the selector's state
func (s *LIFOSelector) Clear() {
	*s = nil
}
