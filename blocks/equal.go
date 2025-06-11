package blocks

// Equal performs a deep equality check, including block ancestry. It is
// therefore inefficient and MUST NOT be used outside of tests.
func (b *Block) Equal(c *Block) bool {
	if bn, cn := b == nil, c == nil; bn == true && cn == true {
		return true
	} else if bn != cn {
		return false
	}

	if b.Hash() != c.Hash() {
		return false
	}

	switch ba, ca := b.ancestry.Load(), c.ancestry.Load(); {
	case ba == nil && ca == nil:
	case ba == nil || ca == nil:
		return false
	case !ba.parent.Equal(ca.parent):
		return false
	case !ba.lastSettled.Equal(ca.lastSettled):
		return false
	}

	return b.execution.Load().Equal(c.execution.Load())
}
