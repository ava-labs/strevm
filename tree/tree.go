package tree

import (
	"slices"
)

const settlementDelay = 5

type Block struct {
	ID                 ID
	ParentID           ID
	Timestamp          uint64 // Seconds
	GasTargetPerSecond uint64

	Txs []Transaction
}

type Transaction struct {
	From   Address
	Nonce  uint64
	To     Address
	Amount uint64

	GasUsed  uint64 // Only known after execution
	GasLimit uint64
}

type ID [32]byte

type Address [20]byte

type Tree struct {
	Settled    *State
	Executed   []*State
	Accepted   []Block
	Processing map[ID]Block
}

func (t *Tree) LatestSettled() *State {
	return t.Settled
}

func (t *Tree) LatestExecuted() *State {
	if e, ok := last(t.Executed); ok {
		return e
	}
	return t.LatestSettled()
}

func (t *Tree) LatestAccepted() Block {
	if a, ok := last(t.Accepted); ok {
		return a
	}
	return t.LatestExecuted().Block
}

func (t *Tree) getState(id ID) (*State, bool) {
	if t.Settled.Block.ID == id {
		return t.Settled, true
	}
	for _, s := range t.Executed {
		if s.Block.ID == id {
			return s, true
		}
	}
	return nil, false
}

func (t *Tree) ExecuteNext() bool {
	if len(t.Accepted) == 0 {
		return false // Nothing to execute
	}

	next := t.Accepted[0]
	prev := t.LatestExecuted()

	nextState := execute(
		next,
		prev,
		func(tx Transaction, s *State, gasPrice, gasPerSecond uint64) {
			from := s.Account(tx.From)
			from.Balance -= tx.GasUsed * gasPrice
			from.Balance -= tx.Amount
			from.Nonce++
			s.Accounts[tx.From] = from

			to := s.Account(tx.To)
			to.Balance += tx.Amount
			s.Accounts[tx.To] = to

			s.Timestamp.Add(tx.GasUsed, gasPerSecond)
		},
	)

	t.Accepted = t.Accepted[1:]
	t.Executed = append(t.Executed, nextState)
	return true
}

func gasTargetToGasPerSecond(gasTarget uint64) uint64 {
	return 2 * gasTarget // From ACP-176
}

func (t *Tree) GetWorstCaseState(settledID ID, id ID) (*State, bool) {
	toExecute, ok := t.getQueue(settledID, id)
	if !ok {
		return nil, false
	}

	prev, ok := t.getState(settledID)
	if !ok {
		return nil, false
	}

	prev = prev.NewChild()
	for _, next := range toExecute {
		prev = execute(
			next,
			prev,
			func(tx Transaction, s *State, gasPrice, gasPerSecond uint64) {
				from := s.Account(tx.From)
				from.Balance -= tx.GasLimit * gasPrice
				from.Balance -= tx.Amount
				from.Nonce++
				s.Accounts[tx.From] = from

				s.Timestamp.Add(tx.GasLimit, gasPerSecond)
			},
		)
		prev.ApplyToParent()
		prev = prev.Parent
	}

	return prev, true
}

func (t *Tree) getQueue(start ID, end ID) ([]Block, bool) {
	if start == end {
		return nil, true
	}

	var queue []Block
	defer func() {
		slices.Reverse(queue)
	}()

	for {
		b, ok := t.Processing[end]
		if !ok {
			break
		}

		queue = append(queue, b)
		end = b.ParentID
		if start == end {
			return queue, true
		}
	}
	for i := len(t.Accepted) - 1; i >= 0; i++ {
		b := t.Accepted[i]
		if b.ID != end {
			continue
		}

		queue = append(queue, b)
		end = b.ParentID
		if start == end {
			return queue, true
		}
	}
	for i := len(t.Executed) - 1; i >= 0; i++ {
		b := t.Executed[i].Block
		if b.ID != end {
			continue
		}

		queue = append(queue, b)
		end = b.ParentID
		if start == end {
			return queue, true
		}
	}

	return nil, false
}

func (t *Tree) settle(id ID) bool {
	for i, s := range t.Executed {
		if s.Block.ID != id {
			continue
		}

		s.Settle()
		t.Settled = s
		t.Executed = t.Executed[i+1:]
		return true
	}
	return false
}

