package transport

import (
	"github.com/bsv-blockchain/go-wire"
)

// StreamPolicy defines how messages are routed to streams
type StreamPolicy interface {
	Name() string
	StreamFor(msg wire.Message) wire.StreamType
	RequiresData1() bool
}

// OurPolicyPriority is our prioritised list of preferred stream policies (net.cpp:948-965)
var OurPolicyPriority = []string{wire.BlockPriorityStreamPolicy, wire.DefaultStreamPolicy}

// defaultStreamPolicy routes all messages to the GENERAL stream (stream_policy.cpp:98-103)
type defaultStreamPolicy struct{}

func (p *defaultStreamPolicy) Name() string {
	return wire.DefaultStreamPolicy
}

func (p *defaultStreamPolicy) StreamFor(msg wire.Message) wire.StreamType {
	return wire.StreamTypeGeneral
}

func (p *defaultStreamPolicy) RequiresData1() bool {
	return false
}

// blockPriorityStreamPolicy routes block/ping/pong to DATA1, everything else to GENERAL (stream_policy.cpp:187-195)
type blockPriorityStreamPolicy struct{}

func (p *blockPriorityStreamPolicy) Name() string {
	return wire.BlockPriorityStreamPolicy
}

func (p *blockPriorityStreamPolicy) StreamFor(msg wire.Message) wire.StreamType {
	cmd := msg.Command()
	switch cmd {
	case wire.CmdBlock, wire.CmdPing, wire.CmdPong:
		return wire.StreamTypeData1
	default:
		return wire.StreamTypeGeneral
	}
}

func (p *blockPriorityStreamPolicy) RequiresData1() bool {
	return true
}

// PolicyForName returns a StreamPolicy for the given name, or false if not found
func PolicyForName(name string) (StreamPolicy, bool) {
	switch name {
	case wire.DefaultStreamPolicy:
		return &defaultStreamPolicy{}, true
	case wire.BlockPriorityStreamPolicy:
		return &blockPriorityStreamPolicy{}, true
	default:
		return nil, false
	}
}

// PreferredPolicy returns the first policy from ours that is also in theirs (net.cpp:948-965)
// Falls back to Default if no common policies
func PreferredPolicy(ours []string, theirs []string) StreamPolicy {
	theirsMap := make(map[string]bool)
	for _, p := range theirs {
		theirsMap[p] = true
	}

	for _, p := range ours {
		if theirsMap[p] {
			policy, _ := PolicyForName(p)
			return policy
		}
	}

	// No common policy; fall back to Default (svp2p deviation: SVNode throws)
	policy, _ := PolicyForName(wire.DefaultStreamPolicy)
	return policy
}
