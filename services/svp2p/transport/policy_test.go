package transport

import (
	"testing"

	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

func TestStreamPolicy_Routing(t *testing.T) {
	bp, ok := PolicyForName(wire.BlockPriorityStreamPolicy)
	require.True(t, ok)
	require.Equal(t, wire.StreamTypeData1, bp.StreamFor(wire.NewMsgPing(1)))
	require.Equal(t, wire.StreamTypeData1, bp.StreamFor(wire.NewMsgPong(1)))
	require.Equal(t, wire.StreamTypeData1, bp.StreamFor(&wire.MsgBlock{}))
	require.Equal(t, wire.StreamTypeGeneral, bp.StreamFor(wire.NewMsgHeaders()), "SVNode routes headers on GENERAL; legacy's DATA1 routing is not carried")
	require.Equal(t, wire.StreamTypeGeneral, bp.StreamFor(wire.NewMsgInv()))
	require.True(t, bp.RequiresData1())

	def, ok := PolicyForName("Default")
	require.True(t, ok)
	require.Equal(t, wire.StreamTypeGeneral, def.StreamFor(&wire.MsgBlock{}))
	require.False(t, def.RequiresData1())

	_, ok = PolicyForName("Bogus")
	require.False(t, ok)
}

func TestPreferredPolicy(t *testing.T) {
	require.Equal(t, "BlockPriority", PreferredPolicy(OurPolicyPriority, []string{"Default", "BlockPriority"}).Name())
	require.Equal(t, "Default", PreferredPolicy(OurPolicyPriority, []string{"Default"}).Name())
	require.Equal(t, "Default", PreferredPolicy(OurPolicyPriority, nil).Name())
	require.Equal(t, "Default", PreferredPolicy([]string{"Default"}, []string{"BlockPriority", "Default"}).Name(), "our list decides the order")
}
