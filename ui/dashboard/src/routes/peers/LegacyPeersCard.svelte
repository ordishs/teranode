<svelte:options runes={true} />

<script lang="ts">
  import Card from '$internal/components/card/index.svelte'
  import Typo from '$internal/components/typo/index.svelte'

  interface LegacyDetail {
    inbound: boolean
    protocol_version: number
    service_flags: number
    ping_micros: number
    time_offset_secs: number
    starting_height: number
    is_sync_peer: boolean
    time_connected: number
  }

  interface LegacyPeer {
    id: string
    client_name?: string
    height?: number
    network_address?: string
    is_connected?: boolean
    is_banned?: boolean
    bytes_sent?: number
    bytes_received?: number
    legacy?: LegacyDetail
  }

  let { peers = [] as LegacyPeer[] } = $props()

  const connectedCount = $derived(peers.filter((p: LegacyPeer) => p.is_connected).length)

  function formatBytes(value: number | undefined): string {
    if (!value) return '0 B'

    const units = ['B', 'KB', 'MB', 'GB', 'TB']
    let scaled = value
    let unit = 0

    while (scaled >= 1024 && unit < units.length - 1) {
      scaled /= 1024
      unit += 1
    }

    return `${scaled.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`
  }

  function formatPing(micros: number | undefined): string {
    if (!micros) return '-'

    return `${(micros / 1000).toFixed(1)} ms`
  }

  function formatOffset(seconds: number | undefined): string {
    if (!seconds) return '0s'

    return `${seconds > 0 ? '+' : ''}${seconds}s`
  }

  function formatServiceFlags(flags: number | undefined): string {
    if (!flags) return '-'

    return `0x${flags.toString(16)}`
  }

  function formatConnected(unixSeconds: number | undefined): string {
    if (!unixSeconds || unixSeconds <= 0) return '-'

    return new Date(unixSeconds * 1000).toLocaleString()
  }

  function peerAddress(peer: LegacyPeer): string {
    return peer.network_address || String(peer.id || '').replace(/^legacy:/, '')
  }
</script>

<Card contentPadding="16px">
  <div class="legacy-header">
    <Typo variant="title" size="h4" value="Legacy Peers (Bitcoin P2P)" />
    <span class="legacy-count">{connectedCount} connected / {peers.length} known</span>
  </div>

  {#if peers.length === 0}
    <div class="legacy-empty">
      <p>No legacy peers</p>
      <p class="sub">The legacy service is not connected to any Bitcoin P2P peer.</p>
    </div>
  {:else}
    <div class="legacy-scroll">
      <table class="legacy-table">
        <thead>
          <tr>
            <th>Address</th>
            <th>User Agent</th>
            <th>Dir</th>
            <th class="lp-num">Version</th>
            <th class="lp-num">Services</th>
            <th class="lp-num">Ping</th>
            <th class="lp-num">Offset</th>
            <th class="lp-num">Start Height</th>
            <th class="lp-num">Height</th>
            <th class="lp-num">Sent</th>
            <th class="lp-num">Received</th>
            <th>Connected</th>
          </tr>
        </thead>
        <tbody>
          {#each peers as peer (peer.id)}
            <tr class:disconnected={!peer.is_connected}>
              <td class="addr">
                {peerAddress(peer)}
                {#if peer.legacy?.is_sync_peer}
                  <span class="badge sync">SYNC</span>
                {/if}
                {#if peer.is_banned}
                  <span class="badge banned">BANNED</span>
                {/if}
              </td>
              <td class="agent" title={peer.client_name || ''}>{peer.client_name || '-'}</td>
              <td>{peer.legacy?.inbound ? 'in' : 'out'}</td>
              <td class="lp-num">{peer.legacy?.protocol_version || '-'}</td>
              <td class="lp-num">{formatServiceFlags(peer.legacy?.service_flags)}</td>
              <td class="lp-num">{formatPing(peer.legacy?.ping_micros)}</td>
              <td class="lp-num">{formatOffset(peer.legacy?.time_offset_secs)}</td>
              <td class="lp-num">{(peer.legacy?.starting_height || 0).toLocaleString()}</td>
              <td class="lp-num">{(peer.height || 0).toLocaleString()}</td>
              <td class="lp-num">{formatBytes(peer.bytes_sent)}</td>
              <td class="lp-num">{formatBytes(peer.bytes_received)}</td>
              <td>{formatConnected(peer.legacy?.time_connected)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</Card>

<style>
  .legacy-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 12px;
  }

  .legacy-count {
    font-size: 0.85rem;
    opacity: 0.7;
  }

  .legacy-empty {
    padding: 24px 0;
    text-align: center;
  }

  .legacy-empty .sub {
    font-size: 0.85rem;
    opacity: 0.6;
  }

  .legacy-scroll {
    overflow-x: auto;
  }

  .legacy-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.85rem;
  }

  .legacy-table th,
  .legacy-table td {
    padding: 6px 10px;
    text-align: left;
    white-space: nowrap;
    border-bottom: 1px solid rgba(128, 128, 128, 0.2);
  }

  .legacy-table th.lp-num,
  .legacy-table td.lp-num {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .legacy-table tr.disconnected {
    opacity: 0.45;
  }

  .legacy-table td.agent {
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .badge {
    margin-left: 6px;
    padding: 1px 5px;
    border-radius: 3px;
    font-size: 0.7rem;
    font-weight: 600;
  }

  .badge.sync {
    background: rgba(56, 142, 60, 0.2);
  }

  .badge.banned {
    background: rgba(198, 40, 40, 0.2);
  }
</style>