func (t *Tree) getIDToSettle(parentID ID, timestamp uint64) (ID, bool) {
	executed := t.Settled
	for _, e := range t.Executed {
		if e.Timestamp.Timestamp+settlementDelay < timestamp {
			// This state was finished executing at least settlementDelay ago
			executed = e
		} else {
			// This state hasn't finished waiting for the settlementDelay yet,
			// so the previous execution result is the most recent one that can
			// be settled.
			return executed.Block.ID, true
		}
	}

	// The last executed block is settle-able by this block, so we need to check
	// if the next block to execute can be settle-able. If it is possible for
	// the next block to be settle-able, we must wait for its execution results.

	var nextBlockToExecute Block
	if len(t.Accepted) > 0 {
		nextBlockToExecute = t.Accepted[0]
	} else {
		var ok bool
		nextBlockToExecute, ok = t.Processing[parentID]
		if !ok {
			// The parentID is one of the executed blocks.
			return executed.Block.ID, true
		}
		for {
			newB, ok := t.Processing[nextBlockToExecute.ParentID]
			if !ok {
				break
			}
			nextBlockToExecute = newB
		}
	}

	// This aligns with the start of block processing. It should act as if the
	// minimum amount of gas was executed.
	minimumExecutionTimestamp := executed.Timestamp
	minimumExecutionTimestamp.Max(ExecutionTimestamp{
		Timestamp: nextBlockToExecute.Timestamp,
	})
	// Can choose to apply the minimum gas charged rule here to further advance
	// this time. (We should do this)
	//
	// We could also introspect into block execution if the next block to
	// execute is accepted (and is therefore currently being executed) to even
	// further advance this time. (We should probably leave this as a future
	// improvement)
	if minimumExecutionTimestamp.Timestamp+settlementDelay < timestamp {
		return ID{}, false
	}
	// The minimum possible timestamp that this block will be settle-able at
	// is after the provided timestamp. So, we can settle the previously
	// found block without issue.
	return executed.Block.ID, true
}

func (t *Tree) Verify(b Block) bool {
	toSettle, ok := t.getIDToSettle(b.ParentID, b.Timestamp)
	if !ok {
		// We currently don't know what blocks are settle-able for this block,
		// so we consider it invalid.
		return false
	}

	worstCaseState, ok := t.GetWorstCaseState(toSettle, b.ParentID)
	if !ok {
		// The parentID isn't processing.
		return false
	}
	// Try to generate the worst state after executing this block
	_ = worstCaseState
	return true
}

func (t *Tree) Accept(id ID) bool {
	b, ok := t.Processing[id]
	if !ok {
		return false
	}

	lastAccepted := t.LatestAccepted()
	if lastAccepted.ID != b.ParentID {
		return false
	}

	t.Accepted = append(t.Accepted, b)
	delete(t.Processing, id)
	return true
}

func (t *Tree) Reject(id ID) bool {
	_, ok := t.Processing[id]
	delete(t.Processing, id)
	return ok
}

// execute the provided block on top of the previous state and return a new
// state.
func execute(
	next Block,
	prev *State,
	executeTx func(
		Transaction,
		*State,
		uint64,
		uint64,
	),
) *State {
	// TODO: Calculate the gas price
	const gasPrice = 1

	// Execute the block
	var (
		diff = &State{
			Parent: prev,

			Accounts:  make(map[Address]Account),
			Block:     next,
			Timestamp: prev.Timestamp,
		}
		currentGasPerSecond = gasTargetToGasPerSecond(prev.Block.GasTargetPerSecond)
	)

	diff.Timestamp.Max(ExecutionTimestamp{
		Timestamp: next.Timestamp,
	})

	for _, tx := range next.Txs {
		executeTx(
			tx,
			diff,
			gasPrice,
			currentGasPerSecond,
		)
	}

	// Fix timestamp if needed
	// TODO: This really isn't handled correctly, by including gas in the
	// timestamp it inherently assumes there is a constant relationship between
	// gas and time. But the relationship is dynamic.
	nextGasPerSecond := gasTargetToGasPerSecond(next.GasTargetPerSecond)
	if diff.Timestamp.AdditionalGasExecuted > nextGasPerSecond {
		diff.Timestamp.Timestamp++
		diff.Timestamp.AdditionalGasExecuted = 0
	}

	return diff
}
