package protocol

type Version string

const Current Version = "runstead.protocol.v1"

func (v Version) String() string {
	return string(v)
}
