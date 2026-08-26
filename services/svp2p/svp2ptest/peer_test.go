package svp2ptest

import (
	"net"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// rawClient is the node side of a ScriptedPeer test: a bare TCP client that
// completes the handshake and then exchanges wire messages.
type rawClient struct {
	t    *testing.T
	conn net.Conn
	net  wire.BitcoinNet
}

func dialScripted(t *testing.T, p *ScriptedPeer) *rawClient {
	t.Helper()

	conn, err := net.Dial("tcp", p.Addr)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	c := &rawClient{t: t, conn: conn, net: p.Net}

	me := wire.NewNetAddressIPPort(net.ParseIP("127.0.0.1"), 0, 0)
	c.write(wire.NewMsgVersion(me, me, 1, 0))

	for {
		msg := c.read(5 * time.Second)
		if _, ok := msg.(*wire.MsgVerAck); ok {
			break
		}
	}

	return c
}

func (c *rawClient) write(msg wire.Message) {
	c.t.Helper()
	require.NoError(c.t, wire.WriteMessage(c.conn, msg, wire.ProtocolVersion, c.net))
}

func (c *rawClient) read(timeout time.Duration) wire.Message {
	c.t.Helper()
	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(timeout)))

	msg, _, err := wire.ReadMessage(c.conn, wire.ProtocolVersion, c.net)
	require.NoError(c.t, err)

	return msg
}

// readUntil returns the first message of the wanted command, answering pings
// and skipping anything else.
func (c *rawClient) readUntil(cmd string, timeout time.Duration) wire.Message {
	c.t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		msg := c.read(time.Until(deadline))
		if msg.Command() == cmd {
			return msg
		}
	}

	c.t.Fatalf("no %s within %s", cmd, timeout)

	return nil
}

func newTestPeer(t *testing.T, height int, script Script) (*ScriptedPeer, *FixtureChain) {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)
	chain := BuildFixtureChain(t, tSettings, height)
	peer := NewScriptedPeer(t, chain, tSettings.ChainCfgParams.Net, script, true)

	return peer, chain
}

func getHeadersFrom(locator chainhash.Hash) *wire.MsgGetHeaders {
	m := wire.NewMsgGetHeaders()
	m.ProtocolVersion = wire.ProtocolVersion
	_ = m.AddBlockLocatorHash(&locator)

	return m
}

func getDataFor(hash chainhash.Hash) *wire.MsgGetData {
	m := wire.NewMsgGetData()
	_ = m.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, &hash))

	return m
}

func TestScriptedPeer_HonestGetHeadersServesFromLocator(t *testing.T) {
	peer, chain := newTestPeer(t, 30, Script{})
	c := dialScripted(t, peer)

	genesis := chain.Headers[0].PrevBlock
	c.write(getHeadersFrom(genesis))

	headers := c.readUntil("headers", 5*time.Second).(*wire.MsgHeaders)
	require.Len(t, headers.Headers, 30)
	require.Equal(t, chain.Tip(), headers.Headers[29].BlockHash())

	require.Equal(t, 1, peer.Transcript.Count(In, "getheaders"))
	require.Equal(t, 1, peer.Transcript.Count(Out, "headers"))
}

func TestScriptedPeer_ScriptOverridesGetHeaders(t *testing.T) {
	var first *wire.MsgHeaders

	replay := Script{OnGetHeaders: func(p *ScriptedPeer, m *wire.MsgGetHeaders) []wire.Message {
		if first == nil {
			first = p.HeadersFor(m)
		}

		return []wire.Message{first}
	}}

	peer, chain := newTestPeer(t, 10, replay)
	c := dialScripted(t, peer)

	genesis := chain.Headers[0].PrevBlock
	c.write(getHeadersFrom(genesis))
	a := c.readUntil("headers", 5*time.Second).(*wire.MsgHeaders)

	c.write(getHeadersFrom(chain.Tip()))
	b := c.readUntil("headers", 5*time.Second).(*wire.MsgHeaders)

	require.Len(t, a.Headers, 10)
	require.Len(t, b.Headers, 10, "the script replays the first batch regardless of the locator")
	require.Equal(t, a.Headers[0].BlockHash(), b.Headers[0].BlockHash())
}

func TestScriptedPeer_WithholdBlocks(t *testing.T) {
	withhold := Script{OnGetData: func(*ScriptedPeer, *wire.MsgGetData) []wire.Message { return nil }}

	peer, chain := newTestPeer(t, 3, withhold)
	c := dialScripted(t, peer)

	hash := chain.Headers[0].BlockHash()
	c.write(getDataFor(hash))

	// Nothing comes back but pings/pongs; give it a moment and check the record.
	time.Sleep(300 * time.Millisecond)

	require.True(t, peer.Requested(hash))
	require.Equal(t, 0, peer.ServedBlocks())
	require.Equal(t, 0, peer.Transcript.Count(Out, "block"))
}

func TestScriptedPeer_WriteDelayThrottles(t *testing.T) {
	slow := Script{WriteDelay: func(msg wire.Message, _ int) time.Duration {
		if msg.Command() == "block" {
			return 50 * time.Millisecond
		}

		return 0
	}}

	peer, chain := newTestPeer(t, 5, slow)
	c := dialScripted(t, peer)

	m := wire.NewMsgGetData()
	for _, h := range chain.Headers {
		hash := h.BlockHash()
		require.NoError(t, m.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, &hash)))
	}

	start := time.Now()
	c.write(m)

	for i := 0; i < 5; i++ {
		c.readUntil("block", 5*time.Second)
	}

	require.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
	require.Equal(t, 5, peer.ServedBlocks())
}

func TestScriptedPeer_TranscriptRecordsWhoClosed(t *testing.T) {
	peer, _ := newTestPeer(t, 1, Script{})
	c := dialScripted(t, peer)

	require.NoError(t, c.conn.Close())

	require.Eventually(t, func() bool { return peer.Transcript.ClosedBy() == "node" }, 5*time.Second, 20*time.Millisecond)

	other, _ := newTestPeer(t, 1, Script{})
	_ = dialScripted(t, other)
	other.Close()

	require.Eventually(t, func() bool { return other.Transcript.ClosedBy() == "peer" }, 5*time.Second, 20*time.Millisecond)
}

func TestScriptedPeer_ServeLimitScript(t *testing.T) {
	peer, chain := newTestPeer(t, 4, ServeLimit(2))
	c := dialScripted(t, peer)

	m := wire.NewMsgGetData()
	for _, h := range chain.Headers {
		hash := h.BlockHash()
		require.NoError(t, m.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, &hash)))
	}

	c.write(m)
	c.readUntil("block", 5*time.Second)
	c.readUntil("block", 5*time.Second)

	time.Sleep(200 * time.Millisecond)
	require.Equal(t, 2, peer.ServedBlocks())
	require.Equal(t, 4, peer.RequestedCount())
}
