package gastime

import (
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/strevm/proxytime"
)

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

// timeMarshaler marshals and unmarshals canoto buffers that represent [Time]
// objects.
//
// TODO(arr4n) remove this when the embedded [proxytime.Time] field can have a
// canoto tag.
type timeMarshaler struct {
	proxy  *proxytime.Time[gas.Gas] `canoto:"pointer,1"`
	target gas.Gas                  `canoto:"uint,2"`
	excess gas.Gas                  `canoto:"uint,3"`

	canotoData canotoData_timeMarshaler `canoto:"nocopy"`
}

// Bytes marshals the time as bytes.
func (tm *Time) Bytes() []byte {
	m := timeMarshaler{
		proxy:  tm.Time,
		target: tm.target,
		excess: tm.excess,
	}
	return m.MarshalCanoto()
}

// FromBytes is the inverse of [Time.Bytes].
func FromBytes(b []byte) (*Time, error) {
	m := new(timeMarshaler)
	if err := m.UnmarshalCanoto(b); err != nil {
		return nil, err
	}
	return makeTime(m.proxy, m.target, m.excess), nil
}
