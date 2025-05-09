package tree

type ExecutionTimestamp struct {
	// Timestamp in seconds from the unix timestamp.
	Timestamp uint64
	// AdditionalGasExecuted after advancing the unix timestamp.
	// Must be less than the gas executed per second.
	AdditionalGasExecuted uint64
}

// Max sets t to the maximum timestamp of t and o.
func (t *ExecutionTimestamp) Max(o ExecutionTimestamp) {
	switch {
	case t.Timestamp < o.Timestamp:
		*t = o
	case t.Timestamp == o.Timestamp:
		t.AdditionalGasExecuted = max(t.AdditionalGasExecuted, o.AdditionalGasExecuted)
	}
}

// Sub returns the amount of gas that occurred between time time t and o. If o
// is more advanced than t, then false is returned.
func (t *ExecutionTimestamp) Sub(o ExecutionTimestamp, gasPerSecond uint64) (uint64, bool) {
	if o.Timestamp > t.Timestamp ||
		(o.Timestamp == t.Timestamp && o.AdditionalGasExecuted > t.AdditionalGasExecuted) {
		return 0, false
	}
	return (t.Timestamp-o.Timestamp)*gasPerSecond + t.AdditionalGasExecuted - o.AdditionalGasExecuted, true
}

// Add advances the timestamp by gas with gasPerSecond.
func (t *ExecutionTimestamp) Add(gas uint64, gasPerSecond uint64) {
	t.AdditionalGasExecuted += gas
	t.Timestamp += t.AdditionalGasExecuted / gasPerSecond
	t.AdditionalGasExecuted %= gasPerSecond
}
