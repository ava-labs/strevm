package tree

import (
	"maps"
	"slices"
)

type State struct {
	Accounts  map[Address]Account
	Block     Block
	Timestamp ExecutionTimestamp
}

type Account struct {
	Nonce   uint64
	Balance uint64
}

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
	Settled    State
	Executed   []State
	Accepted   []Block
	Processing map[ID]Block
}

func (t *Tree) LatestSettled() State {
	return t.Settled
}

func (t *Tree) LatestExecuted() State {
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

func (t *Tree) Settle(id ID) bool {
	var (
		toSettle []State
		found    bool
	)
	for _, s := range t.Executed {
		toSettle = append(toSettle, s)
		if s.Block.ID == id {
			found = true
			break
		}
	}
	if !found {
		return false
	}

	for _, s := range toSettle {
		maps.Copy(t.Settled.Accounts, s.Accounts)
		t.Settled.Block = s.Block
		t.Settled.Timestamp = s.Timestamp
	}
	t.Executed = t.Executed[len(toSettle):]
	return true
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
		func(a Address) Account {
			acc, found := t.GetExecutedAccountAt(a, prev.Block.ID)
			if !found {
				// Assumes the ID is either executed or settled.
				panic("couldn't find executed ID")
			}
			return acc
		},
		func(t Transaction) uint64 {
			return t.GasUsed
		},
	)

	t.Accepted = t.Accepted[1:]
	t.Executed = append(t.Executed, nextState)
	return true
}

func (t *Tree) GetExecutedAccountAt(a Address, id ID) (Account, bool) {
	var found bool
	for i := len(t.Executed) - 1; i >= 0; i++ {
		s := t.Executed[i]
		found = found || s.Block.ID == id
		if !found {
			continue
		}

		if a, ok := s.Accounts[a]; ok {
			return a, true
		}
	}
	return t.Settled.Accounts[a], !found && t.Settled.Block.ID != id
}

func gasTargetToGasPerSecond(gasTarget uint64) uint64 {
	return 2 * gasTarget // From ACP-176
}

func (t *Tree) GetWorstCaseState(a Address, settledID ID, id ID) (State, bool) {
	toExecute, ok := t.getQueue(settledID, id)
	if !ok {
		return State{}, false
	}

	prev, ok := t.getState(settledID)
	if !ok {
		return State{}, false
	}

	accounts := make(map[Address]Account)
	for _, next := range toExecute {
		prev = execute(
			next,
			prev,
			func(a Address) Account {
				if acc, ok := accounts[a]; ok {
					return acc
				}
				acc, found := t.GetExecutedAccountAt(a, settledID)
				if !found {
					// Assumes the ID is either executed or settled.
					panic("couldn't find executed ID")
				}
				return acc
			},
			func(t Transaction) uint64 {
				return t.GasLimit
			},
		)
		maps.Copy(accounts, prev.Accounts)
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

func (t *Tree) getState(id ID) (State, bool) {
	if t.Settled.Block.ID == id {
		return t.Settled, true
	}
	for _, s := range t.Executed {
		if s.Block.ID == id {
			return s, true
		}
	}
	return State{}, false
}

// func (t *Tree) Verify(b Block) bool {
// 	return false
// }

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

func execute(
	next Block,
	prev State,
	getAccount func(Address) Account,
	executeTx func(Transaction) uint64,
) State {
	// TODO: Doesn't handle fixed delay correctly
	timestamp := prev.Timestamp
	timestamp.Max(ExecutionTimestamp{
		Timestamp: next.Timestamp,
	})

	// TODO: Calculate the gas price
	const gasPrice = 1

	// Execute the block
	var (
		accounts             = make(map[Address]Account)
		getOverriddenAccount = func(a Address) Account {
			if acc, ok := accounts[a]; ok {
				return acc
			}
			return getAccount(a)
		}

		blockGasUsed uint64
	)

	for _, tx := range next.Txs {
		txGasUsed := executeTx(tx)

		from := getOverriddenAccount(tx.From)
		from.Balance -= txGasUsed * gasPrice
		from.Balance -= tx.Amount
		from.Nonce++
		accounts[tx.From] = from

		to := getOverriddenAccount(tx.To)
		to.Balance += tx.Amount
		accounts[tx.To] = to

		blockGasUsed += txGasUsed
	}

	currentGasPerSecond := gasTargetToGasPerSecond(prev.Block.GasTargetPerSecond)
	timestamp.Add(blockGasUsed, currentGasPerSecond)

	// Fix timestamp if needed
	// TODO: This really isn't handled correctly, by including gas in the
	// timestamp it inherently assumes there is a constant relationship between
	// gas and time. But the relationship is dynamic.
	nextGasPerSecond := gasTargetToGasPerSecond(next.GasTargetPerSecond)
	if timestamp.AdditionalGasExecuted > nextGasPerSecond {
		timestamp.Timestamp++
		timestamp.AdditionalGasExecuted = 0
	}

	return State{
		Accounts:  accounts,
		Block:     next,
		Timestamp: timestamp,
	}
}
