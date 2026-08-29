package protocol

import (
	"github.com/bsv-blockchain/go-wire"
)

// SendCmpct dispatches NetMsgType::SENDCMPCT (net_processing.cpp
// ProcessSendCompactMessage:2417-2437) to the sending peer's sync state. It
// is the compactDispatcher half of Task 6; see that interface's own doc
// comment for why it is wired independent of SyncEnabled().
func (m *PeerManager) SendCmpct(sp *SyncPeer, msg *wire.MsgSendcmpct) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	sp.State.recordSendCmpct(msg)
}
