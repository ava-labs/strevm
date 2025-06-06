package proxytime

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

// Bytes returns a deterministic marshalling of the time.
func (tm *Time[D]) Bytes() []byte {
	m := timeMarshaler{
		seconds:  tm.seconds,
		fraction: uint64(tm.fraction),
		hertz:    uint64(tm.hertz),
	}
	return m.MarshalCanoto()
}

type timeMarshaler struct {
	seconds  uint64 `canoto:"uint,1"`
	fraction uint64 `canoto:"uint,2"`
	hertz    uint64 `canoto:"uint,3"`

	canotoData canotoData_timeMarshaler
}

// Parse is the inverse of [Time.Bytes].
func Parse[D Duration](b []byte) (*Time[D], error) {
	var m timeMarshaler
	if err := m.UnmarshalCanoto(b); err != nil {
		return nil, err
	}
	tm := New(m.seconds, D(m.hertz))
	tm.Tick(D(m.fraction))
	return tm, nil
}
