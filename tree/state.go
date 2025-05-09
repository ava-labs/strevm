package tree

import "maps"

type State struct {
	Parent *State // If Parent is nil, this is the settled state.

	Accounts  map[Address]Account
	Block     Block
	Timestamp ExecutionTimestamp
}

func (s *State) Settle() {
	if s.Parent == nil {
		return
	}

	s.Parent.Settle()
	maps.Copy(s.Parent.Accounts, s.Accounts)
	*s = *s.Parent
}

func (s *State) ApplyToParent() {
	maps.Copy(s.Parent.Accounts, s.Accounts)
	s.Parent.Block = s.Block
	s.Parent.Timestamp = s.Timestamp
}

func (s *State) NewChild() *State {
	return &State{
		Parent: s,

		Accounts:  make(map[Address]Account),
		Block:     s.Block,
		Timestamp: s.Timestamp,
	}
}

func (s *State) Account(a Address) Account {
	if acc, ok := s.Accounts[a]; ok {
		return acc
	}
	if s.Parent == nil {
		return Account{}
	}
	return s.Parent.Account(a)
}

type Account struct {
	Nonce   uint64
	Balance uint64
}
